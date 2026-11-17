package extension

import (
	"fmt"
	"sort"

	"github.com/tsic404/kosd/pkg/access"
	"github.com/tsic404/kosd/pkg/apptoken"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/write"
)

// Registry is the visibility projection of the active components (C-28): it
// answers what is served and what is visible, and never shows a contribution a
// component did not declare.
type Registry struct {
	classes      map[GroupKind]ObjectClass
	objectOrder  []GroupKind
	controllers  []Controller
	capabilities []Capability
	pages        []Page
	settings     []Setting
	sources      []CatalogueSource
	discovery    []DiscoveryAdapter
	diagnostics  []DiagnosticAdapter
	preferences  []PreferenceSchema
	// apps are the window applications the active components declare (C-18, KOSD-C-73).
	apps []InstalledApp
	// preferenceKeys maps an extension identifier to the keys it declared
	// (C-27).
	preferenceKeys map[string][]string
	// identifiers are the active extension identifiers, so a surface that
	// acts for one component can refuse an uninstalled identifier (KOSD-F-16).
	identifiers map[string]bool
	declaredBy  map[GroupKind]string
	writers     map[GroupKind][]write.Role
	// reads maps an extension identifier to the read permissions it declares
	// (KOSD-C-85).
	reads map[string][]access.DeclaredRead
	// appOrigins maps an extension identifier and application to the entry
	// origin the application declares (KOSD-C-87).
	appOrigins map[string]map[string]string
}

// InstalledApp is one window application an active component declares, with the
// component and the verified package the desktop needs to open it (C-18,
// KOSD-C-73). The extension identifier is what the asset address and the deep
// link resolve against.
type InstalledApp struct {
	Extension string `json:"extension"`
	Component string `json:"component"`
	Phase     string `json:"phase"`
	Digest    string `json:"digest,omitempty"`
	Manifest  App    `json:"manifest"`
}

// Apps returns every installed window application. A component that is not
// active, or one whose application class failed to load, contributes none.
func (r *Registry) Apps() []InstalledApp {
	if r == nil {
		return []InstalledApp{}
	}
	return append([]InstalledApp{}, r.apps...)
}

// AppsOf returns the window applications one extension identifier declares. An
// uninstalled identifier has no applications, never another extension's.
func (r *Registry) AppsOf(identifier string) []InstalledApp {
	if r == nil {
		return nil
	}
	apps := make([]InstalledApp, 0, len(r.apps))
	for _, app := range r.apps {
		if app.Extension == identifier {
			apps = append(apps, app)
		}
	}
	return apps
}

// installedApps projects one entry per declared application, carrying the
// declaring extension, the installed component, and the phase and digest the
// desktop judges it by (C-18, C-73). Contributions already passed the class
// loaders in buildRegistry, so an invalid application never reaches here.
func installedApps(active []activated, effective map[string]Contributions, outcomes map[string]*ComponentResult) []InstalledApp {
	apps := make([]InstalledApp, 0, len(active))
	for _, component := range active {
		result, ok := outcomes[component.name]
		if !ok {
			continue
		}
		for _, app := range effective[component.name].Apps {
			apps = append(apps, InstalledApp{
				Extension: component.loaded.Identifier,
				Component: component.name,
				Phase:     result.Phase,
				Digest:    result.Digest,
				Manifest:  app,
			})
		}
	}
	sort.SliceStable(apps, func(left, right int) bool {
		if apps[left].Extension != apps[right].Extension {
			return apps[left].Extension < apps[right].Extension
		}
		return apps[left].Manifest.ID < apps[right].Manifest.ID
	})
	return apps
}

// DeclaredBy names the extension that declares one served class (C-14). It is
// the identifier an audit record carries when the class is written.
func (r *Registry) DeclaredBy(group, kind string) (string, bool) {
	if r == nil {
		return "", false
	}
	identifier, ok := r.declaredBy[GroupKind{Group: group, Kind: kind}]
	return identifier, ok
}

// ObjectWriters returns the session roles an extension declared for writing its
// object class (C-18, C-59). ok is false when no permission entry declares the
// class's writer set, and the class then takes C-59's default: the global Owner
// writes it. ok is true with no roles when the declaration carries the set
// empty, and no session role writes the class, the global Owner included.
func (r *Registry) ObjectWriters(group, kind string) ([]write.Role, bool) {
	if r == nil {
		return nil, false
	}
	writers, ok := r.writers[GroupKind{Group: group, Kind: kind}]
	if !ok {
		return nil, false
	}
	declared := make([]write.Role, len(writers))
	copy(declared, writers)
	return declared, true
}

// ReadScopes returns the read permissions one extension currently declares
// (KOSD-C-85). An identifier that declares none reads nothing.
func (r *Registry) ReadScopes(identifier string) []access.DeclaredRead {
	if r == nil {
		return nil
	}
	reads := r.reads[identifier]
	out := make([]access.DeclaredRead, 0, len(reads))
	for _, read := range reads {
		out = append(out, access.DeclaredRead{
			Grants:       append([]apptoken.Grant(nil), read.Grants...),
			RequiredRole: read.RequiredRole,
		})
	}
	return out
}

// AppOrigin returns the entry origin one installed application declares
// (KOSD-C-87). An uninstalled application has none.
func (r *Registry) AppOrigin(identifier, appID string) (string, bool) {
	if r == nil {
		return "", false
	}
	origin, ok := r.appOrigins[identifier][appID]
	return origin, ok
}

// ObjectClass resolves one served extension class (C-19).
func (r *Registry) ObjectClass(group, kind string) (controlplane.ExtensionObjectClass, bool) {
	if r == nil {
		return controlplane.ExtensionObjectClass{}, false
	}
	class, ok := r.classes[GroupKind{Group: group, Kind: kind}]
	if !ok {
		return controlplane.ExtensionObjectClass{}, false
	}
	return controlplane.ExtensionObjectClass{
		Versions:   append([]string(nil), class.Versions...),
		Resource:   class.Resource,
		Namespaced: class.Namespaced,
	}, true
}

// IsServing reports whether an extension object class is served now.
func (r *Registry) IsServing(group, kind string) bool {
	_, ok := r.ObjectClass(group, kind)
	return ok
}

// IsActive reports whether one extension identifier belongs to an active
// component. An identifier no active declaration carries is not active.
func (r *Registry) IsActive(identifier string) bool {
	if r == nil || identifier == "" {
		return false
	}
	return r.identifiers[identifier]
}

// ObjectClasses returns the served extension classes in declaration order.
func (r *Registry) ObjectClasses() []ObjectClass {
	if r == nil {
		return nil
	}
	classes := make([]ObjectClass, 0, len(r.objectOrder))
	for _, key := range r.objectOrder {
		classes = append(classes, r.classes[key])
	}
	return classes
}

// Controllers returns the loaded controller contributions.
func (r *Registry) Controllers() []Controller {
	if r == nil {
		return nil
	}
	return append([]Controller(nil), r.controllers...)
}

// ExtensionCapabilities returns the capabilities extensions contribute.
func (r *Registry) ExtensionCapabilities() []Capability {
	if r == nil {
		return nil
	}
	return append([]Capability(nil), r.capabilities...)
}

// Capabilities returns the capability catalogue: the eight kernel baseline
// capabilities followed by the extension contributions (C-09, C-21).
func (r *Registry) Capabilities() []Capability {
	capabilities := BaselineCapabilities()
	if r == nil {
		return capabilities
	}
	return append(capabilities, r.capabilities...)
}

// Capability resolves one catalogue entry by name and version.
func (r *Registry) Capability(name, version string) (Capability, bool) {
	if r == nil {
		return Capability{}, false
	}
	if capability, ok := findCapability(BaselineCapabilities(), name, version); ok {
		return capability, true
	}
	return findCapability(r.capabilities, name, version)
}

// Pages returns the loaded pages.
func (r *Registry) Pages() []Page {
	if r == nil {
		return nil
	}
	return append([]Page(nil), r.pages...)
}

// PageFor returns a page a session role may see; an unknown page and a page the
// role may not see are both absent (C-59).
func (r *Registry) PageFor(id string, role write.Role) (Page, bool) {
	if r == nil {
		return Page{}, false
	}
	for _, page := range r.pages {
		if page.ID == id && page.Allows(role) {
			return page, true
		}
	}
	return Page{}, false
}

// Settings returns the loaded setting items.
func (r *Registry) Settings() []Setting {
	if r == nil {
		return nil
	}
	return append([]Setting(nil), r.settings...)
}

// Setting resolves one setting item.
func (r *Registry) Setting(id string) (Setting, bool) {
	if r == nil {
		return Setting{}, false
	}
	for _, setting := range r.settings {
		if setting.ID == id {
			return setting, true
		}
	}
	return Setting{}, false
}

// CatalogueSources returns the loaded catalogue sources.
func (r *Registry) CatalogueSources() []CatalogueSource {
	if r == nil {
		return nil
	}
	return append([]CatalogueSource(nil), r.sources...)
}

// PreferenceKeys returns the preference keys one extension declared (C-27). The
// second result is false when no active component declares that identifier, so
// a request for a withdrawn extension is refused rather than served empty.
func (r *Registry) PreferenceKeys(identifier string) ([]string, bool) {
	if r == nil {
		return nil, false
	}
	keys, ok := r.preferenceKeys[identifier]
	if !ok {
		return nil, false
	}
	return append([]string(nil), keys...), true
}

// DiscoveryAdapters returns the loaded discovery adapters.
func (r *Registry) DiscoveryAdapters() []DiscoveryAdapter {
	if r == nil {
		return nil
	}
	return append([]DiscoveryAdapter(nil), r.discovery...)
}

// DiagnosticAdapters returns the loaded diagnostic adapters.
func (r *Registry) DiagnosticAdapters() []DiagnosticAdapter {
	if r == nil {
		return nil
	}
	return append([]DiagnosticAdapter(nil), r.diagnostics...)
}

// Preferences returns the loaded preference schemas.
func (r *Registry) Preferences() []PreferenceSchema {
	if r == nil {
		return nil
	}
	return append([]PreferenceSchema(nil), r.preferences...)
}

// activated is one component that passed the dependency gate.
type activated struct {
	name             string
	loaded           Loaded
	disableBlockedBy []string
}

// buildRegistry merges the activated contributions. A class two components both
// declare stays with the earlier one; the later component keeps its other
// contributions and loses that class (C-19, C-20). A catalogue entry is an
// identity too: the first declaration keeps it, and a later claim is a
// contribution conflict rather than a second writer of the same cluster-scoped
// definition (C-24). The returned map is the effective contribution set of each
// component after conflicts.
func buildRegistry(components []activated) (*Registry, map[string]Contributions, map[string][]ClassProblem) {
	registry := &Registry{
		classes:        map[GroupKind]ObjectClass{},
		preferenceKeys: map[string][]string{},
		identifiers:    map[string]bool{},
		declaredBy:     map[GroupKind]string{},
		writers:        map[GroupKind][]write.Role{},
		reads:          map[string][]access.DeclaredRead{},
		appOrigins:     map[string]map[string]string{},
	}
	conflicts := map[string][]ClassProblem{}
	effective := make(map[string]Contributions, len(components))
	serving := map[GroupKind]string{}

	for _, component := range components {
		entry := component.loaded.Contributions
		if component.loaded.Identifier != "" {
			registry.identifiers[component.loaded.Identifier] = true
		}
		kept := make([]ObjectClass, 0, len(entry.ObjectClasses))
		for _, class := range entry.ObjectClasses {
			key := class.GroupKind()
			if owner, taken := serving[key]; taken {
				recordConflict(conflicts, component.name, ClassObjectClasses,
					fmt.Sprintf("class %s is already served by %s", key.String(), owner))
				continue
			}
			serving[key] = component.name
			registry.classes[key] = class
			registry.declaredBy[key] = component.loaded.Identifier
			registry.objectOrder = append(registry.objectOrder, key)
			kept = append(kept, class)
		}
		// A permission entry names the contribution it governs: the class kind,
		// or the group-qualified form. Only classes this component actually
		// serves take its writer list (C-18, C-59). An entry that carries the
		// set empty still declares the class: it registers no writers, which is
		// not the same as leaving the class to C-59's default.
		//
		// One class's writers are declared once. A second entry for the same
		// class would otherwise overwrite the first, and the two forms would
		// then differ by entry order instead of by declaration (C-18, C-59).
		declaredWriters := map[GroupKind]bool{}
		for _, permission := range component.loaded.Permissions {
			if permission.ObjectWriters == nil {
				continue
			}
			for _, class := range kept {
				key := class.GroupKind()
				if class.Kind != permission.Contribution && key.String() != permission.Contribution {
					continue
				}
				if declaredWriters[key] {
					recordConflict(conflicts, component.name, ClassObjectClasses,
						fmt.Sprintf("class %s declares its object writers twice", key.String()))
					continue
				}
				declaredWriters[key] = true
				writers := make([]write.Role, len(permission.ObjectWriters))
				copy(writers, permission.ObjectWriters)
				registry.writers[key] = writers
			}
		}
		entry.ObjectClasses = kept
		effective[component.name] = entry
	}

	managed := map[GroupKind]string{}
	for _, component := range components {
		entry := effective[component.name]
		kept := make([]Controller, 0, len(entry.Controllers))
		for _, controller := range entry.Controllers {
			// Judge every class before taking any of them: a controller rejected
			// for one class must leave no placeholder behind, or the next
			// component's controller for another class is refused for nothing.
			var problems []string
			for _, class := range controller.Manages {
				if _, served := registry.classes[class]; !served {
					problems = append(problems, fmt.Sprintf("controller %s manages class %s that is not served", controller.ID, class.String()))
					continue
				}
				if owner, taken := managed[class]; taken && owner != component.name {
					problems = append(problems, fmt.Sprintf("class %s already has a controller", class.String()))
				}
			}
			if len(problems) > 0 {
				for _, problem := range problems {
					recordConflict(conflicts, component.name, ClassControllers, problem)
				}
				continue
			}
			for _, class := range controller.Manages {
				managed[class] = component.name
			}
			registry.controllers = append(registry.controllers, controller)
			kept = append(kept, controller)
		}
		entry.Controllers = kept
		effective[component.name] = entry
	}

	servingEntries := map[string]string{}
	for _, component := range components {
		entry := effective[component.name]
		keptSources := make([]CatalogueSource, 0, len(entry.CatalogueSources))
		for _, source := range entry.CatalogueSources {
			keptEntries := make([]CatalogueEntry, 0, len(source.Entries))
			for _, offer := range source.Entries {
				if owner, taken := servingEntries[offer.ID]; taken {
					recordConflict(conflicts, component.name, ClassCatalogueSources,
						fmt.Sprintf("entry %s of source %s is already offered by source %s", offer.ID, source.ID, owner))
					continue
				}
				servingEntries[offer.ID] = source.ID
				keptEntries = append(keptEntries, offer)
			}
			source.Entries = keptEntries
			keptSources = append(keptSources, source)
		}
		entry.CatalogueSources = keptSources
		effective[component.name] = entry
		registry.capabilities = append(registry.capabilities, entry.Capabilities.Items...)
		registry.pages = append(registry.pages, entry.Pages...)
		registry.settings = append(registry.settings, entry.Settings...)
		registry.sources = append(registry.sources, keptSources...)
		for _, schema := range entry.Preferences {
			for _, key := range schema.Keys {
				registry.preferenceKeys[component.loaded.Identifier] = appendUnique(registry.preferenceKeys[component.loaded.Identifier], key)
			}
		}
		registry.preferences = append(registry.preferences, entry.Preferences...)
		registry.discovery = append(registry.discovery, entry.DiscoveryAdapters...)
		registry.diagnostics = append(registry.diagnostics, entry.DiagnosticAdapters...)
		registry.reads[component.loaded.Identifier] = declaredReads(component.loaded.Permissions)
		for _, app := range entry.Apps {
			origin := entryOrigin(app.Entry)
			if origin == "" {
				continue
			}
			if registry.appOrigins[component.loaded.Identifier] == nil {
				registry.appOrigins[component.loaded.Identifier] = map[string]string{}
			}
			registry.appOrigins[component.loaded.Identifier][app.ID] = origin
		}
	}
	return registry, effective, conflicts
}

// declaredReads renders the read permissions of one declaration as the scopes
// the control plane enforces for application tokens (KOSD-C-85).
func declaredReads(permissions []Permission) []access.DeclaredRead {
	reads := make([]access.DeclaredRead, 0, len(permissions))
	for _, permission := range permissions {
		grants := make([]apptoken.Grant, 0, len(permission.ReadScope.Groups)+len(permission.ReadScope.Kinds))
		for _, group := range permission.ReadScope.Groups {
			grants = append(grants, apptoken.Grant{Group: group})
		}
		for _, kind := range permission.ReadScope.Kinds {
			grants = append(grants, apptoken.Grant{Group: kind.Group, Kind: kind.Kind})
		}
		if len(grants) == 0 {
			continue
		}
		reads = append(reads, access.DeclaredRead{
			Grants:       grants,
			RequiredRole: write.Role(permission.RequiredRole),
		})
	}
	return reads
}

// entryOrigin reduces an application entry address to the origin a browser
// request must carry (KOSD-C-87). It uses the browser's serialization, so an
// explicitly written default port matches the Origin a browser sends. An
// address without a usable origin declares none.
func entryOrigin(entry string) string {
	return apptoken.NormalizeOrigin(entry)
}

// recordConflict appends one item to a component's class problem.
func recordConflict(conflicts map[string][]ClassProblem, component, class, item string) {
	for index := range conflicts[component] {
		if conflicts[component][index].Class == class {
			conflicts[component][index].Items = append(conflicts[component][index].Items, item)
			return
		}
	}
	conflicts[component] = append(conflicts[component], ClassProblem{Class: class, Items: []string{item}})
}

// appendUnique adds one item unless it is already present.
func appendUnique(items []string, item string) []string {
	for _, existing := range items {
		if existing == item {
			return items
		}
	}
	return append(items, item)
}

// findCapability resolves one capability entry.
func findCapability(capabilities []Capability, name, version string) (Capability, bool) {
	for _, capability := range capabilities {
		if capability.Name == name && capability.Version == version {
			return capability, true
		}
	}
	return Capability{}, false
}
