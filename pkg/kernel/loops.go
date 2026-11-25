package kernel

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/api/v1beta1"
	"github.com/tsic404/kosd/pkg/app"
	"github.com/tsic404/kosd/pkg/backup"
	"github.com/tsic404/kosd/pkg/catalogue"
	"github.com/tsic404/kosd/pkg/endpoint"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/migration"
	"github.com/tsic404/kosd/pkg/provider"
	"github.com/tsic404/kosd/pkg/reconcile"
	"github.com/tsic404/kosd/pkg/volume"
	"github.com/tsic404/kosd/pkg/write"
)

// loopConfig is what the kernel loops need from the process that hosts them.
type loopConfig struct {
	// Dynamic reads and writes every kernel kind.
	Dynamic dynamic.Interface
	// Resources maps a kind to the resource name discovery reported.
	Resources map[schema.GroupKind]string
	// Writer is the admitted write path every reconciler writes through.
	Writer *write.Writer
	// Projection is the contribution projection the loops read (C-28).
	Projection *Projection
	// Catalogue is the directory source sync of C-24.
	Catalogue *catalogue.Service
	// Artifacts resolves an extension package reference.
	Artifacts extension.ArtifactSource
	// Controllers is the extension controller set; it applies each component's
	// enabled state and projects an unavailable controller's objects as
	// Unknown (C-20, C-28).
	Controllers *extension.ControllerSet
	// ControllerSource resolves the implementation this process hosts for one
	// declared extension controller (C-20). A contribution the source does not
	// resolve is declared but unavailable, so its objects project Unknown
	// instead of a stored status nobody maintains (C-08, C-20). nil means this
	// process hosts no extension controller implementation.
	ControllerSource ControllerSource
	// BackupRoot is the directory the backup worker reaches volume data under.
	BackupRoot string
	// BackupImage is the digest-pinned backup component image.
	BackupImage string
	// MigrationImage is the image that runs one migration step.
	MigrationImage string
	// Owner is the global Owner's username; an empty value leaves the restore
	// preflight without its Owner exemption (C-59, C-60).
	Owner string
	// Logger records the loops' failures.
	Logger *slog.Logger
}

// kernelLoops runs every kernel reconcile subject and the two periodic jobs the
// deployment owns: the install pass that refreshes the projection and the
// catalogue sync that writes application definitions (KOSD-C-03, KOSD-C-24).
type kernelLoops struct {
	config    loopConfig
	loops     []*reconcile.Controller
	observers []observer
	// hosted records the controller contributions the set holds, keyed by
	// component object name, with the signature of the set it was built from.
	// The install pass compares a fresh signature so a changed declaration
	// restarts its loops and a withdrawn component leaves the set (C-28).
	hosted map[string]string
	// seen holds the object identifiers each class carried at the last
	// successful sweep, so the next sweep can deliver the ones that
	// disappeared (KOSD-C-79). Only sweepLoop reads and writes it.
	seen map[string]map[string]struct{}
}

// observer is one reconciler and the object class whose appearance or change
// triggers it. The endpoint reconciler also observes applications, because an
// endpoint's route follows its target application (KOSD-C-43, KOSD-C-78).
type observer struct {
	kind       string
	controller *reconcile.Controller
}

// newKernelLoops assembles the reconcilers over one write path. Every subject
// the C-03 and C-04 tables give a status or downstream write to is built here;
// a subject without a writer would leave its objects unobservable forever.
func newKernelLoops(config loopConfig) (*kernelLoops, error) {
	loops := &kernelLoops{
		config: config,
		hosted: map[string]string{},
		seen:   map[string]map[string]struct{}{},
	}

	// The provider catalogue is the capability view the application and volume
	// reconcilers compare requirements against (KOSD-C-09, KOSD-C-47). Without
	// it every application that declares a requirement would stay Pending.
	capabilities := provider.NewCatalogue(config.Writer)
	applications, err := loops.register("Application", app.NewController(
		app.NewWriterStore(config.Writer),
		app.Options{Capabilities: capabilities},
	))
	if err != nil {
		return nil, err
	}
	_ = applications
	if _, err := loops.register("Volume", volume.NewController(
		volume.NewWriterStore(config.Writer),
		volume.Options{
			Capabilities: versionSource{catalogue: capabilities},
			References:   referenceGate{dynamic: config.Dynamic, projection: config.Projection},
		},
	)); err != nil {
		return nil, err
	}
	// The endpoint reconciler observes its own class and the applications its
	// routes point at (KOSD-C-43, KOSD-C-45).
	endpoints, err := loops.register("Endpoint", endpoint.NewReconciler(config.Writer, endpointFacts{catalogue: capabilities}))
	if err != nil {
		return nil, err
	}
	if err := loops.observe("Application", endpoints); err != nil {
		return nil, err
	}
	backupStore := backup.NewWriterStore(config.Writer, write.SubjectBackupReconciler)
	worker := backup.NewLocalWorker(backup.DirDataPlane{Root: config.BackupRoot})
	backupOptions := backup.Options{
		DefaultTarget: config.BackupRoot,
		WorkerImage:   config.BackupImage,
		Owner:         config.Owner,
	}
	if _, err := loops.register("BackupPolicy", backup.NewController(backupStore, worker, backupOptions)); err != nil {
		return nil, err
	}
	if _, err := loops.register("RestoreJob", backup.NewRestoreController(
		backup.NewWriterStore(config.Writer, write.SubjectMigrationReconciler),
		backup.DirDataPlane{Root: config.BackupRoot}, worker, backupOptions,
	)); err != nil {
		return nil, err
	}
	if _, err := loops.register("VolumeMigration", migration.NewController(
		migration.NewWriterStore(config.Writer),
		migration.Options{RunnerImage: config.MigrationImage},
	)); err != nil {
		return nil, err
	}
	// The validator is the Provider's only status writer (KOSD-C-03, KOSD-C-47).
	if _, err := loops.register("Provider", provider.NewReconciler(config.Writer, provider.Options{})); err != nil {
		return nil, err
	}
	return loops, nil
}

// register wraps one reconciler in its own bounded loop and records the class
// that triggers it.
func (l *kernelLoops) register(kind string, reconciler reconcile.Reconciler) (*reconcile.Controller, error) {
	controller, err := reconcile.NewController(reconciler, reconcile.Options{Workers: 1})
	if err != nil {
		return nil, err
	}
	l.loops = append(l.loops, controller)
	l.observers = append(l.observers, observer{kind: kind, controller: controller})
	return controller, nil
}

// observe adds a trigger class to an already registered controller.
func (l *kernelLoops) observe(kind string, controller *reconcile.Controller) error {
	for _, known := range l.observers {
		if known.controller == controller && known.kind == kind {
			return nil
		}
	}
	l.observers = append(l.observers, observer{kind: kind, controller: controller})
	return nil
}

// run starts every loop and the two periodic jobs, and blocks until ctx ends.
func (l *kernelLoops) run(ctx context.Context) {
	var running sync.WaitGroup
	running.Add(len(l.loops))
	for _, loop := range l.loops {
		go func() {
			defer running.Done()
			loop.Run(ctx)
		}()
	}
	for _, job := range []func(context.Context){
		l.config.Controllers.Run,
		l.installLoop,
		l.sweepLoop,
		l.catalogueLoop,
	} {
		running.Add(1)
		go func() {
			defer running.Done()
			job(ctx)
		}()
	}
	running.Wait()
}

// installLoop runs the extension install pass and publishes what it produced. A
// failed pass keeps the previous projection: an unreadable cluster is a
// transient fact, not a withdrawal (KOSD-C-56).
func (l *kernelLoops) installLoop(ctx context.Context) {
	installer := &extension.Installer{
		Dynamic:     l.config.Dynamic,
		Resources:   l.config.Resources,
		Artifacts:   l.config.Artifacts,
		Status:      l.config.Writer,
		Controllers: l.config.Controllers,
		Log:         l.config.Logger,
	}
	ticker := time.NewTicker(extensionInstallInterval)
	defer ticker.Stop()
	for {
		passCtx, cancel := context.WithTimeout(ctx, installPassTimeout)
		installation, err := installer.Install(passCtx)
		cancel()
		if err != nil {
			l.config.Logger.Error("extension install pass failed", "error", err)
		} else {
			l.config.Projection.Publish(installation.Registry())
			l.syncControllers(installation.Components())
			l.syncCatalogue(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ControllerSource resolves the reconcile loop this process hosts for one
// declared extension controller (C-20). ok is false for a contribution whose
// implementation this binary does not link.
type ControllerSource func(identifier string, contribution extension.Controller) (reconcile.Reconciler, bool)

// syncControllers brings the controller set in line with one install pass: an
// active component's declared controllers are registered with the
// implementation this process hosts, or declared when it hosts none, so their
// objects project Unknown instead of a stored status nobody maintains
// (C-08, C-20). A component that left the active set stops and leaves the set
// (C-28, C-29).
func (l *kernelLoops) syncControllers(components []extension.ComponentResult) {
	if l.config.Controllers == nil {
		return
	}
	next := map[string]string{}
	for _, result := range components {
		if !extension.IsContributing(result) {
			continue
		}
		if len(result.Contributions.Controllers) == 0 {
			continue
		}
		signature := controllerSignature(result.Contributions.Controllers)
		next[result.Name] = signature
		if l.hosted[result.Name] == signature {
			continue
		}
		// A changed contribution set replaces the whole set: a controller the
		// declaration no longer names must not keep a serving entry.
		l.config.Controllers.Unregister(result.Name)
		for _, contribution := range result.Contributions.Controllers {
			l.startController(result, contribution)
		}
	}
	for component := range l.hosted {
		if _, kept := next[component]; kept {
			continue
		}
		l.config.Controllers.Unregister(component)
	}
	l.hosted = next
}

// startController registers the implementation this process hosts, or records
// the declaration as unavailable when it hosts none (C-20).
func (l *kernelLoops) startController(result extension.ComponentResult, contribution extension.Controller) {
	if l.config.ControllerSource != nil {
		if reconciler, ok := l.config.ControllerSource(result.Identifier, contribution); ok {
			if err := l.config.Controllers.Register(result.Name, contribution, reconciler); err != nil {
				l.config.Logger.Error("extension controller did not register",
					"component", result.Name, "controller", contribution.ID, "error", err)
			}
			return
		}
	}
	if err := l.config.Controllers.Declare(result.Name, contribution); err != nil {
		l.config.Logger.Error("extension controller did not declare",
			"component", result.Name, "controller", contribution.ID, "error", err)
	}
}

// controllerSignature names one component's controller set, so an unchanged
// declaration does not restart its loops on every pass.
func controllerSignature(contributions []extension.Controller) string {
	parts := make([]string, 0, len(contributions))
	for _, contribution := range contributions {
		classes := make([]string, 0, len(contribution.Manages))
		for _, class := range contribution.Manages {
			classes = append(classes, class.String())
		}
		sort.Strings(classes)
		parts = append(parts, contribution.ID+"="+strings.Join(classes, ","))
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

// catalogueLoop writes the definitions of installable entries even when no
// install pass changes anything (KOSD-C-24).
func (l *kernelLoops) catalogueLoop(ctx context.Context) {
	ticker := time.NewTicker(catalogueSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.syncCatalogue(ctx)
		}
	}
}

// syncCatalogue writes the definitions the active catalogue sources offer. An
// error is transient: the next pass retries it (KOSD-C-24).
func (l *kernelLoops) syncCatalogue(ctx context.Context) {
	if l.config.Catalogue == nil {
		return
	}
	if err := l.config.Catalogue.Sync(ctx); err != nil {
		l.config.Logger.Error("catalogue sync failed", "error", err)
	}
}

// sweepLoop hands every stored object of an observed class to its reconciler.
// The kernel has no watch source yet, so this is how an object that appeared
// after the process started reaches its loop; the reconciler's own requeue
// carries it from there (KOSD-C-06, KOSD-C-79).
func (l *kernelLoops) sweepLoop(ctx context.Context) {
	l.sweep(ctx)
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.sweep(ctx)
		}
	}
}

// sweep lists one class at a time and enqueues every stored object, followed by
// the identifiers that disappeared since the last successful list. The kernel
// has no watch source, so a reconciler's cleanup round for a deleted object is
// reached only by this delivery (KOSD-C-79). A failed list is not a deletion:
// the previous snapshot stays and the next sweep retries it (KOSD-C-56).
func (l *kernelLoops) sweep(ctx context.Context) {
	for _, kind := range l.observedKinds() {
		if err := l.enqueueKind(ctx, kind); err != nil {
			l.config.Logger.Error("kernel sweep failed", "kind", kind, "error", err)
		}
	}
	l.sweepExtensionClasses(ctx)
}

// observedKinds returns the kernel classes the sweep lists, each once even when
// two reconcilers observe it.
func (l *kernelLoops) observedKinds() []string {
	kinds := make([]string, 0, len(l.observers))
	seen := map[string]bool{}
	for _, observer := range l.observers {
		if seen[observer.kind] {
			continue
		}
		seen[observer.kind] = true
		kinds = append(kinds, observer.kind)
	}
	return kinds
}

// enqueueKind lists one kernel class across every namespace and enqueues one
// round per object to each reconciler observing it.
func (l *kernelLoops) enqueueKind(ctx context.Context, kind string) error {
	plural, known := api.ResourceName(kind)
	if !known {
		return nil
	}
	key := api.Group + "/" + kind
	return l.deliverClass(ctx, key, schema.GroupVersionResource{
		Group:    api.Group,
		Version:  v1beta1.GroupVersion.Version,
		Resource: plural,
	}, kind, l.observersOf(kind), false)
}

// sweepExtensionClasses lists the extension classes this process can reconcile
// and delivers their objects to the controller that manages them, so an
// installed extension's objects reach their loop (KOSD-C-79).
func (l *kernelLoops) sweepExtensionClasses(ctx context.Context) {
	if l.config.Controllers == nil {
		return
	}
	registry := l.config.Projection.Registry()
	if registry == nil {
		return
	}
	for _, class := range registry.ObjectClasses() {
		if class.Resource == "" || len(class.Versions) == 0 {
			continue
		}
		if !l.config.Controllers.Implemented(class.Group, class.Kind) {
			continue
		}
		key := class.Group + "/" + class.Kind
		if err := l.deliverClass(ctx, key, schema.GroupVersionResource{
			Group:    class.Group,
			Version:  class.Versions[0],
			Resource: class.Resource,
		}, class.Kind, nil, true); err != nil {
			l.config.Logger.Error("extension sweep failed", "class", class.GroupKind().String(), "error", err)
		}
	}
}

// deliverClass lists one class and enqueues its stored objects to the kernel
// reconcilers and, when deliverExtension is set, to the extension controller
// that manages the class. Identifiers that were in the previous successful list
// but are gone now are delivered as well: a deletion reaches the reconciler
// that must clean up its downstream objects (KOSD-C-79).
func (l *kernelLoops) deliverClass(
	ctx context.Context,
	key string,
	gvr schema.GroupVersionResource,
	kind string,
	controllers []*reconcile.Controller,
	deliverExtension bool,
) error {
	list, err := l.config.Dynamic.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	current := make(map[string]struct{}, len(list.Items))
	for index := range list.Items {
		object := &list.Items[index]
		identifier := object.GetNamespace() + "/" + object.GetName()
		current[identifier] = struct{}{}
		request := reconcile.Request{Kind: kind, Namespace: object.GetNamespace(), Name: object.GetName()}
		for _, controller := range controllers {
			controller.Enqueue(request)
		}
		if deliverExtension {
			l.config.Controllers.EnqueueClass(gvr.Group, kind, request)
		}
	}
	for identifier := range l.seen[key] {
		if _, present := current[identifier]; present {
			continue
		}
		namespace, name := splitIdentifier(identifier)
		request := reconcile.Request{Kind: kind, Namespace: namespace, Name: name}
		for _, controller := range controllers {
			controller.Enqueue(request)
		}
		if deliverExtension {
			l.config.Controllers.EnqueueClass(gvr.Group, kind, request)
		}
	}
	l.seen[key] = current
	return nil
}

// observersOf returns every reconciler triggered by one kernel class.
func (l *kernelLoops) observersOf(kind string) []*reconcile.Controller {
	controllers := make([]*reconcile.Controller, 0, 1)
	for _, observer := range l.observers {
		if observer.kind == kind {
			controllers = append(controllers, observer.controller)
		}
	}
	return controllers
}

// splitIdentifier reverses the namespace/name key a sweep stores.
func splitIdentifier(identifier string) (string, string) {
	for index := 0; index < len(identifier); index++ {
		if identifier[index] == '/' {
			return identifier[:index], identifier[index+1:]
		}
	}
	return "", identifier
}

// versionSource answers the registered version of one capability, which is what
// a volume's status records (KOSD-C-38).
type versionSource struct {
	catalogue *provider.Catalogue
}

// Version implements volume.CapabilitySource.
func (s versionSource) Version(ctx context.Context, name string) (string, bool, error) {
	registered, err := s.catalogue.Capabilities(ctx)
	if err != nil {
		return "", false, err
	}
	for _, capability := range registered {
		if capability.Name == name {
			return capability.Version, true, nil
		}
	}
	return "", false, nil
}

// referenceGate reports the extension objects that declare a reference to one
// kernel object (KOSD-C-19). The gate reads the current projection, so a
// component installed after the process started holds its references too.
type referenceGate struct {
	dynamic    dynamic.Interface
	projection *Projection
}

// Referrers implements volume.ReferenceGate.
func (g referenceGate) Referrers(ctx context.Context, target write.Ref) ([]v1beta1.ObjectRef, error) {
	return extension.Referrers(ctx, g.dynamic, g.projection.Registry(), target)
}

// endpointFacts answers the two declarations C-43 requires before an http.path
// endpoint is accepted. No package field declares path safety yet, so a
// path-prefix shape stays refused, which is ADR-008's default.
type endpointFacts struct {
	catalogue *provider.Catalogue
}

// PathSafe implements endpoint.Facts.
func (f endpointFacts) PathSafe(context.Context, write.Ref) (bool, error) {
	return false, nil
}

// CapabilityRegistered implements endpoint.Facts.
func (f endpointFacts) CapabilityRegistered(ctx context.Context, capability string) (bool, error) {
	registered, err := f.catalogue.Capabilities(ctx)
	if err != nil {
		return false, err
	}
	for _, candidate := range registered {
		if candidate.Name == capability {
			return true, nil
		}
	}
	return false, nil
}
