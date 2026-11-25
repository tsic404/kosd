package extension

import (
	"context"
	"errors"
	"fmt"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kosd/api/v1beta1"
	"github.com/tsic404/kosd/pkg/reconcile"
	"github.com/tsic404/kosd/pkg/state"
)

// DefaultFailureThreshold is how many consecutive failed rounds make a
// controller unavailable (C-20).
const DefaultFailureThreshold = 3

// ErrControllerDisabled is returned when a disabled component's controller is
// asked for a round; a disabled component's controllers stop writing (C-28).
var ErrControllerDisabled = errors.New("controller is disabled")

// ControllerState is the observed availability of one extension controller.
type ControllerState struct {
	Component           string
	Controller          string
	IsAvailable         bool
	ConsecutiveFailures int
	LastError           string
}

// ControllerSet runs one reconcile loop per declared controller (C-06, C-20).
// Each loop has its own health: a controller that keeps failing never stops
// another one, and only the classes it manages become unobservable.
type ControllerSet struct {
	threshold int
	mu        sync.Mutex
	entries   map[controllerKey]*controllerEntry
	serving   map[schema.GroupKind]controllerKey
	running   bool
	runCtx    context.Context
	loops     sync.WaitGroup
}

// controllerKey identifies one controller contribution.
type controllerKey struct {
	component  string
	controller string
}

// controllerEntry is one registered loop and its health.
type controllerEntry struct {
	controller *reconcile.Controller
	// contribution is what the package declared: the classes it serves and the
	// write scope it was granted.
	contribution Controller
	// cancel stops this entry's loop when the component is withdrawn.
	cancel    context.CancelFunc
	state     ControllerState
	isEnabled bool
}

// NewControllerSet builds a set of controller loops; a threshold below one
// selects DefaultFailureThreshold.
func NewControllerSet(threshold int) *ControllerSet {
	if threshold < 1 {
		threshold = DefaultFailureThreshold
	}
	return &ControllerSet{
		threshold: threshold,
		entries:   map[controllerKey]*controllerEntry{},
		serving:   map[schema.GroupKind]controllerKey{},
	}
}

// Declare records one declared controller that this process has no
// implementation for. Its classes are served, but the controller counts as
// unavailable, so the objects it manages project as Unknown instead of showing
// a stored status nobody maintains (KOSD-C-08, KOSD-C-20).
func (s *ControllerSet) Declare(component string, contribution Controller) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := controllerKey{component: component, controller: contribution.ID}
	if _, exists := s.entries[key]; exists {
		return nil
	}
	for _, class := range contribution.Manages {
		if owner, taken := s.serving[class.Schema()]; taken && owner != key {
			return fmt.Errorf("class %s already has controller %s/%s", class.String(), owner.component, owner.controller)
		}
	}
	s.entries[key] = &controllerEntry{
		contribution: contribution,
		isEnabled:    true,
		state: ControllerState{
			Component:  component,
			Controller: contribution.ID,
			LastError:  "控制器没有可执行的实现",
		},
	}
	for _, class := range contribution.Manages {
		s.serving[class.Schema()] = key
	}
	return nil
}

// Register adds one controller contribution. A class already served by another
// controller is refused: one object has one extension controller (C-20).
func (s *ControllerSet) Register(component string, contribution Controller, reconciler reconcile.Reconciler) error {
	if reconciler == nil {
		return errors.New("controller has no reconciler")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := controllerKey{component: component, controller: contribution.ID}
	if existing, exists := s.entries[key]; exists {
		if existing.controller != nil {
			return nil
		}
		// A declaration the process can now execute stops projecting as
		// unavailable and starts its loop (C-20).
		controller, err := reconcile.NewController(
			recordingReconciler{set: s, key: key, inner: reconciler},
			reconcile.Options{},
		)
		if err != nil {
			return err
		}
		existing.controller = controller
		existing.state.IsAvailable = true
		existing.state.LastError = ""
		existing.state.ConsecutiveFailures = 0
		s.startLocked(key, existing)
		return nil
	}
	for _, class := range contribution.Manages {
		if owner, taken := s.serving[class.Schema()]; taken {
			return fmt.Errorf("class %s already has controller %s/%s", class.String(), owner.component, owner.controller)
		}
	}
	controller, err := reconcile.NewController(
		recordingReconciler{set: s, key: key, inner: reconciler},
		reconcile.Options{},
	)
	if err != nil {
		return err
	}
	s.entries[key] = &controllerEntry{
		controller:   controller,
		contribution: contribution,
		isEnabled:    true,
		state: ControllerState{
			Component:   component,
			Controller:  contribution.ID,
			IsAvailable: true,
		},
	}
	for _, class := range contribution.Manages {
		s.serving[class.Schema()] = key
	}
	s.startLocked(key, s.entries[key])
	return nil
}

// Unregister removes every controller of one component and stops its loops. A
// withdrawn component's classes are no longer served and its rounds stop
// (KOSD-C-28, KOSD-C-29).
func (s *ControllerSet) Unregister(component string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range s.entries {
		if key.component != component {
			continue
		}
		if entry.cancel != nil {
			entry.cancel()
			entry.cancel = nil
		}
		for _, class := range entry.contribution.Manages {
			if owner, ok := s.serving[class.Schema()]; ok && owner == key {
				delete(s.serving, class.Schema())
			}
		}
		delete(s.entries, key)
	}
}

// Enqueue asks for a round of one object on one controller.
func (s *ControllerSet) Enqueue(component, id string, request reconcile.Request) error {
	s.mu.Lock()
	entry, ok := s.entries[controllerKey{component: component, controller: id}]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("no controller %s/%s", component, id)
	}
	isEnabled := entry.isEnabled
	s.mu.Unlock()
	if !isEnabled {
		return ErrControllerDisabled
	}
	entry.controller.Enqueue(request)
	return nil
}

// EnqueueClass delivers one object of a served class to the controller that
// manages it. A class with no implementation has no loop to deliver to, so the
// round is dropped: its objects stay unobservable (KOSD-C-08, KOSD-C-20).
func (s *ControllerSet) EnqueueClass(group, kind string, request reconcile.Request) {
	s.mu.Lock()
	key, served := s.serving[schema.GroupKind{Group: group, Kind: kind}]
	entry, known := s.entries[key]
	deliverable := served && known && entry.isEnabled && entry.controller != nil
	s.mu.Unlock()
	if !deliverable {
		return
	}
	entry.controller.Enqueue(request)
}

// Serving reports which controller serves one object class. ok is false when no
// active component declares the class (KOSD-C-19).
func (s *ControllerSet) Serving(group, kind string) (string, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, served := s.serving[schema.GroupKind{Group: group, Kind: kind}]
	if !served {
		return "", "", false
	}
	return key.component, key.controller, true
}

// Implemented reports whether one served class has a controller this process
// can execute. A declared class without an implementation is served but never
// reconciled, so a caller that lists objects for delivery can skip it
// (KOSD-C-20).
func (s *ControllerSet) Implemented(group, kind string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, served := s.serving[schema.GroupKind{Group: group, Kind: kind}]
	if !served {
		return false
	}
	entry, known := s.entries[key]
	return known && entry.controller != nil
}

// SetEnabled starts or stops every controller of one component. A disabled
// component's controllers produce no further rounds; enabling them resumes
// reconciliation (C-28).
func (s *ControllerSet) SetEnabled(component string, isEnabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range s.entries {
		if key.component == component {
			entry.isEnabled = isEnabled
		}
	}
}

// isEnabled reports whether one controller accepts rounds.
func (s *ControllerSet) isEnabled(key controllerKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	return ok && entry.isEnabled
}

// Run starts every loop and blocks until ctx is done. A controller the install
// pass registers after the process started begins its loop then, so an
// extension installed at runtime reaches the set (KOSD-C-28, KOSD-C-79).
func (s *ControllerSet) Run(ctx context.Context) {
	s.mu.Lock()
	s.running = true
	s.runCtx = ctx
	for key, entry := range s.entries {
		s.startLocked(key, entry)
	}
	s.mu.Unlock()

	<-ctx.Done()
	s.mu.Lock()
	s.running = false
	s.runCtx = nil
	for _, entry := range s.entries {
		if entry.cancel != nil {
			entry.cancel()
			entry.cancel = nil
		}
	}
	s.mu.Unlock()
	s.loops.Wait()
}

// startLocked starts one entry's loop when the set is running and the entry has
// an implementation. The caller holds the lock.
func (s *ControllerSet) startLocked(key controllerKey, entry *controllerEntry) {
	if !s.running || s.runCtx == nil || entry.controller == nil || entry.cancel != nil {
		return
	}
	loopCtx, cancel := context.WithCancel(s.runCtx)
	entry.cancel = cancel
	s.loops.Add(1)
	go func() {
		defer s.loops.Done()
		entry.controller.Run(loopCtx)
	}()
}

// State returns one controller's state.
func (s *ControllerSet) State(component, id string) (ControllerState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[controllerKey{component: component, controller: id}]
	if !ok {
		return ControllerState{}, false
	}
	return entry.state, true
}

// ProjectedStatus is what a reader sees of one object: when its controller is
// unavailable the object is Unknown, and every other class keeps its status
// (C-08, C-20).
func (s *ControllerSet) ProjectedStatus(group, kind string, stored v1beta1.ObjectStatus) v1beta1.ObjectStatus {
	s.mu.Lock()
	key, served := s.serving[schema.GroupKind{Group: group, Kind: kind}]
	entry, known := s.entries[key]
	available, controller := true, ""
	if served && known {
		available, controller = entry.state.IsAvailable, entry.state.Controller
	}
	s.mu.Unlock()

	if available {
		return stored
	}
	projected := stored
	projected.Health.State = v1beta1.HealthStateUnknown
	projected.Conditions = append([]metav1.Condition(nil), stored.Conditions...)
	observed := state.ReconciledOf(stored.Conditions)
	projected.Conditions = state.Set(projected.Conditions,
		state.Unobserved(observed.ObservedGeneration, "扩展控制器 "+controller+" 不可用"))
	return projected
}

// record applies one round's outcome to the controller's health. A successful
// round makes an unavailable controller available again.
func (s *ControllerSet) record(key controllerKey, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok {
		return
	}
	if err == nil {
		entry.state.IsAvailable = true
		entry.state.ConsecutiveFailures = 0
		entry.state.LastError = ""
		return
	}
	entry.state.ConsecutiveFailures++
	entry.state.LastError = err.Error()
	if entry.state.ConsecutiveFailures >= s.threshold {
		entry.state.IsAvailable = false
	}
}

// recordingReconciler reports each round's outcome to the set before returning
// it to the loop.
type recordingReconciler struct {
	set   *ControllerSet
	key   controllerKey
	inner reconcile.Reconciler
}

// Reconcile runs the round and records its outcome. A round belonging to a
// component that was disabled while queued is dropped, so a stopped controller
// never writes (C-28).
func (r recordingReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	if !r.set.isEnabled(r.key) {
		return reconcile.Result{}, nil
	}
	result, err := r.inner.Reconcile(ctx, request)
	r.set.record(r.key, err)
	return result, err
}
