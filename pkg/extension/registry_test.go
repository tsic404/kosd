package extension

import (
	"testing"
	"time"

	"github.com/tsic404/kosd/pkg/write"
)

// TestRegistryShowsOnlyActivatedContributions checks that the projection answers
// from the declaration and nothing else (C-18, C-28).
func TestRegistryShowsOnlyActivatedContributions(t *testing.T) {
	declaration := Declaration{
		Identifier: "demo.widgets",
		Version:    "1.0.0",
		APIGroup:   "widgets.example.test",
		Contributions: Contributions{
			ObjectClasses: []ObjectClass{{
				Group: "widgets.example.test", Kind: "Widget", Resource: "widgets", Versions: []string{"v1"},
			}},
			Pages: []Page{{
				ID: "widgets", Title: "Widgets", Location: "panel",
				Items: []PageItem{{ID: "widgets", Label: "Widgets", Source: PageSourceLocal, Store: "widgets", Reason: "只读展示"}},
			}},
			Settings: []Setting{{ID: "widgets-endpoint", Scope: SettingScopeCluster, Writer: SettingWriterUser}},
		},
	}
	assessment := Evaluate([]Request{packageRequest(t, "widgets", declaration, time.Now())})
	registry := assessment.Registry

	if !registry.IsServing("widgets.example.test", "Widget") {
		t.Fatal("the declared class is not served")
	}
	if registry.IsServing("widgets.example.test", "Gadget") {
		t.Fatal("an undeclared class is served")
	}
	if _, found := registry.Setting("widgets-endpoint"); !found {
		t.Fatal("the declared setting is not visible")
	}
	if _, found := registry.Setting("undeclared"); found {
		t.Fatal("an undeclared setting is visible")
	}
	if _, found := registry.Capability("demo.widgets/undeclared", "v1"); found {
		t.Fatal("an undeclared capability is visible")
	}
}

// TestRegistryCapabilitiesKeepTheBaseline checks that extension capabilities are
// appended to the eight baseline names, not mixed into them (KOSD-TC-2).
func TestRegistryCapabilitiesKeepTheBaseline(t *testing.T) {
	declaration := fullDeclaration("demo.widgets")
	assessment := Evaluate([]Request{packageRequest(t, "widgets", declaration, time.Now())})
	capabilities := assessment.Registry.Capabilities()
	if len(capabilities) != len(BaselineCapabilities())+1 {
		t.Fatalf("catalogue = %v", capabilities)
	}
	for index, baseline := range BaselineCapabilities() {
		if capabilities[index].Name != baseline.Name || capabilities[index].Version != baseline.Version {
			t.Fatalf("baseline entry %d = %v, want %v", index, capabilities[index], baseline)
		}
	}
	last := capabilities[len(capabilities)-1]
	if last.Name != "demo.widgets/widgets" || last.Version != "v1" {
		t.Fatalf("extension capability = %v", last)
	}
	if _, found := assessment.Registry.Capability("demo.widgets/widgets", "v1"); !found {
		t.Fatal("the extension capability is not resolvable")
	}
}

// TestPageVisibilityFollowsTheSessionRole checks C-59's page judgement.
func TestPageVisibilityFollowsTheSessionRole(t *testing.T) {
	declaration := Declaration{
		Identifier: "demo.pages",
		Version:    "1.0.0",
		Contributions: Contributions{
			Pages: []Page{
				{ID: "everyone", Title: "Everyone", Location: "panel",
					Items: []PageItem{{ID: "value", Label: "Value", Source: PageSourceLocal, Store: "everyone", Reason: "只读展示"}}},
				{ID: "members", Title: "Members", Location: "panel", RequiredRole: string(write.RoleMember),
					Items: []PageItem{{ID: "value", Label: "Value", Source: PageSourceLocal, Store: "members", Reason: "只读展示"}}},
				{ID: "owners", Title: "Owners", Location: "panel", RequiredRole: string(write.RoleOwner),
					Items: []PageItem{{ID: "value", Label: "Value", Source: PageSourceLocal, Store: "owners", Reason: "只读展示"}}},
			},
		},
	}
	assessment := Evaluate([]Request{packageRequest(t, "pages", declaration, time.Now())})
	registry := assessment.Registry

	cases := []struct {
		role    write.Role
		visible []string
	}{
		{role: write.RoleGuest, visible: []string{"everyone"}},
		{role: write.RoleMember, visible: []string{"everyone", "members"}},
		{role: write.RoleOwner, visible: []string{"everyone", "members", "owners"}},
		{role: "", visible: nil},
	}
	for _, testCase := range cases {
		for _, id := range []string{"everyone", "members", "owners"} {
			_, found := registry.PageFor(id, testCase.role)
			want := containsString(testCase.visible, id)
			if found != want {
				t.Errorf("role %q sees %s = %v, want %v", testCase.role, id, found, want)
			}
		}
	}
}

// TestRejectedControllerLeavesNoReservation checks that a controller refused
// for a class it cannot take reserves nothing for the classes it does name:
// another component's controller for one of them must still be accepted
// (C-20).
func TestRejectedControllerLeavesNoReservation(t *testing.T) {
	group := "demo.example.test"
	alpha := ObjectClass{Group: group, Kind: "Alpha", Resource: "alphas", Versions: []string{"v1"}}
	beta := ObjectClass{Group: group, Kind: "Beta", Resource: "betas", Versions: []string{"v1"}}

	// holder serves Beta and controls it.
	holder := activated{name: "holder", loaded: Loaded{Contributions: Contributions{
		ObjectClasses: []ObjectClass{beta},
		Controllers:   []Controller{{ID: "holder", Manages: []GroupKind{beta.GroupKind()}}},
	}}}
	// split declares Alpha and Beta but loses Beta to holder; its controller
	// names both, so the whole controller is refused after Alpha was reserved.
	split := activated{name: "split", loaded: Loaded{Contributions: Contributions{
		ObjectClasses: []ObjectClass{alpha, beta},
		Controllers:   []Controller{{ID: "split", Manages: []GroupKind{alpha.GroupKind(), beta.GroupKind()}}},
	}}}
	// contender loses Alpha as a class but its controller only names Alpha,
	// which no accepted controller has taken.
	contender := activated{name: "contender", loaded: Loaded{Contributions: Contributions{
		ObjectClasses: []ObjectClass{alpha},
		Controllers:   []Controller{{ID: "contender", Manages: []GroupKind{alpha.GroupKind()}}},
	}}}

	registry, _, conflicts := buildRegistry([]activated{holder, split, contender})

	for _, problem := range conflicts["contender"] {
		if problem.Class == ClassControllers {
			t.Fatalf("contender's controller was refused by a reservation left behind: %v", problem.Items)
		}
	}
	accepted := make([]string, 0, len(registry.Controllers()))
	for _, controller := range registry.Controllers() {
		accepted = append(accepted, controller.ID)
	}
	if !containsString(accepted, "contender") {
		t.Fatalf("controllers = %v, want contender's controller", accepted)
	}
	if containsString(accepted, "split") {
		t.Fatalf("controllers = %v, want the refused split controller absent", accepted)
	}
}

// containsString reports whether values holds want.
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestWriterSetKeepsEmptyAndAbsentApart checks the two declared forms of C-18
// through the artifact, loading and the registry: a writer set declared empty
// registers the class with no roles, while an absent set leaves the class to
// C-59's default (KOSD-AC-80).
func TestWriterSetKeepsEmptyAndAbsentApart(t *testing.T) {
	const group = "records.example.test"
	declaration := Declaration{
		Identifier: "demo.records",
		Version:    "1.0.0",
		APIGroup:   group,
		Contributions: Contributions{
			ObjectClasses: []ObjectClass{
				{Group: group, Kind: "Audit", Resource: "audits", Versions: []string{"v1"}, Namespaced: true},
				{Group: group, Kind: "Note", Resource: "notes", Versions: []string{"v1"}, Namespaced: true},
			},
		},
		Permissions: []Permission{
			{Contribution: "Audit", ObjectWriters: RoleSet{}},
		},
	}

	// The artifact keeps the declared empty set: an empty array is not the same
	// wire form as an absent key.
	content, _, err := Pack(declaration, nil)
	if err != nil {
		t.Fatalf("pack declaration: %v", err)
	}
	artifact, err := Read(content)
	if err != nil {
		t.Fatalf("read package: %v", err)
	}
	if artifact.Declaration.Permissions[0].ObjectWriters == nil {
		t.Fatal("packing turned the declared empty writer set into an absent one")
	}

	registry := Evaluate([]Request{packageRequest(t, "records", artifact.Declaration, time.Now())}).Registry
	writers, declared := registry.ObjectWriters(group, "Audit")
	if !declared {
		t.Fatal("the class with an explicitly empty writer set was not registered")
	}
	if len(writers) != 0 {
		t.Fatalf("writers of the declared empty set = %v, want none", writers)
	}
	if _, declared := registry.ObjectWriters(group, "Note"); declared {
		t.Fatal("a class with no writer declaration was registered as declared")
	}
}

// TestDuplicateWriterDeclarationIsRefused checks that a second writer
// declaration for one class is refused instead of overwriting the first: the
// declared empty set stays the class's writer set, the later non-empty entry
// does not restore Owner writes, and the conflict names the class (C-18, C-59).
func TestDuplicateWriterDeclarationIsRefused(t *testing.T) {
	const group = "records.example.test"
	class := ObjectClass{Group: group, Kind: "Audit", Resource: "audits", Versions: []string{"v1"}, Namespaced: true}
	wantProblem := "class " + class.GroupKind().String() + " declares its object writers twice"

	cases := []struct {
		name        string
		permissions []Permission
		wantWriters []write.Role
	}{
		{
			name: "declared empty first",
			permissions: []Permission{
				{Contribution: "Audit", ObjectWriters: RoleSet{}},
				{Contribution: "Audit", ObjectWriters: RoleSet{write.RoleOwner}},
			},
			wantWriters: []write.Role{},
		},
		{
			name: "declared non-empty first",
			permissions: []Permission{
				{Contribution: "Audit", ObjectWriters: RoleSet{write.RoleOwner}},
				{Contribution: "Audit", ObjectWriters: RoleSet{}},
			},
			wantWriters: []write.Role{write.RoleOwner},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			component := activated{name: "demo.records", loaded: Loaded{
				Contributions: Contributions{ObjectClasses: []ObjectClass{class}},
				Permissions:   testCase.permissions,
			}}
			registry, _, conflicts := buildRegistry([]activated{component})

			reported := false
			for _, problem := range conflicts[component.name] {
				for _, item := range problem.Items {
					if problem.Class == ClassObjectClasses && item == wantProblem {
						reported = true
					}
				}
			}
			if !reported {
				t.Fatalf("conflicts = %v, want %q", conflicts[component.name], wantProblem)
			}

			writers, declared := registry.ObjectWriters(group, "Audit")
			if !declared {
				t.Fatal("the class stopped being declared by the duplicate")
			}
			if len(writers) != len(testCase.wantWriters) {
				t.Fatalf("writers = %v, want %v", writers, testCase.wantWriters)
			}
			for index, role := range writers {
				if role != testCase.wantWriters[index] {
					t.Fatalf("writers = %v, want %v", writers, testCase.wantWriters)
				}
			}
		})
	}
}
