package controlplane

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kosd/api"
)

// terminalNodeType and terminalPodType are the two runtime classes a terminal
// target names (KOSD-C-95).
func terminalNodeType() ObjectType {
	return ObjectType{Group: "", Version: "v1", Kind: "Node", Resource: "nodes"}
}

func terminalPodType() ObjectType {
	return ObjectType{Group: "", Version: "v1", Kind: "Pod", Resource: "pods", Namespaced: true}
}

// terminalUnavailable is the one answer an unauthorized caller and an absent
// target share (KOSD-C-93). It names no object and carries no target field.
func terminalUnavailable() *Error {
	return terminalFailure(CodeTerminalTargetUnavailable, StageAuthorize, "目标不存在或当前身份不可访问")
}

// terminalSourceUnavailable reports a cluster read that cannot answer, so an
// absent target is never guessed (KOSD-C-93, KOSD-C-95).
func (s *Server) terminalSourceUnavailable(message string, err error) *Error {
	s.log.Error("terminal target read failed", "error", err)
	return terminalFailure(CodeTerminalTargetSourceUnavailable, StageRead, message)
}

// resolveTerminalTarget reads the live object behind one target and verifies
// its immutable identity (KOSD-C-95). A same-name replacement carries a new
// UID and is refused, so a session never inherits another object's authority.
func (s *Server) resolveTerminalTarget(ctx context.Context, target TerminalTarget) (terminalResolvedTarget, *Error) {
	resolved := terminalResolvedTarget{target: target}
	switch target.Type {
	case TerminalTargetTypeNode:
		observed, err := s.source.Get(ctx, terminalNodeType(), "", target.ID)
		switch {
		case apierrors.IsNotFound(err):
			return terminalResolvedTarget{}, terminalUnavailable()
		case err != nil:
			return terminalResolvedTarget{}, s.terminalSourceUnavailable("节点目标来源不可用，无法判断目标", err)
		}
		if nestedText(observed.Object, "metadata", "uid") != target.UID {
			return terminalResolvedTarget{}, terminalUnavailable()
		}
		resolved.node = observed.Object
		resolved.nodeReady = nodeReady(observed.Object)
		resolved.ended = isDeleting(observed.Object)
		return resolved, nil
	case TerminalTargetTypePod:
		observed, err := s.source.Get(ctx, terminalPodType(), target.Namespace, target.ID)
		switch {
		case apierrors.IsNotFound(err):
			return terminalResolvedTarget{}, terminalUnavailable()
		case err != nil:
			return terminalResolvedTarget{}, s.terminalSourceUnavailable("Pod 目标来源不可用，无法判断目标", err)
		}
		if nestedText(observed.Object, "metadata", "uid") != target.UID {
			return terminalResolvedTarget{}, terminalUnavailable()
		}
		if !podHasContainer(observed.Object, target.Container) {
			return terminalResolvedTarget{}, terminalUnavailable()
		}
		resolved.pod = observed.Object
		resolved.ended = podEnded(observed.Object)
		application, failure := s.applicationOwningPod(ctx, observed.Object)
		if failure != nil {
			return terminalResolvedTarget{}, failure
		}
		resolved.application = application
		if application != nil {
			resolved.applicationRef = ObjectRef{
				Group:     api.Group,
				Kind:      "Application",
				Namespace: nestedText(application, "metadata", "namespace"),
				Name:      nestedText(application, "metadata", "name"),
			}
			// A definition that cannot be read leaves the sensitive set
			// unknown, so the boundary must refuse rather than assume the
			// container is clean (ADR-043, KOSD-C-14).
			keys, known, failure := s.sensitiveKeys(ctx, nestedText(application, "spec", "definitionRef"))
			resolved.sensitive = failure != nil || !known || len(keys) > 0
		}
		return resolved, nil
	default:
		return terminalResolvedTarget{}, invalidField("目标类型只能是 node 或 pod")
	}
}

// applicationOwningPod finds the Application whose rendered workload selects
// this Pod (KOSD-C-76). A Pod no application owns has no application entry,
// which KOSD-C-93 leaves to the global Owner.
func (s *Server) applicationOwningPod(ctx context.Context, pod map[string]any) (map[string]any, *Error) {
	applicationType, failure := resolveType("Application", "", s.extensions)
	if failure != nil {
		return nil, failure
	}
	applications, err := s.source.List(ctx, applicationType, "")
	if err != nil {
		return nil, s.terminalSourceUnavailable("应用来源不可用，无法判断 Pod 归属", err)
	}
	namespace := nestedText(pod, "metadata", "namespace")
	name := nestedText(pod, "metadata", "name")
	for _, observed := range applications {
		if applicationNamespace(observed.Object) != namespace {
			continue
		}
		pods, err := s.applicationPods(ctx, observed.Object)
		if err != nil {
			return nil, s.terminalSourceUnavailable("运行内核来源不可用，无法判断 Pod 归属", err)
		}
		for _, candidate := range pods {
			if candidate.Namespace == namespace && candidate.Name == name {
				return observed.Object, nil
			}
		}
	}
	return nil, nil
}

// authorizeTerminalTarget judges one resolved target by the account's existing
// permission (KOSD-C-93): a node is the global Owner's alone, an application's
// Pod follows the application write rule, and a Pod no application owns is the
// Owner's. Every refusal is the unified target-unavailable answer.
func (s *Server) authorizeTerminalTarget(ctx context.Context, console TerminalSession, resolved terminalResolvedTarget) *Error {
	if resolved.target.Type == TerminalTargetTypeNode {
		if console.Role != "Owner" {
			return terminalUnavailable()
		}
		return nil
	}
	if resolved.application == nil {
		if console.Role != "Owner" {
			return terminalUnavailable()
		}
		return nil
	}
	if s.terminal.config.Console == nil {
		return terminalUnavailable()
	}
	sessionCtx := s.terminal.config.Console.WithSession(ctx, console)
	if _, failure := s.authorize(sessionCtx, VerbUpdate, resolved.applicationRef, nil, resolved.application); failure != nil {
		return terminalUnavailable()
	}
	return nil
}

// judgeTerminalBoundary refuses a target whose execution identity cannot be
// isolated from the control plane's credentials or application sensitive
// parameters (ADR-043). A missing judge is a refusal: the surface never
// assumes an isolation it cannot test.
func (s *Server) judgeTerminalBoundary(ctx context.Context, resolved terminalResolvedTarget) *Error {
	if s.terminal.config.Boundary == nil {
		return terminalFailure(CodeTerminalCredentialBoundaryUnavailable, StageConnect, "无法保证凭据隔离，已拒绝连接")
	}
	return s.terminal.config.Boundary.Judge(ctx, TerminalBoundaryTarget{
		Target:    resolved.target,
		Pod:       resolved.pod,
		Sensitive: resolved.sensitive,
	})
}

// nodeReady reports whether the Node's Ready condition holds.
func nodeReady(node map[string]any) bool {
	conditions, _, _ := unstructured.NestedSlice(node, "status", "conditions")
	for _, entry := range conditions {
		condition, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if text(condition["type"]) == "Ready" && text(condition["status"]) == "True" {
			return true
		}
	}
	return false
}

// podEnded reports whether the Pod already reached a terminal phase or is
// being deleted (KOSD-C-93).
func podEnded(pod map[string]any) bool {
	if isDeleting(pod) {
		return true
	}
	switch nestedText(pod, "status", "phase") {
	case "Succeeded", "Failed":
		return true
	}
	return false
}

// podHasContainer reports whether the Pod declares the selected container.
func podHasContainer(pod map[string]any, container string) bool {
	containers, _, _ := unstructured.NestedSlice(pod, "spec", "containers")
	for _, entry := range containers {
		declared, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if text(declared["name"]) == container {
			return true
		}
	}
	return false
}

// isDeleting reports whether the object carries a deletion timestamp.
func isDeleting(object map[string]any) bool {
	_, found, _ := unstructured.NestedFieldNoCopy(object, "metadata", "deletionTimestamp")
	return found
}
