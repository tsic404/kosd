package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/tsic404/kosd/pkg/controlplane"
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

	status, body := call(t, address, http.MethodGet, "/api/v1/resources/Application", "")
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
