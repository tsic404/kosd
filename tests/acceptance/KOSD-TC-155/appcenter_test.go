package kosdtc155

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/api/v1beta1"
	"github.com/tsic404/kosd/pkg/app"
	"github.com/tsic404/kosd/pkg/catalogue"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/identity"
	"github.com/tsic404/kosd/pkg/pages"
	"github.com/tsic404/kosd/pkg/write"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

const (
	appCenterIdentifier = "kos-appstore"
	appCenterPage       = "app-center"
	appCenterSource     = "kos-appstore"
	entriesItem         = "entries"
	entriesReference    = appCenterIdentifier + "/entries"
	entryID             = "web"
	entryDisplay        = "Web"
	installAction       = "install"
	installPath         = "/api/v1/resources/Application"
	otherComponent      = "notes"
)

// entriesTemplate renders the one object a definition written by the catalogue
// sync installs.
const entriesTemplate = `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
`

// otherProduct is the product that stays: one page and one window application
// with no catalogue contribution, so the case can see what the app center's
// uninstall touches (KOSD-C-53).
func otherProduct() extension.Declaration {
	return extension.Declaration{
		Identifier:       "acme.notes",
		Version:          "1.0.0",
		FrameworkVersion: ">=1.0 <2.0",
		Contributions: extension.Contributions{
			Pages: []extension.Page{{
				ID: "notes", Title: "Notes", Location: "panel",
				ReadOnly: true, ReadOnlyReason: "只读展示",
				Items: []extension.PageItem{{
					ID: "notes", Label: "Notes", Source: extension.PageSourceLocal,
					Store: "notes", Reason: "只读展示",
				}},
			}},
			Apps: []extension.App{{
				ID: "notes", Name: "Notes", Version: "1.0.0",
				Icon: "https://notes.example/icon.svg", Entry: "https://notes.example/",
				Window: extension.AppWindow{
					Width: 900, Height: 600, MinWidth: 480, MinHeight: 320,
					Resizable: true, Mode: extension.AppWindowSingle, DefaultState: extension.AppStateNormal,
				},
				Permissions: []string{},
			}},
		},
	}
}

// pageReader answers the page items the app center's declaration names from the
// host's catalogue projection. The entries are read through a function because
// the shell is built after its page configuration.
type pageReader struct {
	entries func() []catalogue.Entry
}

// Read implements pages.Readings. The value is read at request time, so the
// observation stamp is the read time (KOSD-C-13).
func (r pageReader) Read(ctx context.Context, _ identity.Session, item extension.PageItem) (pages.Reading, *controlplane.Error) {
	if item.Reference != entriesReference {
		return pages.Reading{}, &controlplane.Error{
			Code: controlplane.CodeCapabilityMissing, Stage: controlplane.StageCapability,
		}
	}
	return pages.Reading{Value: r.entries(), ObservedAt: time.Now()}, nil
}

// appCenterDeclaration is the product surface the kernel no longer ships: one
// catalogue source and the page over it (KOSD-C-24, KOSD-C-31).
func appCenterDeclaration(t *testing.T) extension.Declaration {
	t.Helper()
	definition := v1beta1.ApplicationDefinitionSpec{
		Package:  v1beta1.PackageIdentity{Version: "1.0.0"},
		Template: entriesTemplate,
	}
	digest, err := app.DefinitionDigest(definition)
	if err != nil {
		t.Fatalf("digest the definition: %v", err)
	}
	definition.Package.Digest = digest
	return extension.Declaration{
		Identifier:       appCenterIdentifier,
		Version:          "1.0.0",
		FrameworkVersion: ">=1.0 <2.0",
		Contributions: extension.Contributions{
			CatalogueSources: []extension.CatalogueSource{{
				ID: appCenterSource, Sync: "pull",
				Entries: []extension.CatalogueEntry{{
					ID:         entryID,
					Display:    extension.CatalogueDisplay{Name: entryDisplay},
					Definition: definition,
				}},
			}},
			Pages: []extension.Page{{
				ID: appCenterPage, Title: "应用中心", Location: "panel",
				Items: []extension.PageItem{{
					ID: entriesItem, Label: "可安装应用", Source: extension.PageSourceComputed,
					Reference: entriesReference, Action: installAction,
				}},
				Actions: []extension.Action{{
					ID: installAction, Method: http.MethodPost, Path: installPath,
				}},
			}},
		},
		Permissions: []extension.Permission{
			{Contribution: appCenterPage, RequiredRole: string(write.RoleMember)},
			{Contribution: appCenterSource, ReadScope: extension.ReadScope{Groups: []string{api.Group}}},
		},
	}
}

// startShell starts a shell whose page router serves the app center's items from
// the catalogue the shell owns (KOSD-C-22).
func startShell(t *testing.T) *harness.Shell {
	t.Helper()
	var shell *harness.Shell
	cluster := harness.Start(t)
	shell = cluster.StartShell(t, func(config *pages.Config) {
		config.Readings = pages.Router{appCenterIdentifier: pageReader{
			entries: func() []catalogue.Entry { return shell.Catalogue.Entries() },
		}}
	})
	return shell
}

// pageIDs returns the page identities the shell lists for one session.
func pageIDs(t *testing.T, shell *harness.Shell, token string) []string {
	t.Helper()
	status, body := shell.Call(t, http.MethodGet, "/api/v1/pages", nil, token)
	if status != http.StatusOK {
		t.Fatalf("list pages = %d %v", status, body)
	}
	ids := make([]string, 0, len(harness.Items(t, body)))
	for _, item := range harness.Items(t, body) {
		ids = append(ids, harness.String(item, "id"))
	}
	return ids
}

// TestTC155AppCenterAppearsOnlyWithItsComponent reads the shell with no
// component, installs the app center's declaration, reads its page and the
// synced entry, then uninstalls it and reads the shell again (KOSD-AC-72,
// KOSD-TC-155).
func TestTC155AppCenterAppearsOnlyWithItsComponent(t *testing.T) {
	shell := startShell(t)
	cluster := shell.Cluster
	owner := shell.SignInOwner(t, "owner", "owner-password")
	kindsBefore := append([]string(nil), api.Kinds()...)
	artifacts := extension.MemoryArtifacts{}

	if ids := pageIDs(t, shell, owner); len(ids) != 0 {
		t.Fatalf("pages before the install = %v, want none", ids)
	}
	if entries := shell.Catalogue.Entries(); len(entries) != 0 {
		t.Fatalf("catalogue entries before the install = %v, want none", entries)
	}

	installation := cluster.InstallPackageWith(t, "appstore", appCenterDeclaration(t), artifacts, shell.Retention())
	shell.Use(installation)
	shell.SyncCatalogue(t)

	if ids := pageIDs(t, shell, owner); len(ids) != 1 || ids[0] != appCenterPage {
		t.Fatalf("pages after the install = %v, want the app center", ids)
	}
	status, body := shell.Call(t, http.MethodGet, "/api/v1/pages/"+appCenterPage, nil, owner)
	if status != http.StatusOK {
		t.Fatalf("read the page = %d %v", status, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("page items = %v, want the entries item", items)
	}
	item, _ := items[0].(map[string]any)
	if harness.String(item, "id") != entriesItem || harness.String(item, "source") != extension.PageSourceComputed {
		t.Fatalf("item = %v, want the computed entries item", item)
	}
	entry, _ := item["entry"].(map[string]any)
	if harness.String(entry, "method") != http.MethodPost || harness.String(entry, "path") != installPath {
		t.Fatalf("entry = %v, want the install write", entry)
	}
	offered, _ := item["value"].([]any)
	if len(offered) != 1 {
		t.Fatalf("entries value = %v, want the synced entry", item["value"])
	}
	synced, _ := offered[0].(map[string]any)
	if harness.String(synced, "id") != entryID || harness.String(synced, "definition") != entryID {
		t.Fatalf("entry = %v, want the entry the source offers", synced)
	}

	cluster.DeleteComponent(t, "appstore")
	shell.Use(cluster.ExtensionState(t, artifacts))
	if ids := pageIDs(t, shell, owner); len(ids) != 0 {
		t.Fatalf("pages after the uninstall = %v, want none", ids)
	}
	if entries := shell.Catalogue.Entries(); len(entries) != 0 {
		t.Fatalf("catalogue entries after the uninstall = %v, want none", entries)
	}
	if !reflect.DeepEqual(api.Kinds(), kindsBefore) {
		t.Fatalf("the kernel kind set changed: %v", api.Kinds())
	}
}

// TestTC155UninstallLeavesOtherProductsIntact proves the uninstall is scoped:
// the other product keeps its page and window application (KOSD-TC-155).
func TestTC155UninstallLeavesOtherProductsIntact(t *testing.T) {
	shell := startShell(t)
	cluster := shell.Cluster
	shell.SignInOwner(t, "owner", "owner-password")
	artifacts := extension.MemoryArtifacts{}

	installation := cluster.InstallPackageWith(t, otherComponent, otherProduct(), artifacts, shell.Retention())
	shell.Use(installation)
	installation = cluster.InstallPackageWith(t, "appstore", appCenterDeclaration(t), artifacts, shell.Retention())
	shell.Use(installation)
	if apps := installation.Registry().Apps(); len(apps) != 1 || apps[0].Manifest.ID != "notes" {
		t.Fatalf("apps = %+v, want the other product's window", apps)
	}
	if pages := installation.Registry().Pages(); len(pages) != 2 {
		t.Fatalf("pages = %+v, want the app center next to the other product", pages)
	}

	cluster.DeleteComponent(t, "appstore")
	installation = cluster.ExtensionState(t, artifacts)
	shell.Use(installation)
	if pages := installation.Registry().Pages(); len(pages) != 1 || pages[0].ID != "notes" {
		t.Fatalf("pages after the uninstall = %+v, want the other product's page", pages)
	}
	if apps := installation.Registry().Apps(); len(apps) != 1 || apps[0].Manifest.ID != "notes" {
		t.Fatalf("apps after the uninstall = %+v, want the other product's window", apps)
	}
}

// kernelEntriesDeclaration declares the page item identity the kernel used to
// answer itself: the bare catalogue reference, with no product in front of it.
func kernelEntriesDeclaration() extension.Declaration {
	return extension.Declaration{
		Identifier: "acme.bare",
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			Pages: []extension.Page{{
				ID: appCenterPage, Title: "应用中心", Location: "panel",
				ReadOnly: true, ReadOnlyReason: "只读展示",
				Items: []extension.PageItem{{
					ID: entriesItem, Label: "可安装应用", Source: extension.PageSourceComputed,
					Reference: "catalogue/entries", Reason: "只读展示",
				}},
			}},
		},
	}
}

// TestTC155KernelServesNoAppCenterReader checks that the composed shell holds no
// reader of its own for the app center: an item naming the bare catalogue
// reference the removed kernel product answered is unreadable until the product
// that owns the reference is installed and routes it (KOSD-C-22, KOSD-C-53).
func TestTC155KernelServesNoAppCenterReader(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	owner := shell.SignInOwner(t, "owner", "owner-password")
	artifacts := extension.MemoryArtifacts{}

	installation := shell.Cluster.InstallPackageWith(t, "bare", kernelEntriesDeclaration(), artifacts, shell.Retention())
	shell.Use(installation)
	if ids := pageIDs(t, shell, owner); len(ids) != 1 {
		t.Fatalf("pages = %v, want the declared page", ids)
	}
	status, body := shell.Call(t, http.MethodGet, "/api/v1/pages/"+appCenterPage, nil, owner)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("read the page = %d %v, want the unreadable source", status, body)
	}
	if code := harness.String(body, "error", "code"); code != controlplane.CodeCapabilityMissing {
		t.Fatalf("code = %q, want %q", code, controlplane.CodeCapabilityMissing)
	}

}
