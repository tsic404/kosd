// Package terminal implements the control plane's execution responsibility
// (ADR-043): a Pod session is an exec channel into the container the session
// names, and the credential boundary judge refuses a target whose isolation
// cannot be guaranteed.
package terminal

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/install"
)

// outputGrace bounds how long one batch waits for the target to produce output.
// A receipt says the batch was received, not that a command finished
// (KOSD-C-92), so this is a delivery window rather than a completion wait.
const outputGrace = 25 * time.Millisecond

// closeGrace bounds the wait for a channel to end once its input is closed.
const closeGrace = 2 * time.Second

// Kubernetes executes Pod sessions through the runtime's exec subresource. It
// never runs a node shell: a node session's root shell can read the control
// plane's own credentials, which ADR-043 makes a boundary failure.
type Kubernetes struct {
	// Client reaches the runtime API.
	Client kubernetes.Interface
	// Config is the cluster connection the exec stream dials.
	Config *rest.Config
}

// Available implements controlplane.TerminalExecutor.
func (k *Kubernetes) Available(context.Context) *controlplane.Error { return nil }

// Open implements controlplane.TerminalExecutor. Only a Pod target has a
// container to exec into; a node target names an execution identity this
// executor cannot isolate.
func (k *Kubernetes) Open(ctx context.Context, target controlplane.TerminalTarget, _ string) (controlplane.TerminalChannel, error) {
	if k.Client == nil || k.Config == nil || target.Type != controlplane.TerminalTargetTypePod {
		return nil, controlplane.ErrTerminalExecutorUnavailable
	}
	request := k.Client.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(target.Namespace).
		Name(target.ID).
		SubResource("exec")
	request.VersionedParams(&corev1.PodExecOptions{
		Container: target.Container,
		Command:   []string{"/bin/sh"},
		Stdin:     true,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(k.Config, http.MethodPost, request.URL())
	if err != nil {
		return nil, errors.Join(controlplane.ErrTerminalExecutorUnavailable, err)
	}
	stdinReader, stdinWriter := io.Pipe()
	output := &lockedBuffer{}
	done := make(chan error, 1)
	streamContext, cancel := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		done <- executor.StreamWithContext(streamContext, remotecommand.StreamOptions{
			Stdin:  stdinReader,
			Stdout: output,
			Stderr: output,
		})
	}()
	return &kubernetesChannel{stdin: stdinWriter, output: output, done: done, cancel: cancel}, nil
}

// kubernetesChannel is one exec stream. Input is written to the container's
// stdin; output is collected between batches and never stored.
type kubernetesChannel struct {
	stdin  *io.PipeWriter
	output *lockedBuffer
	done   chan error
	cancel context.CancelFunc
	once   sync.Once
}

// Send implements controlplane.TerminalChannel.
func (c *kubernetesChannel) Send(ctx context.Context, _ uint64, data []byte) (controlplane.TerminalReceipt, error) {
	if err := c.finished(); err != nil {
		return controlplane.TerminalReceipt{}, err
	}
	c.output.reset()
	if _, err := c.stdin.Write(data); err != nil {
		return controlplane.TerminalReceipt{}, classify(err)
	}
	select {
	case <-ctx.Done():
		return controlplane.TerminalReceipt{Result: controlplane.TerminalReceiptAccepted, Output: c.output.snapshot()}, nil
	case <-time.After(outputGrace):
	}
	return controlplane.TerminalReceipt{
		Result: controlplane.TerminalReceiptAccepted,
		Output: c.output.snapshot(),
	}, nil
}

// Close implements controlplane.TerminalChannel. The stream ending is the
// channel's end; a target that was already gone is reported as ended.
func (c *kubernetesChannel) Close(context.Context) (controlplane.TerminalReceipt, error) {
	defer c.cancel()
	c.once.Do(func() { _ = c.stdin.Close() })
	select {
	case err := <-c.done:
		if err != nil && !errors.Is(err, io.EOF) && !isExit(err) {
			return controlplane.TerminalReceipt{Result: controlplane.TerminalReceiptUnknown}, classify(err)
		}
		return controlplane.TerminalReceipt{Result: controlplane.TerminalReceiptAccepted}, nil
	case <-time.After(closeGrace):
		return controlplane.TerminalReceipt{Result: controlplane.TerminalReceiptUnknown}, nil
	}
}

// finished reports the stream's error once it ended.
func (c *kubernetesChannel) finished() error {
	select {
	case err := <-c.done:
		c.done <- err
		if err == nil {
			return controlplane.ErrTerminalTargetEnded
		}
		return classify(err)
	default:
		return nil
	}
}

// classify maps an exec failure to the executor vocabulary (KOSD-C-93).
func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err), errors.Is(err, io.EOF):
		return errors.Join(controlplane.ErrTerminalTargetEnded, err)
	default:
		return errors.Join(controlplane.ErrTerminalExecutorUnavailable, err)
	}
}

// isExit reports a non-zero shell exit, which ends the channel but is not an
// executor failure.
func isExit(err error) bool {
	var exit interface{ ExitStatus() int }
	return errors.As(err, &exit)
}

// lockedBuffer collects output between two batches.
type lockedBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, data...)
	return len(data), nil
}

func (b *lockedBuffer) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = b.buf[:0]
}

func (b *lockedBuffer) snapshot() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf...)
}

// Boundary is the ADR-043 credential-isolation judge. A node shell is
// root@node and a container shell runs as the Pod's container; either can read
// values KOSD-C-14 keeps away from the browser. The judge refuses a node until
// the deployment declares its isolation, and refuses a Pod whose container can
// read the control plane's credential, an application sensitive parameter, or
// host files that carry either.
type Boundary struct {
	// NodeIsolated asserts that root@node cannot read the control plane's
	// credentials or application sensitive parameters.
	NodeIsolated bool
}

// Judge implements controlplane.CredentialBoundary.
func (b Boundary) Judge(_ context.Context, observed controlplane.TerminalBoundaryTarget) *controlplane.Error {
	if observed.Target.Type == controlplane.TerminalTargetTypeNode {
		if !b.NodeIsolated {
			return boundaryRefusal("节点执行身份无法与控制面凭据隔离，已拒绝连接")
		}
		return nil
	}
	if observed.Sensitive {
		return boundaryRefusal("应用敏感参数可被容器读取，已拒绝连接")
	}
	if podExposesProtectedValues(observed.Pod, observed.Target.Container) {
		return boundaryRefusal("容器可读取控制面凭据或受保护的存储值，已拒绝连接")
	}
	return nil
}

// boundaryRefusal is the one KOSD-C-93 answer for a target whose isolation
// cannot be guaranteed.
func boundaryRefusal(message string) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeTerminalCredentialBoundaryUnavailable,
		Message:     message,
		Stage:       controlplane.StageConnect,
		SideEffects: controlplane.SideEffectsNone,
	}
}

// podExposesProtectedValues reports whether the selected container can read
// the control plane's credential, a service account token, or a Secret that may
// carry one (ADR-043, KOSD-C-14, KOSD-F-9). An application sensitive parameter
// is judged from the owning application's definition before this check, so a
// plain ConfigMap mount is not itself a refusal. A fact the judge cannot read
// is a refusal, never an assumption of isolation.
func podExposesProtectedValues(pod map[string]any, container string) bool {
	if len(pod) == 0 {
		return true
	}
	if nestedString(pod, "metadata", "labels", "app") == install.ControlPlaneName ||
		nestedString(pod, "spec", "serviceAccountName") == install.ControlPlaneServiceAccount {
		return true
	}
	spec, _ := pod["spec"].(map[string]any)
	if spec == nil {
		return true
	}
	if truthy(spec["hostNetwork"]) || truthy(spec["hostPID"]) || truthy(spec["hostIPC"]) {
		return true
	}
	volumes := map[string]map[string]any{}
	for _, volume := range mapsOf(spec["volumes"]) {
		if name := stringOf(volume["name"]); name != "" {
			volumes[name] = volume
		}
	}
	for _, declared := range mapsOf(spec["containers"]) {
		if stringOf(declared["name"]) != container {
			continue
		}
		if security, ok := declared["securityContext"].(map[string]any); ok && truthy(security["privileged"]) {
			return true
		}
		for _, env := range mapsOf(declared["env"]) {
			if stringOf(env["name"]) == "KUBECONFIG" && strings.HasPrefix(stringOf(env["value"]), "/etc/kos/managed") {
				return true
			}
			if valueFrom, ok := env["valueFrom"].(map[string]any); ok {
				if valueFrom["secretKeyRef"] != nil {
					return true
				}
			}
		}
		for _, from := range mapsOf(declared["envFrom"]) {
			if from["secretRef"] != nil {
				return true
			}
		}
		for _, mount := range mapsOf(declared["volumeMounts"]) {
			volume := volumes[stringOf(mount["name"])]
			if volume == nil {
				continue
			}
			if volume["hostPath"] != nil || volume["secret"] != nil {
				return true
			}
			if projected, ok := volume["projected"].(map[string]any); ok {
				for _, source := range mapsOf(projected["sources"]) {
					// A projected service account token is the default
					// kube-api-access volume shape, so it cannot be filtered
					// by the control plane's own ServiceAccount name.
					if source["secret"] != nil || source["serviceAccountToken"] != nil {
						return true
					}
				}
			}
		}
		return false
	}
	return true
}

// nestedString reads one nested string field; a missing field is empty.
func nestedString(object map[string]any, path ...string) string {
	current := any(object)
	for _, key := range path {
		nested, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = nested[key]
	}
	return stringOf(current)
}

// stringOf reads a string value; any other type is empty.
func stringOf(value any) string {
	text, _ := value.(string)
	return text
}

// truthy reports a boolean field's value; a non-boolean is false.
func truthy(value any) bool {
	flag, _ := value.(bool)
	return flag
}

// mapsOf reads a list of objects; a non-list is empty.
func mapsOf(value any) []map[string]any {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	objects := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if object, ok := item.(map[string]any); ok {
			objects = append(objects, object)
		}
	}
	return objects
}
