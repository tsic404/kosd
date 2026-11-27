package controlplane

import (
	"context"
	"errors"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/pkg/reference"
)

// referenceTarget is one kernel object a submitted extension object names at a
// declared reference path (C-19).
type referenceTarget struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
}

// applicationOf returns the target's Application identity when it names one.
func (t referenceTarget) applicationOf(referrerNamespace string) (ObjectRef, bool) {
	if t.Group != api.Group || t.Kind != "Application" || t.Name == "" {
		return ObjectRef{}, false
	}
	namespace := t.Namespace
	if namespace == "" {
		namespace = referrerNamespace
	}
	return ObjectRef{Group: api.Group, Kind: "Application", Namespace: namespace, Name: t.Name}, true
}

// declaredReferences reads the references one object carries at the paths its
// class declares (C-19). The shape is the one the reference gate reads: an
// object reference is apiVersion (or group), kind, namespace and name, and a
// path may hold one reference or a list of them.
func declaredReferences(object *unstructured.Unstructured, paths []string) []referenceTarget {
	targets := make([]referenceTarget, 0, len(paths))
	for _, path := range paths {
		value, found, err := unstructured.NestedFieldNoCopy(object.Object, strings.Split(path, ".")...)
		if err != nil || !found {
			continue
		}
		targets = append(targets, decodeReferenceValue(value)...)
	}
	return targets
}

// decodeReferenceValue reads one reference value or a list of them.
func decodeReferenceValue(value any) []referenceTarget {
	switch typed := value.(type) {
	case map[string]any:
		if parsed, ok := decodeReference(typed); ok {
			return []referenceTarget{parsed}
		}
	case []any:
		targets := make([]referenceTarget, 0, len(typed))
		for _, item := range typed {
			targets = append(targets, decodeReferenceValue(item)...)
		}
		return targets
	}
	return nil
}

// decodeReference reads one object reference.
func decodeReference(value map[string]any) (referenceTarget, bool) {
	target := referenceTarget{}
	target.Kind, _ = value["kind"].(string)
	target.Name, _ = value["name"].(string)
	if target.Kind == "" || target.Name == "" {
		return referenceTarget{}, false
	}
	target.Group, _ = value["group"].(string)
	if target.Group == "" {
		if apiVersion, ok := value["apiVersion"].(string); ok {
			if prefix, _, found := strings.Cut(apiVersion, "/"); found {
				target.Group = prefix
			}
		}
	}
	target.Namespace, _ = value["namespace"].(string)
	return target, true
}

// referenceApplications lists the Applications one desired object names at its
// class's declared reference paths (C-19), sorted and without repeats.
func (s *Server) referenceApplications(desired *unstructured.Unstructured) []ObjectRef {
	if s.extensions == nil || desired == nil {
		return nil
	}
	class, ok := s.extensions.ObjectClass(desired.GroupVersionKind().Group, desired.GetKind())
	if !ok || len(class.References) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	applications := make([]ObjectRef, 0, len(class.References))
	for _, target := range declaredReferences(desired, class.References) {
		application, isApplication := target.applicationOf(desired.GetNamespace())
		if !isApplication || seen[objectRefKey(application)] {
			continue
		}
		seen[objectRefKey(application)] = true
		applications = append(applications, application)
	}
	sort.Slice(applications, func(left, right int) bool {
		return objectRefKey(applications[left]) < objectRefKey(applications[right])
	})
	return applications
}

// guardReferences refuses a write that would add a declarative reference to an
// Application whose removal already started (C-19). A reference already stored
// on the object is not new: it keeps blocking the removal as before. The caller
// must hold the reference boundary, so the removal cannot be released between
// this read and the write it judges.
func (s *Server) guardReferences(ctx context.Context, desired, stored *unstructured.Unstructured) *Error {
	if s.extensions == nil || desired == nil {
		return nil
	}
	class, ok := s.extensions.ObjectClass(desired.GroupVersionKind().Group, desired.GetKind())
	if !ok || len(class.References) == 0 {
		return nil
	}
	known := storedReferenceKeys(stored, class.References)
	for _, target := range declaredReferences(desired, class.References) {
		application, isApplication := target.applicationOf(desired.GetNamespace())
		if !isApplication || known[objectRefKey(application)] {
			continue
		}
		observed, failure := s.readKernelObject(ctx, application)
		if failure != nil {
			return failure
		}
		if observed == nil {
			// A reference to an object that does not exist yet names nothing to
			// protect; the write is judged on its own, as before.
			continue
		}
		if observed.GetDeletionTimestamp() != nil {
			return refusedReference(application)
		}
	}
	return nil
}

// writeReferenced runs one write of an extension object inside the exclusion
// the uninstall gate release also takes (C-19). The guard read and the write
// happen while the release is excluded, so a stored reference is either seen by
// the release's scan or refused before it is written; no compensating write is
// needed.
func (s *Server) writeReferenced(ctx context.Context, object desiredObject, stored *unstructured.Unstructured, write func(context.Context) *Error) *Error {
	applications := s.referenceApplications(object.Object)
	if len(applications) == 0 {
		return write(ctx)
	}
	keys := make([]string, 0, len(applications))
	for _, application := range applications {
		keys = append(keys, reference.Key(application.Group, application.Kind, application.Namespace, application.Name))
	}
	return s.underBoundary(ctx, object.Ref, keys, func(ctx context.Context) *Error {
		if failure := s.guardReferences(ctx, object.Object, stored); failure != nil {
			return failure
		}
		return write(ctx)
	})
}

// underBoundary runs decide while holding every named boundary record. The
// records are taken in the caller's order, so two writes naming the same
// Applications cannot deadlock.
func (s *Server) underBoundary(ctx context.Context, ref ObjectRef, keys []string, decide func(context.Context) *Error) *Error {
	var failure *Error
	var hold func(int) error
	hold = func(index int) error {
		if index == len(keys) {
			failure = decide(ctx)
			return nil
		}
		return s.boundary.Do(ctx, keys[index], func(ctx context.Context) error {
			return hold(index + 1)
		})
	}
	if err := hold(0); err != nil {
		if errors.Is(err, reference.ErrBusy) {
			return (&Error{
				Code:        CodeConflict,
				Message:     "对象 " + ref.String() + " 的引用边界正被其他写入占用，请稍后重试",
				Stage:       StageApply,
				Retryable:   true,
				SideEffects: SideEffectsNone,
			}).about(ref)
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return internal(StageApply, "等待引用边界超时，请稍后重试").about(ref)
		}
		s.log.Error("reference boundary failed", "object", ref.String(), "error", err)
		return internal(StageApply, "引用边界暂不可用，请稍后重试").about(ref)
	}
	return failure
}

// readKernelObject reads one kernel object through the live client, so the
// guard never decides on a projection that lags behind the cluster. A missing
// object is not an error.
func (s *Server) readKernelObject(ctx context.Context, ref ObjectRef) (*unstructured.Unstructured, *Error) {
	objectType, known := kernelType(ref.Kind)
	if !known {
		return nil, nil
	}
	object, err := resourceOf(s.dynamic, objectType, ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, s.readFailure(err, ref)
	}
	return object, nil
}

// storedReferenceKeys lists the Application references an object already
// carries, so the guard judges only the edges the write adds.
func storedReferenceKeys(stored *unstructured.Unstructured, paths []string) map[string]bool {
	keys := make(map[string]bool)
	if stored == nil {
		return keys
	}
	for _, target := range declaredReferences(stored, paths) {
		if application, isApplication := target.applicationOf(stored.GetNamespace()); isApplication {
			keys[objectRefKey(application)] = true
		}
	}
	return keys
}

// objectRefKey is the stable key of one kernel object reference.
func objectRefKey(ref ObjectRef) string {
	return ref.Group + "/" + ref.Kind + "/" + ref.Namespace + "/" + ref.Name
}

// refusedReference is the answer of a write that would add a reference to an
// Application being removed (C-19).
func refusedReference(application ObjectRef) *Error {
	failure := &Error{
		Code:               CodeReferenceRefused,
		Message:            "应用 " + application.String() + " 已被请求删除，不能再写入指向它的引用",
		Stage:              StageValidate,
		RequiresUserAction: true,
		SideEffects:        SideEffectsNone,
	}
	return failure.about(application)
}
