// Package kosdtc180 verifies the two declared writer-set forms of C-18 and
// C-59: a set declared empty lets no session role write the class, the global
// Owner included, an absent set takes C-59's default, the two forms stay apart
// after a reload and reverse when the declaration swaps them, and a third-party
// identifier changes nothing (KOSD-AC-80, KOSD-TC-180).
package kosdtc180

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/write"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

const (
	// group is the extension's own API group; the case declares two classes in
	// it so the comparison never depends on the kind's name.
	group = "records.example.test"
	// auditKind is the class whose writer set the round controls, and noteKind
	// the class that carries the other form.
	auditKind = "Audit"
	noteKind  = "Note"
)

// form is what one declared writer set must do when a session role writes it.
type form int

const (
	// declaredEmpty is the set that appears with no role: nobody writes.
	declaredEmpty form = iota
	// undeclared is the set that does not appear: C-59's default writes.
	undeclared
)

// declaration declares both classes and one controller over them. audit and
// note carry the two forms: nil omits objectWriters, an empty set carries it.
func declaration(identifier string, audit, note extension.RoleSet) extension.Declaration {
	permissions := make([]extension.Permission, 0, 2)
	for _, entry := range []struct {
		contribution string
		writers      extension.RoleSet
	}{{auditKind, audit}, {noteKind, note}} {
		if entry.writers == nil {
			continue
		}
		permissions = append(permissions, extension.Permission{
			Contribution:  entry.contribution,
			ObjectWriters: entry.writers,
		})
	}
	return extension.Declaration{
		Identifier: identifier,
		Version:    "1.0.0",
		APIGroup:   group,
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{
				{Group: group, Kind: auditKind, Resource: "audits", Versions: []string{"v1"}, Namespaced: true},
				{Group: group, Kind: noteKind, Resource: "notes", Versions: []string{"v1"}, Namespaced: true},
			},
			Controllers: []extension.Controller{{
				ID:      "records",
				Manages: []extension.GroupKind{{Group: group, Kind: auditKind}, {Group: group, Kind: noteKind}},
				Scope:   extension.WriteScope{Groups: []string{group}, Namespaces: []string{harness.Namespace}},
			}},
		},
		Permissions: permissions,
	}
}

// record builds one extension object body.
func record(kind, name string) map[string]any {
	return map[string]any{
		"apiVersion": group + "/v1",
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"spec":       map[string]any{"payload": name},
	}
}

// role is one session the case drives.
type role struct {
	name  string
	token string
}

// TestTC180EmptyAndAbsentWriterSets runs the three rounds of the case: the
// declared forms in one order, the same declaration with the forms swapped, and
// the first round again under a third-party identifier.
func TestTC180EmptyAndAbsentWriterSets(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, group, "v1", auditKind, "audits", true)
	cluster.InstallCRD(t, group, "v1", noteKind, "notes", true)
	artifacts := extension.MemoryArtifacts{}

	// Round one: Audit's set is declared empty and Note's is absent.
	installation := cluster.InstallPackage(t, "records", declaration("kos.example.records", extension.RoleSet{}, nil), artifacts)
	assertRound(t, cluster, installation, "records", "one", declaredEmpty, undeclared)

	// Round two: the same declaration with the forms swapped. Reloading must
	// keep them apart, so the outcomes reverse.
	installation = cluster.UpgradePackage(t, "records", declaration("kos.example.records", nil, extension.RoleSet{}), artifacts)
	assertRound(t, cluster, installation, "records", "two", undeclared, declaredEmpty)

	// Round three: the same declarations under a third-party identifier and
	// product name. The judgement does not look at either.
	cluster.DeleteComponent(t, "records")
	installation = cluster.InstallPackage(t, "third-party", declaration("thirdparty.records", extension.RoleSet{}, nil), artifacts)
	assertRound(t, cluster, installation, "third-party", "three", declaredEmpty, undeclared)
}

// assertRound drives the role matrix of one loaded declaration. audit and note
// are the outcomes the two classes must produce in this round.
func assertRound(t *testing.T, cluster *harness.Cluster, installation *extension.Installation, component, label string, audit, note form) {
	t.Helper()
	registry := installation.Registry()
	system := cluster.StartAccess(t, registry, func(config *controlplane.Config) {
		config.Extensions = registry
	})
	owner := system.Owner(t, "owner", "owner-password")
	member := system.Account(t, owner, "member", "member-password", write.RoleMember)
	guest := system.Account(t, owner, "guest", "guest-password", write.RoleGuest)
	// The Owner writes last, so the record a refused Member or Guest rewrite
	// targets is still stored when they try (C-59, C-60).
	roles := []role{{"Guest", guest}, {"Member", member}, {"Owner", owner}}

	result, found := installation.Component(component)
	if !found || len(result.Contributions.Controllers) == 0 {
		t.Fatalf("component %s carries no controller: %+v", component, result)
	}
	controller := cluster.Writer()
	controller.Resources[schema.GroupKind{Group: group, Kind: auditKind}] = "audits"
	controller.Resources[schema.GroupKind{Group: group, Kind: noteKind}] = "notes"
	controller.Scope = result.Contributions.Controllers[0].Scope.Admit()

	// The controller writes both classes: its path is C-20's and stays open
	// whatever the declared writer set says.
	for _, kind := range []string{auditKind, noteKind} {
		name := controllerName(label, kind)
		object := &unstructured.Unstructured{Object: record(kind, name)}
		if _, err := controller.Create(context.Background(), write.SubjectExtensionReconciler, write.Session{}, object); err != nil {
			t.Fatalf("round %s: the extension controller writes %s: %v", label, kind, err)
		}
	}
	for _, kind := range []string{auditKind, noteKind} {
		plural := map[string]string{auditKind: "audits", noteKind: "notes"}[kind]
		stored, err := cluster.ExtensionResource(group, "v1", plural, harness.Namespace).
			Get(t.Context(), controllerName(label, kind), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("round %s: the controller's %s record is unreadable: %v", label, kind, err)
		}
		if harness.String(stored.Object, "spec", "payload") == "" {
			t.Fatalf("round %s: the controller's %s record carries no spec: %v", label, kind, stored.Object)
		}
	}

	// The role matrix of both classes.
	judge(t, cluster, system, roles, auditKind, "audits", audit, label)
	judge(t, cluster, system, roles, noteKind, "notes", note, label)
}

// judge drives create, rewrite and delete of one class as every role and
// compares the outcome with the declared form.
func judge(t *testing.T, cluster *harness.Cluster, system *harness.Access, roles []role, kind, plural string, want form, label string) {
	t.Helper()
	path := "/api/v1/resources/" + kind + "?namespace=" + harness.Namespace
	seeded := controllerName(label, kind)
	recordPath := "/api/v1/resources/" + kind + "/" + seeded + "?namespace=" + harness.Namespace
	// A delete carries no body, so the API group comes from the query.
	deletePath := recordPath + "&apiVersion=" + group + "/v1"

	for _, role := range roles {
		status, body := system.Call(t, role.token, http.MethodPost, path,
			record(kind, fmt.Sprintf("%s-%s-%s", strings.ToLower(role.name), strings.ToLower(kind), label)), nil)
		expectWrite(t, want, role, kind, "create", status, body)

		stored, err := cluster.ExtensionResource(group, "v1", plural, harness.Namespace).
			Get(t.Context(), seeded, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("round %s: read %s before %s's rewrite: %v", label, seeded, role.name, err)
		}
		status, body = system.Call(t, role.token, http.MethodPut, recordPath,
			record(kind, seeded), map[string]string{"If-Match": stored.GetResourceVersion()})
		expectWrite(t, want, role, kind, "rewrite", status, body)

		status, body = system.Call(t, role.token, http.MethodDelete, deletePath, nil,
			map[string]string{"If-Match": stored.GetResourceVersion()})
		expectWrite(t, want, role, kind, "delete", status, body)
	}
}

// controllerName is the record name the extension controller writes and the
// user writes target; it is a DNS label because the API server refuses others.
func controllerName(label, kind string) string {
	return fmt.Sprintf("controller-%s-%s", label, strings.ToLower(kind))
}

// expectWrite checks one write against the class's declared form. A declared
// empty set refuses every role with a permission denial; an absent set admits
// the global Owner and refuses the rest, which is C-59's default.
func expectWrite(t *testing.T, want form, role role, kind, verb string, status int, body map[string]any) {
	t.Helper()
	admitted := want == undeclared && role.name == "Owner"
	if admitted {
		if status != http.StatusAccepted {
			t.Fatalf("%s %s of %s = %d %v, want 202", role.name, verb, kind, status, body)
		}
		return
	}
	if status != http.StatusForbidden {
		t.Fatalf("%s %s of %s = %d %v, want 403", role.name, verb, kind, status, body)
	}
	if code := harness.String(body, "error", "code"); code != "permission_denied" {
		t.Fatalf("%s %s of %s carries code %q, want permission_denied", role.name, verb, kind, code)
	}
}
