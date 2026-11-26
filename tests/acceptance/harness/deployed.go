package harness

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"

	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/identity"
)

// The first Owner the installation creates before the process serves. A real
// installation hands out the one-time password; the case replaces it here the
// way the operator's first sign-in does, so the deployed-process case writes
// with an ordinary session (KOSD-C-54, KOSD-C-63).
const (
	OwnerName     = "owner"
	OwnerPassword = "owner-password"
)

// DeployedProcess is the control-plane binary started the way the cluster's
// Deployment manifest starts it: the manifest's container args are the process
// arguments, and only the paths and connection facts a test environment owns
// are overridden (KOSD-C-03, ADR-011).
type DeployedProcess struct {
	// URL is the process's HTTP address.
	URL string
	// PackagesPath is the package directory the process reads artifacts from.
	PackagesPath string
	// DatabasePath is the control-plane database the process owns.
	DatabasePath string
	// HTTP calls the running process.
	HTTP *HTTP

	logs    *bytes.Buffer
	command *exec.Cmd
	done    chan error
}

// StartDeployedProcess builds cmd/kos-control-plane and starts it over the
// acceptance cluster. It fails the case when the process cannot start, and
// prints the process log when the case fails.
func (c *Cluster) StartDeployedProcess(t *testing.T) *DeployedProcess {
	t.Helper()
	workspace := t.TempDir()
	binary := filepath.Join(workspace, "kos-control-plane")
	build := exec.Command("go", "build", "-o", binary, "./cmd/kos-control-plane")
	build.Dir = RepoRoot(t)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the control plane: %v\n%s", err, output)
	}

	kubeconfig := writeKubeconfig(t, c, workspace)
	packages := filepath.Join(workspace, "packages")
	if err := os.MkdirAll(packages, 0o755); err != nil {
		t.Fatalf("create the package directory: %v", err)
	}
	address := freeAddress(t)
	database := filepath.Join(workspace, "kos.db")
	seedOwner(t, database)
	args := append(deploymentArgs(t),
		"-kubeconfig", kubeconfig,
		"-database", database,
		"-packages", packages,
		"-backup-root", filepath.Join(workspace, "backup"),
		"-address", address,
	)

	logs := &bytes.Buffer{}
	command := exec.Command(binary, args...)
	command.Stdout = logs
	command.Stderr = logs
	if err := command.Start(); err != nil {
		t.Fatalf("start the control plane: %v", err)
	}
	process := &DeployedProcess{
		URL:          "http://" + address,
		PackagesPath: packages,
		DatabasePath: database,
		HTTP:         &HTTP{URL: "http://" + address},
		logs:         logs,
		command:      command,
		done:         make(chan error, 1),
	}
	go func() { process.done <- command.Wait() }()
	t.Cleanup(func() {
		process.stop(t)
		if t.Failed() {
			t.Logf("control-plane process log:\n%s", logs.String())
		}
	})
	Eventually(t, 60*time.Second, "the control plane to answer its health probe", func() bool {
		response, err := http.Get(process.URL + "/api/v1/system/health")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode < 500
	})
	return process
}

// seedOwner creates the first Owner in the database the process will open, so
// the case can sign in over the deployed process's own account surface. The
// installation is what normally writes this row (KOSD-C-54, KOSD-C-63).
func seedOwner(t *testing.T, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := identity.Open(ctx, identity.OpenOptions{Path: path})
	if err != nil {
		t.Fatalf("open the control-plane database: %v", err)
	}
	defer store.Close()
	service := identity.New(store, identity.Config{})
	bootstrap, err := service.BootstrapOwner(ctx, OwnerName)
	if err != nil {
		t.Fatalf("create the first Owner: %v", err)
	}
	if !bootstrap.Created || bootstrap.OneTimePassword == "" {
		t.Fatalf("bootstrap = %+v, want a created Owner with a one-time password", bootstrap)
	}
	login, err := service.Login(ctx, OwnerName, bootstrap.OneTimePassword)
	if err != nil {
		t.Fatalf("sign in as the first Owner: %v", err)
	}
	if err := service.ChangePassword(ctx, login.Token, bootstrap.OneTimePassword, OwnerPassword); err != nil {
		t.Fatalf("replace the one-time password: %v", err)
	}
}

// SignIn signs in over the deployed process's own account surface and returns
// the bearer token the case writes and reads with (KOSD-C-63).
func (p *DeployedProcess) SignIn(t *testing.T) string {
	t.Helper()
	status, body := p.HTTP.Call(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": OwnerName, "password": OwnerPassword,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("sign in as %s: status %d, body %v", OwnerName, status, body)
	}
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatalf("sign in as %s returned no token: %v", OwnerName, body)
	}
	return token
}

// Call sends one JSON request to the deployed process with the session token.
func (p *DeployedProcess) Call(t *testing.T, method, path string, body any, token string) (int, map[string]any) {
	t.Helper()
	headers := map[string]string{}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	return p.HTTP.Call(t, method, path, body, headers)
}

// DeliverPackage writes one packed extension package into the directory the
// process reads artifacts from and returns the reference and digest a component
// names (KOSD-C-17).
func (p *DeployedProcess) DeliverPackage(t *testing.T, name string, declaration extension.Declaration) (string, string) {
	t.Helper()
	content, digest, err := extension.Pack(declaration, nil)
	if err != nil {
		t.Fatalf("pack extension %s: %v", name, err)
	}
	reference := name + ".tar.gz"
	if err := os.WriteFile(filepath.Join(p.PackagesPath, reference), content, 0o644); err != nil {
		t.Fatalf("write package %s: %v", name, err)
	}
	return reference, digest
}

// stop asks the process to shut down, and terminates it when it does not.
func (p *DeployedProcess) stop(t *testing.T) {
	t.Helper()
	if p.command.Process == nil {
		return
	}
	_ = p.command.Process.Signal(os.Interrupt)
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		_ = p.command.Process.Kill()
		<-p.done
	}
}

// deploymentArgs reads the container arguments of the control-plane Deployment
// from the manifests the cluster applies.
func deploymentArgs(t *testing.T) []string {
	t.Helper()
	path := filepath.Join(RepoRoot(t), "config", "deploy", "kos-control-plane.yaml")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, document := range strings.Split(string(content), "\n---") {
		var object map[string]any
		if err := yaml.Unmarshal([]byte(document), &object); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if object["kind"] != "Deployment" {
			continue
		}
		args, found := containerArgs(object)
		if !found {
			t.Fatalf("%s carries no control-plane container args", path)
		}
		return args
	}
	t.Fatalf("%s carries no Deployment", path)
	return nil
}

// containerArgs extracts the first container's args from a Deployment manifest.
func containerArgs(object map[string]any) ([]string, bool) {
	raw := value(object, "spec", "template", "spec", "containers")
	containers, ok := raw.([]any)
	if !ok || len(containers) == 0 {
		return nil, false
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		return nil, false
	}
	raw, ok = container["args"]
	if !ok {
		return []string{}, true
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	args := make([]string, 0, len(list))
	for _, item := range list {
		text, ok := item.(string)
		if !ok {
			return nil, false
		}
		args = append(args, text)
	}
	return args, true
}

// value walks a decoded manifest along a chain of map keys.
func value(object map[string]any, path ...string) any {
	var current any = object
	for _, key := range path {
		parent, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = parent[key]
	}
	return current
}

// writeKubeconfig renders the acceptance cluster's admin configuration as the
// kubeconfig file the process reads, so the case exercises the same client
// construction the cluster does.
func writeKubeconfig(t *testing.T, cluster *Cluster, directory string) string {
	t.Helper()
	if cluster.Config == nil {
		t.Fatal("the acceptance cluster carries no admin configuration")
	}
	apiConfig := clientcmdapi.NewConfig()
	apiConfig.Clusters["acceptance"] = &clientcmdapi.Cluster{
		Server:                   cluster.Config.Host,
		CertificateAuthorityData: cluster.Config.CAData,
		InsecureSkipTLSVerify:    cluster.Config.Insecure,
	}
	apiConfig.AuthInfos["acceptance"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cluster.Config.CertData,
		ClientKeyData:         cluster.Config.KeyData,
		Token:                 cluster.Config.BearerToken,
	}
	apiConfig.Contexts["acceptance"] = &clientcmdapi.Context{
		Cluster:  "acceptance",
		AuthInfo: "acceptance",
	}
	apiConfig.CurrentContext = "acceptance"
	path := filepath.Join(directory, "kubeconfig")
	if err := clientcmd.WriteToFile(*apiConfig, path); err != nil {
		t.Fatalf("write the kubeconfig: %v", err)
	}
	return path
}

// freeAddress reserves a loopback port and returns it, so the process binds an
// address the case can reach without a fixed port colliding with a parallel
// package.
func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve an address: %v", err)
	}
	address := listener.Addr().String()
	listener.Close()
	return address
}
