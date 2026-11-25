package kernel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
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
	"github.com/tsic404/kosd/pkg/maintenance"
	"github.com/tsic404/kosd/pkg/pages"
	"github.com/tsic404/kosd/pkg/panel"
	"github.com/tsic404/kosd/pkg/settings"
	terminalpkg "github.com/tsic404/kosd/pkg/terminal"
	"github.com/tsic404/kosd/pkg/upgrade"
	"github.com/tsic404/kosd/pkg/write"
)

// Process is one running control-plane process: the read projection served over
// HTTP and every kernel reconcile loop, in a single process (ADR-011).
type Process struct {
	// Server is the HTTP surface the process serves.
	Server *http.Server

	store  *identity.Store
	domain *controlplane.Server
	close  sync.Once
}

// Start builds the process and starts its background work. The caller serves
// Server and ends the process by cancelling ctx; Close releases the resources
// the process opened.
func Start(ctx context.Context, options Options) (*Process, error) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return start(ctx, options, logger)
}

func start(ctx context.Context, options Options, logger *slog.Logger) (*Process, error) {
	slog.SetDefault(logger)

	restConfig, err := clusterConfig(options.Kubeconfig)
	if err != nil {
		return nil, err
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("build discovery client: %w", err)
	}
	apiextensionsClient, err := apiextensionsclientset.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("build object definition client: %w", err)
	}
	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("build run kernel client: %w", err)
	}
	var executor controlplane.TerminalExecutor
	switch options.TerminalExecutor {
	case "":
	case "kubernetes":
		executor = &terminalpkg.Kubernetes{Client: kubeClient, Config: restConfig}
	default:
		return nil, fmt.Errorf("unknown -terminal-executor %q: want empty or kubernetes", options.TerminalExecutor)
	}

	store, err := identity.Open(ctx, identity.OpenOptions{Path: options.DatabasePath})
	if err != nil {
		return nil, err
	}

	projection := NewProjection()
	banner := processBanner{state: &degrade.Static{}}
	protection := degrade.NewProtection(degrade.ProtectionConfig{ThresholdBytes: options.MemoryLimit})
	trail := identity.AuditTrail{Store: store}
	accounts := identity.New(store, identity.Config{
		Owned:       identity.ClusterOwnedObjects{Dynamic: dynamicClient},
		Preferences: projection,
		Auditor:     trail,
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
	artifacts := extension.DirectoryArtifacts{Root: options.PackagesPath}
	componentInstaller := &extension.Installer{
		Dynamic:   dynamicClient,
		Resources: resources,
		Artifacts: artifacts,
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
	controllers := extension.NewControllerSet(extension.DefaultFailureThreshold)
	owner := firstOwnerName(ctx, store)
	loops, err := newKernelLoops(loopConfig{
		Dynamic:          dynamicClient,
		Resources:        resources,
		Writer:           kernelWriter,
		Projection:       projection,
		Catalogue:        catalogueService,
		Artifacts:        artifacts,
		Controllers:      controllers,
		ControllerSource: options.ControllerSource,
		BackupRoot:       options.BackupRoot,
		BackupImage:      options.BackupImage,
		MigrationImage:   options.MigrationImage,
		Owner:            owner,
		Logger:           logger,
	})
	if err != nil {
		store.Close()
		return nil, err
	}
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
	domain, err := controlplane.New(controlplane.Config{
		Dynamic:     dynamicClient,
		Extensions:  projection,
		Controllers: controllers,
		Catalogue:   catalogueService,
		Authorizer:  identity.Authorizer{Policy: &access.Policy{Objects: controlplane.NewClusterSource(dynamicClient), Extensions: projection}},
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
			Boundary:    terminalpkg.Boundary{NodeIsolated: options.TerminalNodeIsolated},
			IdleTimeout: terminalIdle,
		},
		Log: logger,
	})
	if err != nil {
		store.Close()
		return nil, err
	}

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
	accountsAPI := identity.NewAPI(accounts).Handler(shell)
	// The health endpoint is the probe surface: it answers before any session
	// exists, so it is served ahead of the account gate (KOSD-C-56, KOSD-C-57).
	root := http.NewServeMux()
	root.Handle("GET /api/v1/system/health", domain.Handler())
	// A terminal token is the only credential the attach and input routes
	// accept, so they sit ahead of the console gate (KOSD-C-91).
	root.Handle("POST /api/v1/terminal/sessions/{sessionId}/attach", domain.TerminalPublicHandler())
	root.Handle("POST /api/v1/terminal/sessions/{sessionId}/input", domain.TerminalPublicHandler())
	root.Handle("/", pagesAPI.RefuseWrites(accountsAPI))
	server := &http.Server{Addr: options.Address, Handler: root, ReadHeaderTimeout: 10 * time.Second}

	// One sample before serving keeps the probe answerable from the first
	// request instead of reporting an unmeasured process (KOSD-C-57).
	protection.Sample(ctx, memoryReader{})
	go sampleMemory(ctx, protection)
	// The install pass, the catalogue sync and every kernel reconcile loop are
	// the write path ADR-011 puts in this process. Without them nothing writes
	// an Application's status or its downstream workload.
	go loops.run(ctx)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	}()

	logger.Info("kos-control-plane started", "address", options.Address, "database", options.DatabasePath, "packages", options.PackagesPath)
	return &Process{Server: server, store: store, domain: domain}, nil
}

// Serve answers requests until the process is asked to stop.
func (p *Process) Serve() error {
	if err := p.Server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Close releases the process resources. It is safe to call more than once.
func (p *Process) Close() {
	p.close.Do(func() {
		p.domain.Close()
		p.store.Close()
	})
}

// firstOwnerName reads the global Owner's username. An installation that has
// not delivered its first Owner yet leaves the restore preflight without the
// Owner exemption, which is the conservative direction.
func firstOwnerName(ctx context.Context, store *identity.Store) string {
	owner, err := store.FirstOwner(ctx)
	if err != nil {
		return ""
	}
	return owner.Username
}

// clusterDefinitions reads the object definition manifests the release ships
// and reports the kinds that no longer serve the version the control plane
// requires (KOSD-C-16). The process either has a usable object model or refuses
// to start, never serving half of one (KOSD-C-56).
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

// ReadMemory implements degrade.MemoryReader.
func (memoryReader) ReadMemory(ctx context.Context) (int64, error) {
	return budget.ProcessMemory(ctx)
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

// processBanner exposes the process-wide degradation notice to the page shell
// (KOSD-C-56).
type processBanner struct {
	state *degrade.Static
}

// Banner implements panel.Banners.
func (b processBanner) Banner() *degrade.Notice { return b.state.State().Notice }

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
