package controlplane

import (
	"context"
	"net/http"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/tsic404/kosd/pkg/app"
)

// PodSourceType is the C-53 source type of a Pod projection item: the item is
// a cluster object, so its judgement is the object's field values (C-76).
const PodSourceType = "object"

// PodContainer is one container of a projected Pod: the name the workload
// declared and the image it runs (C-76).
type PodContainer struct {
	// Name is the container name.
	Name string `json:"name"`
	// Image is the image reference the container runs.
	Image string `json:"image"`
}

// PodProjection is one item of an application's Pod projection (C-76). It
// carries only the listed facts; the runtime API's other fields and objects
// stay behind the projection.
type PodProjection struct {
	// Name is the Pod's identity.
	Name string `json:"name"`
	// Node is the node the Pod runs on.
	Node string `json:"node,omitempty"`
	// Phase is the Pod's runtime phase.
	Phase string `json:"phase,omitempty"`
	// Ready tells whether the Pod's Ready condition holds.
	Ready bool `json:"ready"`
	// Restarts is the restart count summed over the Pod's containers.
	Restarts int64 `json:"restarts"`
	// Containers are the Pod's containers in spec order.
	Containers []PodContainer `json:"containers"`
	// StartedAt is when the Pod started; nil before the runtime reports one.
	StartedAt *time.Time `json:"startedAt,omitempty"`
	// SourceType is C-53's source type of this item.
	SourceType string `json:"sourceType"`
	// ObservedAt is when the source observed this Pod (C-13).
	ObservedAt time.Time `json:"observedAt"`
	// Stale tells whether ObservedAt is older than StaleThreshold at read
	// time (C-13).
	Stale bool `json:"stale"`
}

// podListResponse is one application's Pod projection.
type podListResponse struct {
	// Application is the application the projection belongs to.
	Application ObjectRef `json:"application"`
	// Items are the application's Pods; an application without Pods, including
	// a stopped one, answers with an empty list (C-76).
	Items []PodProjection `json:"items"`
}

// handleApplicationPods reads one application's Pod projection (C-76): a
// read-only view of the Pods its own workloads own, with C-59 visibility. A
// stopped application projects no Pods, and a runtime the control plane cannot
// read is a failure with a reason rather than an empty projection.
func (s *Server) handleApplicationPods(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	applicationType, failure := resolveType("Application", "", s.extensions)
	if failure != nil {
		writeError(w, failure)
		return
	}
	ref := ObjectRef{Group: applicationType.Group, Kind: applicationType.Kind, Namespace: namespace, Name: name}
	observed, err := s.source.Get(r.Context(), applicationType, namespace, name)
	if err != nil {
		writeError(w, s.clusterReadFailure(err, ref))
		return
	}
	if _, failure := s.authorize(r.Context(), VerbRead, ref, observed.Object, nil); failure != nil {
		writeError(w, failure)
		return
	}
	now := s.now()
	items := make([]PodProjection, 0)
	if !applicationPaused(observed.Object) {
		pods, err := s.applicationPods(r.Context(), observed.Object)
		if err != nil {
			writeError(w, s.clusterReadFailure(err, ref))
			return
		}
		for _, pod := range pods {
			items = append(items, projectPod(pod, now))
		}
	}
	writeJSON(w, http.StatusOK, podListResponse{Application: ref, Items: items})
}

// handlePodDelete deletes exactly the one Pod the request names (C-77). The
// target must be one of the application's own Pods, the application's write
// role judges the request, and a target that is already gone is accepted so a
// repeated request has no second effect. The expected state is untouched: no
// application spec write happens here, and the replacement instance is the
// runtime's own decision.
func (s *Server) handlePodDelete(w http.ResponseWriter, r *http.Request) {
	if s.refuseWrite(w) {
		return
	}
	namespace, name, podName := r.PathValue("namespace"), r.PathValue("name"), r.PathValue("pod")
	if !RecordableName(podName) {
		writeError(w, invalidField("Pod 名字不合法"))
		return
	}
	applicationType, failure := resolveType("Application", "", s.extensions)
	if failure != nil {
		writeError(w, failure)
		return
	}
	appRef := ObjectRef{Group: applicationType.Group, Kind: applicationType.Kind, Namespace: namespace, Name: name}
	ctx, cancel := context.WithTimeout(s.operations, s.timeout)
	defer cancel()
	stored, err := s.source.Get(ctx, applicationType, namespace, name)
	if err != nil {
		writeError(w, s.clusterReadFailure(err, appRef))
		return
	}
	// The Pod lives where the application renders, not where its object is
	// stored, so the render namespace is resolved once and used for both the
	// existence probe and the answer (C-32).
	renderNamespace := applicationNamespace(stored.Object)
	podRef := ObjectRef{Kind: "Pod", Namespace: renderNamespace, Name: podName}
	// A Pod is not a KOS object, so the write is judged on the application that
	// owns it: the role write rule of C-59 is the permission rule of C-77.
	grant, failure := s.decide(r.Context(), VerbDeletePod, appRef, nil, stored.Object)
	actor := s.actorOfGrant(r.Context(), grant)
	if failure != nil {
		s.audit(ctx, actor, grant.Extension, podRef, string(VerbDeletePod), ResultRejected)
		writeError(w, failure)
		return
	}
	pods, err := s.applicationPods(ctx, stored.Object)
	if err != nil {
		writeError(w, s.auditFailure(ctx, actor, grant.Extension, podRef, string(VerbDeletePod),
			s.clusterReadFailure(err, appRef)))
		return
	}
	target, found := findPod(pods, podName)
	if found {
		podRef.Namespace = target.Namespace
	} else {
		// The named Pod is not this application's: a Pod of another
		// application keeps the request refused, while a Pod that is gone
		// makes a repeated delete an accepted no-op (C-77).
		exists, failure := s.podExists(ctx, renderNamespace, podName)
		if failure != nil {
			writeError(w, s.auditFailure(ctx, actor, grant.Extension, podRef, string(VerbDeletePod), failure))
			return
		}
		if exists {
			writeError(w, s.auditFailure(ctx, actor, grant.Extension, podRef, string(VerbDeletePod),
				notFound("Pod "+podRef.String()+" 不属于应用 "+appRef.Name).about(podRef)))
			return
		}
		s.audit(ctx, actor, grant.Extension, podRef, string(VerbDeletePod), ResultAccepted)
		writeJSON(w, http.StatusAccepted, writeResponse{Accepted: true, Ref: new(podRef), Idempotent: true})
		return
	}
	idempotent := podIsTerminating(target.Object)
	if !idempotent {
		if failure := s.deletePod(ctx, target); failure != nil {
			writeError(w, s.auditFailure(ctx, actor, grant.Extension, podRef, string(VerbDeletePod), failure))
			return
		}
	}
	s.audit(ctx, actor, grant.Extension, podRef, string(VerbDeletePod), ResultAccepted)
	writeJSON(w, http.StatusAccepted, writeResponse{Accepted: true, Ref: new(podRef), Idempotent: idempotent})
}

// applicationPods returns the Pods the application's own rendered workloads
// own (C-76): it walks status.rendered, reads each workload's Pod selector and
// lists the Pods that selector matches in the workload's namespace. A Pod of
// another application in the same namespace carries other labels and is not
// returned.
func (s *Server) applicationPods(ctx context.Context, application map[string]any) ([]selectedPod, error) {
	namespace := applicationNamespace(application)
	rendered := renderedRefs(application)
	pods := make([]selectedPod, 0)
	seen := make(map[string]bool)
	listed := make(map[string][]ObservedObject)
	for _, ref := range app.WorkloadRefs(rendered) {
		objectType, known := workloadClass[ref.Kind]
		if !known {
			continue
		}
		if ref.Version != "" {
			objectType.Version = ref.Version
		}
		workloadNamespace := ref.Namespace
		if workloadNamespace == "" {
			workloadNamespace = namespace
		}
		observed, err := s.source.Get(ctx, objectType, workloadNamespace, ref.Name)
		if apierrors.IsNotFound(err) {
			// A workload that is gone has no Pods to project.
			continue
		}
		if err != nil {
			return nil, err
		}
		selected, err := s.workloadPods(ctx, objectType.Kind, observed, workloadNamespace, listed)
		if err != nil {
			return nil, err
		}
		for _, pod := range selected {
			key := pod.Namespace + "/" + pod.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			pods = append(pods, pod)
		}
	}
	sort.Slice(pods, func(left, right int) bool {
		if pods[left].Namespace != pods[right].Namespace {
			return pods[left].Namespace < pods[right].Namespace
		}
		return pods[left].Name < pods[right].Name
	})
	return pods, nil
}

// applicationNamespace reads where an application's render lives: the declared
// namespace wins over the object's own, because that is where the workloads
// and their Pods are (C-32).
func applicationNamespace(application map[string]any) string {
	namespace := nestedText(application, "spec", "namespace")
	if namespace == "" {
		namespace = nestedText(application, "metadata", "namespace")
	}
	return namespace
}

// applicationPaused reports whether the application's desired state is paused
// (C-66): a stopped application projects no Pods (C-76).
func applicationPaused(application map[string]any) bool {
	paused, _, _ := unstructured.NestedBool(application, "spec", "paused")
	return paused
}

// findPod returns the Pod the request names, or a false answer when the
// application does not own one under that name.
func findPod(pods []selectedPod, name string) (selectedPod, bool) {
	for _, pod := range pods {
		if pod.Name == name {
			return pod, true
		}
	}
	return selectedPod{}, false
}

// podIsTerminating reports whether the Pod is already being deleted, so a
// repeat request changes nothing (C-77).
func podIsTerminating(pod map[string]any) bool {
	deletion, _, _ := unstructured.NestedFieldNoCopy(pod, "metadata", "deletionTimestamp")
	return deletion != nil
}

// podExists reads whether a Pod of that name is present in the namespace. The
// delete path uses it to tell a target that is gone from a Pod of another
// application, which the request may not delete (C-77).
func (s *Server) podExists(ctx context.Context, namespace, name string) (bool, *Error) {
	podType := workloadClass["Pod"]
	ref := ObjectRef{Group: podType.Group, Kind: podType.Kind, Namespace: namespace, Name: name}
	_, err := resourceOf(s.dynamic, podType, namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		// The probe is a read: an unreachable runtime names itself the same
		// way the projection route does (C-64, C-76).
		return false, s.clusterReadFailure(err, ref)
	}
	return true, nil
}

// deletePod removes one Pod behind its UID, so a name the runtime reuses is not
// deleted by a request that saw the earlier object. A target that is already
// gone or was replaced is not an error: the request's effect is the one delete.
func (s *Server) deletePod(ctx context.Context, pod selectedPod) *Error {
	podType := workloadClass["Pod"]
	ref := ObjectRef{Group: podType.Group, Kind: podType.Kind, Namespace: pod.Namespace, Name: pod.Name}
	options := metav1.DeleteOptions{}
	if uid := types.UID(nestedText(pod.Object, "metadata", "uid")); uid != "" {
		options.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	err := resourceOf(s.dynamic, podType, pod.Namespace).Delete(ctx, pod.Name, options)
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		return nil
	}
	if err != nil {
		return s.writeFailure(ctx, podType, err, ref)
	}
	return nil
}

// projectPod turns one observed Pod into its projection item (C-76): identity,
// node, phase, readiness, restart count, containers with images and start
// time, plus the source type, observation time and stale marker of C-13.
func projectPod(pod selectedPod, now time.Time) PodProjection {
	object := pod.Object
	item := PodProjection{
		Name:       pod.Name,
		Node:       nestedText(object, "spec", "nodeName"),
		Phase:      nestedText(object, "status", "phase"),
		Ready:      podReady(object),
		Restarts:   podRestarts(object),
		Containers: podContainerImages(object),
		SourceType: PodSourceType,
		ObservedAt: pod.ObservedAt.UTC(),
		Stale:      now.Sub(pod.ObservedAt) > StaleThreshold,
	}
	if started := nestedText(object, "status", "startTime"); started != "" {
		if stamp, err := time.Parse(time.RFC3339Nano, started); err == nil {
			utc := stamp.UTC()
			item.StartedAt = &utc
		}
	}
	return item
}

// podReady reports whether the Pod's Ready condition holds.
func podReady(pod map[string]any) bool {
	conditions, _, _ := unstructured.NestedSlice(pod, "status", "conditions")
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

// podRestarts sums the restart count of the Pod's containers.
func podRestarts(pod map[string]any) int64 {
	statuses, _, _ := unstructured.NestedSlice(pod, "status", "containerStatuses")
	total := int64(0)
	for _, entry := range statuses {
		status, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		switch count := status["restartCount"].(type) {
		case int64:
			total += count
		case int32:
			total += int64(count)
		case int:
			total += int64(count)
		case float64:
			total += int64(count)
		}
	}
	return total
}

// podContainerImages lists the Pod's containers with their images in spec
// order.
func podContainerImages(pod map[string]any) []PodContainer {
	containers, _, _ := unstructured.NestedSlice(pod, "spec", "containers")
	items := make([]PodContainer, 0, len(containers))
	for _, entry := range containers {
		container, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		name := text(container["name"])
		if name == "" {
			continue
		}
		items = append(items, PodContainer{Name: name, Image: text(container["image"])})
	}
	return items
}
