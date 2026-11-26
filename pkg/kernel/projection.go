package kernel

import (
	"sync"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/write"
)

// Projection is the contribution projection every surface reads. One process
// outlives any single install pass, so the projection is replaced in place
// whenever a pass finishes (KOSD-C-28).
type Projection struct {
	mu       sync.RWMutex
	registry *extension.Registry
}

// NewProjection builds an empty projection: no component is active yet.
func NewProjection() *Projection { return &Projection{} }

// Publish replaces the projection with the one a pass produced; a nil registry
// is the projection of no active component.
func (p *Projection) Publish(registry *extension.Registry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.registry = registry
}

// Registry is the active projection, or nil before the first install pass.
func (p *Projection) Registry() *extension.Registry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.registry
}

// ObjectClass implements controlplane.ExtensionKinds.
func (p *Projection) ObjectClass(group, kind string) (controlplane.ExtensionObjectClass, bool) {
	return p.Registry().ObjectClass(group, kind)
}

// Settings implements settings.Declarations.
func (p *Projection) Settings() []extension.Setting { return p.Registry().Settings() }

// IsActive implements controlplane.TerminalExtensions: a component that has
// not reached the active set serves no terminal session (KOSD-F-16).
func (p *Projection) IsActive(identifier string) bool {
	return p.Registry().IsActive(identifier)
}

// Setting implements settings.Declarations.
func (p *Projection) Setting(id string) (extension.Setting, bool) {
	return p.Registry().Setting(id)
}

// Pages implements pages.Declarations.
func (p *Projection) Pages() []extension.Page { return p.Registry().Pages() }

// PageFor implements pages.Declarations.
func (p *Projection) PageFor(id string, role write.Role) (extension.Page, bool) {
	return p.Registry().PageFor(id, role)
}

// Apps implements apps.Declarations.
func (p *Projection) Apps() []extension.InstalledApp { return p.Registry().Apps() }

// CatalogueSources implements catalogue.Declarations.
func (p *Projection) CatalogueSources() []extension.CatalogueSource {
	return p.Registry().CatalogueSources()
}

// Capabilities implements catalogue.Declarations.
func (p *Projection) Capabilities() []extension.Capability {
	return p.Registry().Capabilities()
}

// PreferenceKeys implements identity.PreferenceSchemas.
func (p *Projection) PreferenceKeys(identifier string) ([]string, bool) {
	return p.Registry().PreferenceKeys(identifier)
}

// DeclaredBy implements access.ExtensionRoles.
func (p *Projection) DeclaredBy(group, kind string) (string, bool) {
	return p.Registry().DeclaredBy(group, kind)
}

// ObjectWriters implements access.ExtensionRoles.
func (p *Projection) ObjectWriters(group, kind string) ([]write.Role, bool) {
	return p.Registry().ObjectWriters(group, kind)
}
