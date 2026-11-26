// Command kos-control-plane is the deployable control-plane process: it serves
// the account and domain API browsers and extensions use (KOSD-C-12,
// KOSD-C-63) and refreshes the contribution projection from the live components
// (KOSD-C-28).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/pkg/access"
	"github.com/tsic404/kosd/pkg/apps"
	"github.com/tsic404/kosd/pkg/budget"
	"github.com/tsic404/kosd/pkg/catalogue"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/degrade"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/identity"
	"github.com/tsic404/kosd/pkg/install"
	"github.com/tsic404/kosd/pkg/maintenance"
	"github.com/tsic404/kosd/pkg/pages"
	"github.com/tsic404/kosd/pkg/panel"
	"github.com/tsic404/kosd/pkg/settings"
	terminalpkg "github.com/tsic404/kosd/pkg/terminal"
	"github.com/tsic404/kosd/pkg/upgrade"
	"github.com/tsic404/kosd/pkg/write"
)

// Defaults a deployment may override by flag.
const (
	defaultAddress      = ":8080"
	defaultDatabasePath = "/var/lib/kosd/kos.db"
	// memorySampleInterval keeps the degradation protection measurable
	// (KOSD-C-57). It runs well inside the five-minute window, so one missed
	// read cannot turn the health probe unavailable.
	memorySampleInterval = 10 * time.Second
	// extensionInstallInterval bounds how often the projection is refreshed
	// from the live components (KOSD-C-28).
	extensionInstallInterval = 10 * time.Second
	// installPassTimeout is the hard limit of one projection refresh.
	installPassTimeout = 30 * time.Second
	// shutdownTimeout bounds the graceful drain after a signal.
	shutdownTimeout = 30 * time.Second
)

// extensionSource is the contribution projection every surface reads. One API
// process outlives any single install pass, so the projection is replaced in
// place whenever a pass finishes (KOSD-C-28).
type extensionSource struct {
	mu       sync.RWMutex
	registry *extension.Registry
}

func newExtensionSource() *extensionSource { return &extensionSource{} }

// publish replaces the projection with the one a pass produced; a nil registry
// is the projection of no active component.
func (s *extensionSource) publish(registry *extension.Registry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registry = registry
}

func (s *extensionSource) current() *extension.Registry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.registry
}

// ObjectClass implements controlplane.ExtensionKinds.
func (s *extensionSource) ObjectClass(group, kind string) (controlplane.ExtensionObjectClass, bool) {
	return s.current().ObjectClass(group, kind)
}

// Settings implements settings.Declarations.
func (s *extensionSource) Settings() []extension.Setting { return s.current().Settings() }

// IsActive implements controlplane.TerminalExtensions: a component that has
// not reached the active set serves no terminal session (KOSD-F-16).
func (s *extensionSource) IsActive(identifier string) bool {
	return s.current().IsActive(identifier)
}

// Setting implements settings.Declarations.
func (s *extensionSource) Setting(id string) (extension.Setting, bool) {
	return s.current().Setting(id)
}

// Pages implements pages.Declarations.
func (s *extensionSource) Pages() []extension.Page { return s.current().Pages() }

// PageFor implements pages.Declarations.
func (s *extensionSource) PageFor(id string, role write.Role) (extension.Page, bool) {
	return s.current().PageFor(id, role)
}

// Apps implements apps.Declarations.
func (s *extensionSource) Apps() []extension.InstalledApp { return s.current().Apps() }

// CatalogueSources implements catalogue.Declarations.
func (s *extensionSource) CatalogueSources() []extension.CatalogueSource {
	return s.current().CatalogueSources()
}

// Capabilities implements catalogue.Declarations.
func (s *extensionSource) Capabilities() []extension.Capability {
	return s.current().Capabilities()
}

// PreferenceKeys implements identity.PreferenceSchemas.
func (s *extensionSource) PreferenceKeys(identifier string) ([]string, bool) {
	return s.current().PreferenceKeys(identifier)
}

// DeclaredBy implements access.ExtensionRoles.
func (s *extensionSource) DeclaredBy(group, kind string) (string, bool) {
	return s.current().DeclaredBy(group, kind)
}

// ObjectWriters implements access.ExtensionRoles.
func (s *extensionSource) ObjectWriters(group, kind string) ([]write.Role, bool) {
	return s.current().ObjectWriters(group, kind)
}

// ReadScopes implements access.ExtensionRoles (KOSD-C-85).
func (s *extensionSource) ReadScopes(identifier string) []access.DeclaredRead {
	return s.current().ReadScopes(identifier)
}

// AppOrigin implements access.ExtensionRoles (KOSD-C-87).
func (s *extensionSource) AppOrigin(identifier, appID string) (string, bool) {
	return s.current().AppOrigin(identifier, appID)
}

// definitionDirectory reads the CRD manifests the release ships and reports the
// kinds that no longer serve the version the control plane requires
// (KOSD-C-16). The process either has a usable object model or refuses to
// start, never serving half of one (KOSD-C-56).
type clusterDefinitions struct {
	client apiextensionsclientset.Interface
}

// MissingDefinitions implements controlplane.Definitions.
func (d clusterDefinitions) MissingDefinitions(ctx context.Context, group, version string, kinds []string) ([]string, error) {
	list, err := d.client.ApiextensionsV1().CustomResourceDefinitions().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list object definitions: %w", err)
	}
	served := map[string]bool{}
	for index := range list.Items {
		definition := &list.Items[index]
		if definition.Spec.Group != group {
			continue
		}
		for _, candidate := range definition.Spec.Versions {
			if candidate.Name == version && candidate.Served {
				served[definition.Spec.Names.Kind] = true
			}
		}
	}
	missing := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		if !served[kind] {
			missing = append(missing, kind)
		}
	}
	return missing, nil
}

// memoryReader feeds the degradation protection the same process memory
// measurement the budget declares (KOSD-C-57).
type memoryReader struct{}

func (memoryReader) ReadMemory(ctx context.Context) (int64, error) {
	return budget.ProcessMemory(ctx)
}

// processBanner exposes the process-wide degradation notice to the page shell
// (KOSD-C-56).
type processBanner struct {
	state *degrade.Static
}

func (b processBanner) Banner() *degrade.Notice { return b.state.State().Notice }

func main() {
	if err := run(); err != nil {
		slog.Error("kos-control-plane stopped", "error", err)
		os.Exit(1)
	}
}

// run parses the flags, builds the cluster clients and serves until the process
// is asked to stop.
func run() error {
	var (
		address              string
		kubeconfig           string
		databasePath         string
		packagesPath         string
		memoryLimit          int64
		terminalRunner       string
		terminalNodeIsolated bool
	)
	flag.StringVar(&address, "address", defaultAddress, "HTTP listen address")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "path to the kubeconfig; empty uses the in-cluster configuration")
	flag.StringVar(&databasePath, "database", defaultDatabasePath, "path to the control-plane database")
	flag.StringVar(&packagesPath, "packages", install.PackageDirectory, "directory extension packages are read from")
	flag.Int64Var(&memoryLimit, "memory-limit", 0, "degradation threshold in bytes; zero selects the default")
	flag.StringVar(&terminalRunner, "terminal-executor", "", "terminal executor: empty leaves sessions unavailable, kubernetes executes Pod sessions")
	flag.BoolVar(&terminalNodeIsolated, "terminal-node-isolated", false, "assert that root@node cannot read control-plane credentials; without it node sessions are refused")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	restConfig, err := clusterConfig(kubeconfig)
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("build kubernetes client: %w", err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("build discovery client: %w", err)
	}
	apiextensionsClient, err := apiextensionsclientset.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("build object definition client: %w", err)
	}
	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("build run kernel client: %w", err)
	}
	var executor controlplane.TerminalExecutor
	switch terminalRunner {
	case "":
	case "kubernetes":
		executor = &terminalpkg.Kubernetes{Client: kubeClient, Config: restConfig}
	default:
		return fmt.Errorf("unknown -terminal-executor %q: want empty or kubernetes", terminalRunner)
	}

	store, err := identity.Open(ctx, identity.OpenOptions{Path: databasePath})
	if err != nil {
		return err
	}
	defer store.Close()

	projection := newExtensionSource()
	// The window tokens live in the control-plane database and are written by the
	// account layer, so the process that serves the accounts also serves the
	// token table the window host takes a token from (KOSD-C-75, KOSD-C-86).
	windowTokens, err := identity.NewAppTokens(store, projection)
	if err != nil {
		return err
	}
	defer windowTokens.Close()
	banner := processBanner{state: &degrade.Static{}}
	protection := degrade.NewProtection(degrade.ProtectionConfig{ThresholdBytes: memoryLimit})
	trail := identity.AuditTrail{Store: store}
	accounts := identity.New(store, identity.Config{
		Owned:       identity.ClusterOwnedObjects{Dynamic: dynamicClient},
		Preferences: projection,
		Auditor:     trail,
		AppTokens:   windowTokens,
		Log:         logger,
	})
	// The upgrade preflight record shares the control-plane database with the
	// accounts, and its two writes are the Owner's (KOSD-C-59, KOSD-C-88).
	resources := resourcesOf(discoveryClient)
	kernelWriter := &write.Writer{Dynamic: dynamicClient, Resources: resources}
	maintenanceFlow := &maintenance.Service{
		Dynamic: dynamicClient,
		Writer:  kernelWriter,
		Pods:    maintenance.Cluster{Kube: kubeClient},
		Auditor: trail,
		Log:     logger,
	}
	componentInstaller := &extension.Installer{
		Dynamic:   dynamicClient,
		Resources: resources,
		Artifacts: extension.DirectoryArtifacts{Root: packagesPath},
		Status:    kernelWriter,
	}
	upgrades := upgrade.New(upgrade.Config{
		Store: store,
		Platform: &upgrade.Flow{
			Maintenance:  maintenanceFlow,
			Applications: upgrade.ClusterApplications{Dynamic: dynamicClient},
			Nodes:        upgrade.ClusterNodes{Dynamic: dynamicClient},
		},
		// The same installer reads the installed extensions' framework
		// declarations, which the platform preflight judges against the target
		// release's declared framework API version set (KOSD-C-16).
		Frameworks: componentInstaller,
		Extensions: componentInstaller,
		Installed:  projection,
		Auditor:    trail,
		Log:        logger,
	})
	catalogueService := catalogue.New(projection, dynamicClient)
	terminalIdle := func(ctx context.Context) (int, *controlplane.Error) {
		values, err := store.SettingValues(ctx, extension.SettingScopeCluster, "")
		if err != nil {
			return 0, &controlplane.Error{
				Code:        controlplane.CodeDatabaseUnavailable,
				Message:     "控制面数据库不可用，无法读取终端空闲配置",
				Stage:       controlplane.StageRead,
				Retryable:   true,
				SideEffects: controlplane.SideEffectsNone,
			}
		}
		minutes, ok := controlplane.TerminalIdleMinutes(values[controlplane.TerminalIdleSettingID])
		if !ok {
			return 0, &controlplane.Error{
				Code:        controlplane.CodeInternal,
				Message:     "已存的终端空闲配置无效",
				Stage:       controlplane.StageRead,
				SideEffects: controlplane.SideEffectsNone,
			}
		}
		return minutes, nil
	}
	authorizer := identity.Authorizer{Policy: &access.Policy{Objects: controlplane.NewClusterSource(dynamicClient), Extensions: projection}}
	domain, err := controlplane.New(controlplane.Config{
		Dynamic:     dynamicClient,
		Extensions:  projection,
		Catalogue:   catalogueService,
		Authorizer:  authorizer,
		Auditor:     trail,
		Actor:       identity.Operator,
		Degradation: banner.state,
		Protection:  protection,
		Definitions: clusterDefinitions{client: apiextensionsClient},
		Terminal: controlplane.TerminalConfig{
			Console:     identity.TerminalSessions{Service: accounts, Store: store},
			Audit:       identity.TerminalAuditTrail{Store: store},
			Extensions:  projection,
			Executor:    executor,
			Boundary:    terminalpkg.Boundary{NodeIsolated: terminalNodeIsolated},
			IdleTimeout: terminalIdle,
		},
		Log: logger,
	})
	if err != nil {
		return err
	}
	defer domain.Close()

	settingsService := settings.New(projection, store, settings.Config{Auditor: trail})
	pagesService := panel.New(projection, panel.Config{Dynamic: dynamicClient, Audit: store, Banners: banner})
	appsService := apps.New(projection)
	settingsAPI := settings.NewAPI(settingsService).Handler()
	catalogueAPI := catalogue.NewAPI(catalogueService).Handler()
	pagesAPI := pages.NewAPI(pagesService)
	appsAPI := apps.NewAPI(appsService).Handler()
	upgradesAPI := upgrade.NewAPI(upgrades).Handler()
	shell := http.NewServeMux()
	shell.Handle("/api/v1/settings", settingsAPI)
	shell.Handle("/api/v1/settings/", settingsAPI)
	shell.Handle("/api/v1/catalogue/", catalogueAPI)
	shell.Handle("/api/v1/pages", pagesAPI.Handler())
	shell.Handle("/api/v1/pages/", pagesAPI.Handler())
	shell.Handle("/api/v1/apps", appsAPI)
	shell.Handle("/api/v1/upgrade-preflights", upgradesAPI)
	shell.Handle("/api/v1/upgrade-preflights/", upgradesAPI)
	shell.Handle("/api/v1/upgrade-requests", upgradesAPI)
	shell.Handle("/", domain.Handler())
	windowIssuer := identity.NewAppTokenIssuer(accounts, windowTokens, authorizer.Policy)
	accountsAPI := identity.NewAPIWithAppTokens(accounts, windowTokens, windowIssuer).Handler(shell)
	// The health endpoint is the probe surface: it answers before any session
	// exists, so it is served ahead of the account gate (KOSD-C-56, KOSD-C-57).
	root := http.NewServeMux()
	root.Handle("GET /api/v1/system/health", domain.Handler())
	// A terminal token is the only credential the attach and input routes
	// accept, so they sit ahead of the console gate (KOSD-C-91).
	root.Handle("POST /api/v1/terminal/sessions/{sessionId}/attach", domain.TerminalPublicHandler())
	root.Handle("POST /api/v1/terminal/sessions/{sessionId}/input", domain.TerminalPublicHandler())
	root.Handle("/", pagesAPI.RefuseWrites(accountsAPI))
	server := &http.Server{Addr: address, Handler: root, ReadHeaderTimeout: 10 * time.Second}

	// One sample before serving keeps the probe answerable from the first
	// request instead of reporting an unmeasured process (KOSD-C-57).
	protection.Sample(ctx, memoryReader{})
	go sampleMemory(ctx, protection)
	go refreshProjection(ctx, dynamicClient, discoveryClient, projection, extension.DirectoryArtifacts{Root: packagesPath}, logger)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	}()

	logger.Info("kos-control-plane started", "address", address, "database", databasePath, "packages", packagesPath)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// sampleMemory keeps the degradation protection measurable (KOSD-C-57).
func sampleMemory(ctx context.Context, protection *degrade.Protection) {
	ticker := time.NewTicker(memorySampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			protection.Sample(ctx, memoryReader{})
		}
	}
}

// refreshProjection runs the extension install pass and publishes what it
// produced. A failed pass keeps the previous projection: an unreadable cluster
// is a transient fact, not a withdrawal (KOSD-C-56).
func refreshProjection(ctx context.Context, client dynamic.Interface, discoveryClient discovery.DiscoveryInterface, projection *extensionSource, artifacts extension.ArtifactSource, logger *slog.Logger) {
	resources := resourcesOf(discoveryClient)
	installer := &extension.Installer{
		Dynamic:   client,
		Resources: resources,
		Artifacts: artifacts,
		Status:    &write.Writer{Dynamic: client, Resources: resources},
	}
	ticker := time.NewTicker(extensionInstallInterval)
	defer ticker.Stop()
	for {
		passCtx, cancel := context.WithTimeout(ctx, installPassTimeout)
		installation, err := installer.Install(passCtx)
		cancel()
		if err != nil {
			logger.Error("extension install pass failed", "error", err)
		} else {
			projection.publish(installation.Registry())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// resourcesOf resolves every kernel kind and the native kinds an extension
// controller may write (KOSD-C-04), the map the write path needs.
func resourcesOf(client discovery.DiscoveryInterface) map[schema.GroupKind]string {
	resources := make(map[schema.GroupKind]string, len(api.Kinds()))
	for _, kind := range api.Kinds() {
		if name, ok := api.ResourceName(kind); ok {
			resources[schema.GroupKind{Group: api.Group, Kind: kind}] = name
		}
	}
	for _, groupVersion := range []schema.GroupVersion{
		{Version: "v1"}, {Group: "apps", Version: "v1"}, {Group: "batch", Version: "v1"},
	} {
		list, err := client.ServerResourcesForGroupVersion(groupVersion.String())
		if err != nil {
			continue
		}
		for _, resource := range list.APIResources {
			if strings.Contains(resource.Name, "/") {
				continue
			}
			key := schema.GroupKind{Group: groupVersion.Group, Kind: resource.Kind}
			if _, known := resources[key]; !known {
				resources[key] = resource.Name
			}
		}
	}
	return resources
}

// clusterConfig builds the client configuration: the given kubeconfig, or the
// service account the control plane runs under.
func clusterConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("read kubeconfig %s: %w", kubeconfig, err)
		}
		return config, nil
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("read the in-cluster configuration: %w", err)
	}
	return config, nil
}
