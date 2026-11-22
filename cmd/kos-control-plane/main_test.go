package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/identity"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

// TestProcessServesProbeAndGuardsTheAPI starts the built binary against a real
// API server and checks the two things a deployment depends on: the health
// probe answers before any session exists (KOSD-C-56, KOSD-C-57) and every
// other route still requires one (KOSD-C-63).
func TestProcessServesProbeAndGuardsTheAPI(t *testing.T) {
	cluster := harness.Start(t)
	binary := filepath.Join(t.TempDir(), "kos-control-plane")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the process: %v\n%s", err, output)
	}
	address := freeAddress(t)
	database := filepath.Join(t.TempDir(), "kos.db")
	command := exec.Command(binary,
		"-kubeconfig", writeKubeconfig(t, cluster.Config),
		"-database", database,
		"-address", address,
	)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatalf("start the process: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	t.Cleanup(func() {
		if command.ProcessState == nil || !command.ProcessState.Exited() {
			_ = command.Process.Kill()
			<-exited
		}
	})

	health := waitForHealth(t, address, &output)
	if status, _ := health["status"].(string); status != "ok" {
		t.Fatalf("health = %v, want a measurable protection reporting ok", health)
	}

	// The page write refusal sits ahead of the account gate, so a write method
	// on a page read route answers before any session exists (KOSD-C-89).
	status, body := call(t, address, http.MethodPost, "/api/v1/pages", "")
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("a page write = %d %v, want 405", status, body)
	}
	if code := errorCode(body); code != controlplane.CodePageReadOnly {
		t.Fatalf("the page refusal carries %q, want %q", code, controlplane.CodePageReadOnly)
	}

	status, body = call(t, address, http.MethodGet, "/api/v1/resources/Application", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated resource read = %d %v, want 401", status, body)
	}
	if code := errorCode(body); code != controlplane.CodeUnauthenticated {
		t.Fatalf("the refusal carries %q, want %q", code, controlplane.CodeUnauthenticated)
	}

	// The account surface is mounted in front of the domain API: a wrong
	// password reaches the login handler and is refused there (KOSD-C-63).
	status, body = call(t, address, http.MethodPost, "/api/v1/auth/login", `{"username":"owner","password":"wrong"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("a failed login = %d %v, want 401", status, body)
	}
	if code := errorCode(body); code != controlplane.CodeLoginFailed {
		t.Fatalf("the failed login carries %q, want %q", code, controlplane.CodeLoginFailed)
	}

	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal the process: %v", err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("the process stopped with %v\n%s", err, output.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the process did not stop after SIGTERM\n%s", output.String())
	}
}

// errorCode reads the stable code out of the failure envelope (KOSD-C-12).
func errorCode(body map[string]any) string {
	failure, _ := body["error"].(map[string]any)
	code, _ := failure["code"].(string)
	return code
}

// waitForHealth polls the probe until the process answers, failing the case
// with the process output when it never does.
func waitForHealth(t *testing.T, address string, output *bytes.Buffer) map[string]any {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		status, body := call(t, address, http.MethodGet, "/api/v1/system/health", "")
		if status == http.StatusOK {
			return body
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the probe never answered\n%s", output.String())
	return nil
}

// call sends one request and decodes the JSON answer.
func call(t *testing.T, address, method, path, payload string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if payload != "" {
		reader = strings.NewReader(payload)
	}
	request, err := http.NewRequest(method, "http://"+address+path, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if payload != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, map[string]any{}
	}
	defer response.Body.Close()
	decoded := map[string]any{}
	content, _ := io.ReadAll(response.Body)
	if err := json.Unmarshal(content, &decoded); err != nil {
		t.Fatalf("%s %s answered %q: %v", method, path, content, err)
	}
	return response.StatusCode, decoded
}

// freeAddress reserves one loopback port and releases it for the process.
func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a listen address: %v", err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

// writeKubeconfig renders the acceptance cluster's credentials as a file the
// process reads like a deployment reads its mounted kubeconfig.
func writeKubeconfig(t *testing.T, config *rest.Config) string {
	t.Helper()
	cluster := clientcmdapi.NewCluster()
	cluster.Server = config.Host
	cluster.CertificateAuthorityData = config.CAData
	cluster.InsecureSkipTLSVerify = config.Insecure
	user := clientcmdapi.NewAuthInfo()
	user.ClientCertificateData = config.CertData
	user.ClientKeyData = config.KeyData
	user.Token = config.BearerToken
	context := clientcmdapi.NewContext()
	context.Cluster = "acceptance"
	context.AuthInfo = "acceptance"
	kubeconfig := clientcmdapi.NewConfig()
	kubeconfig.Clusters["acceptance"] = cluster
	kubeconfig.AuthInfos["acceptance"] = user
	kubeconfig.Contexts["acceptance"] = context
	kubeconfig.CurrentContext = "acceptance"
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*kubeconfig, path); err != nil {
		t.Fatalf("write the kubeconfig: %v", err)
	}
	return path
}

// processPackage is the extension package the process test delivers: the
// kos-taskmanager pages and window application the product packs (KOSD-AC-72).
func processPackage(t *testing.T) extension.Declaration {
	t.Helper()
	return harness.TaskmanagerDeclaration(t)
}

// TestProcessInstallsPackagesFromItsDirectory checks the deployed process shape:
// it reads the package from the directory it was started with, verifies the
// digest over those bytes, loads the declared page and application, and refuses
// a package whose digest does not match with a readable reason (KOSD-C-17,
// KOSD-AC-72).
func TestProcessInstallsPackagesFromItsDirectory(t *testing.T) {
	cluster := harness.Start(t)
	binary := filepath.Join(t.TempDir(), "kos-control-plane")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the process: %v\n%s", err, output)
	}

	packages := t.TempDir()
	// The product's window application names its icon and document inside the
	// package, so the artifact carries those two asset paths (KOSD-C-73).
	assets := []extension.Asset{
		{Path: "assets/index.html", Content: []byte("<!doctype html><title>运行视图</title>")},
		{Path: "assets/icon.svg", Content: []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"></svg>`)},
	}
	content, digest, err := extension.Pack(processPackage(t), assets)
	if err != nil {
		t.Fatalf("pack the system application: %v", err)
	}
	reference := "dist/kos-taskmanager-0.1.0.tar.gz"
	target := filepath.Join(packages, filepath.FromSlash(reference))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("make the package directory: %v", err)
	}
	if err := os.WriteFile(target, content, 0o644); err != nil {
		t.Fatalf("deliver the package: %v", err)
	}

	database := filepath.Join(t.TempDir(), "kos.db")
	password := seedFirstOwner(t, database)
	address := freeAddress(t)
	command := exec.Command(binary,
		"-kubeconfig", writeKubeconfig(t, cluster.Config),
		"-database", database,
		"-address", address,
		"-packages", packages,
	)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatalf("start the process: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	t.Cleanup(func() {
		if command.ProcessState == nil || !command.ProcessState.Exited() {
			_ = command.Process.Kill()
			<-exited
		}
	})
	waitForHealth(t, address, &output)

	token := signInOwner(t, address, password)
	cluster.Create(t, &unstructured.Unstructured{Object: harness.Component(
		"kos-taskmanager", reference, digest)})
	cluster.Create(t, &unstructured.Unstructured{Object: harness.Component(
		"kos-taskmanager-bad", reference, "sha256:"+strings.Repeat("0", 64))})

	installed := waitForPhase(t, cluster, "kos-taskmanager", extension.PhaseReady, 60*time.Second, &output)
	if condition := conditionOf(t, installed, extension.ConditionPackageVerified); condition.Status != metav1.ConditionTrue {
		t.Fatalf("PackageVerified = %s, want True", condition.Status)
	}

	refused := waitForPhase(t, cluster, "kos-taskmanager-bad", extension.PhaseUnavailable, 60*time.Second, &output)
	condition := conditionOf(t, refused, extension.ConditionPackageVerified)
	if condition.Reason != extension.ReasonDigestMismatch {
		t.Fatalf("refused reason = %q, want %q", condition.Reason, extension.ReasonDigestMismatch)
	}
	if !strings.Contains(condition.Message, digest) || !strings.Contains(condition.Message, strings.Repeat("0", 64)) {
		t.Fatalf("refused message = %q, want the carried and the named digest", condition.Message)
	}

	pages := callWithToken(t, address, http.MethodGet, "/api/v1/pages", token)
	if !containsItemID(t, pages, "id", "overview") {
		t.Fatalf("pages = %v, want the package's page", pages)
	}
	overview := callWithToken(t, address, http.MethodGet, "/api/v1/pages/overview", token)
	items := harness.Items(t, overview)
	if len(items) != 7 {
		t.Fatalf("overview has %d items, want the seven the package declares", len(items))
	}
	origin := itemByID(t, overview, "maintenance-origin")
	if harness.String(origin, "source") != extension.PageSourceDatabase {
		t.Fatalf("maintenance origin source = %q, want %q", harness.String(origin, "source"), extension.PageSourceDatabase)
	}
	originValue, ok := origin["value"].(map[string]any)
	if !ok {
		t.Fatalf("maintenance origin value = %v, want an object", origin["value"])
	}
	if nodes, ok := originValue["nodes"].([]any); !ok || len(nodes) != 0 {
		t.Fatalf("maintenance origin = %v, want no row while every node is schedulable", originValue)
	}
	nodesReady := itemByID(t, overview, "nodes-ready")
	readyValue, ok := nodesReady["value"].(map[string]any)
	if !ok {
		t.Fatalf("nodes ready value = %v, want an object", nodesReady["value"])
	}
	if readyValue["total"] != float64(0) || readyValue["ready"] != float64(0) {
		t.Fatalf("nodes ready = %v, want no node in the fresh cluster", readyValue)
	}
	apps := callWithToken(t, address, http.MethodGet, "/api/v1/apps", token)
	if !appWithDigest(t, apps, "kos-taskmanager", digest) {
		t.Fatalf("applications = %v, want the package's window application", apps)
	}
}

// seedFirstOwner creates the first Owner in a fresh control-plane database and
// replaces its one-time password, the state an installation leaves behind.
func seedFirstOwner(t *testing.T, database string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := identity.Open(ctx, identity.OpenOptions{Path: database})
	if err != nil {
		t.Fatalf("open the identity database: %v", err)
	}
	defer func() { _ = store.Close() }()
	bootstrap, err := identity.New(store, identity.Config{}).BootstrapOwner(ctx, "owner")
	if err != nil {
		t.Fatalf("create the first Owner: %v", err)
	}
	service := identity.New(store, identity.Config{})
	login, err := service.Login(ctx, "owner", bootstrap.OneTimePassword)
	if err != nil {
		t.Fatalf("sign in with the one-time password: %v", err)
	}
	if err := service.ChangePassword(ctx, login.Token, bootstrap.OneTimePassword, "owner-password"); err != nil {
		t.Fatalf("replace the one-time password: %v", err)
	}
	return "owner-password"
}

// signInOwner returns a session token that may read the control-plane API
// (C-63).
func signInOwner(t *testing.T, address, password string) string {
	t.Helper()
	status, body := call(t, address, http.MethodPost, "/api/v1/auth/login", jsonBody(t, map[string]any{
		"username": "owner", "password": password,
	}))
	if status != http.StatusOK {
		t.Fatalf("sign in = %d %v", status, body)
	}
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatalf("sign in returned no token: %v", body)
	}
	return token
}

// waitForPhase polls one component until it reports the expected phase.
func waitForPhase(t *testing.T, cluster *harness.Cluster, name, phase string, timeout time.Duration, output *bytes.Buffer) *unstructured.Unstructured {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		object := cluster.Get(t, "v1beta1", "SystemComponent", name)
		reported, _, err := unstructured.NestedString(object.Object, "status", "phase")
		if err != nil {
			t.Fatalf("read phase of %s: %v", name, err)
		}
		if reported == phase {
			return object
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("component %s never reached %s\n%s", name, phase, output.String())
	return nil
}

// conditionOf returns the condition of one type.
func conditionOf(t *testing.T, object *unstructured.Unstructured, conditionType string) metav1.Condition {
	t.Helper()
	for _, condition := range harness.Conditions(t, object) {
		if condition.Type == conditionType {
			return condition
		}
	}
	t.Fatalf("%s carries no %s condition", object.GetName(), conditionType)
	return metav1.Condition{}
}

// callWithToken reads one API answer as a signed-in session.
func callWithToken(t *testing.T, address, method, path, token string) map[string]any {
	t.Helper()
	status, body := callWithTokenBody(t, address, method, path, token, "")
	if status != http.StatusOK {
		t.Fatalf("%s %s = %d %v", method, path, status, body)
	}
	return body
}

// callWithTokenBody sends one request as a session and decodes the answer.
func callWithTokenBody(t *testing.T, address, method, path, token, payload string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if payload != "" {
		reader = strings.NewReader(payload)
	}
	request, err := http.NewRequest(method, "http://"+address+path, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if payload != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	decoded := map[string]any{}
	content, _ := io.ReadAll(response.Body)
	if len(content) > 0 {
		if err := json.Unmarshal(content, &decoded); err != nil {
			t.Fatalf("%s %s answered %q: %v", method, path, content, err)
		}
	}
	return response.StatusCode, decoded
}

// jsonBody encodes one request body.
func jsonBody(t *testing.T, value map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode the request body: %v", err)
	}
	return string(encoded)
}

// containsItemID reports whether a list answer carries an item with the value.
func containsItemID(t *testing.T, body map[string]any, field, value string) bool {
	t.Helper()
	for _, item := range harness.Items(t, body) {
		if harness.String(item, field) == value {
			return true
		}
	}
	return false
}

// itemByID reads one item off a rendered page.
func itemByID(t *testing.T, page map[string]any, id string) map[string]any {
	t.Helper()
	for _, item := range harness.Items(t, page) {
		if harness.String(item, "id") == id {
			return item
		}
	}
	t.Fatalf("page has no item %q", id)
	return nil
}

// appWithDigest reports whether the application list carries one application
// with the given identifier and verified digest.
func appWithDigest(t *testing.T, body map[string]any, id, digest string) bool {
	t.Helper()
	for _, item := range harness.Items(t, body) {
		manifest, _ := item["manifest"].(map[string]any)
		if harness.String(manifest, "id") == id && harness.String(item, "digest") == digest {
			return true
		}
	}
	return false
}
