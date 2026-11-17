package access_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/pkg/access"
	"github.com/tsic404/kosd/pkg/apptoken"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/write"
)

// listedSource answers kernel-object reads from one application list.
type listedSource struct {
	objects []map[string]any
}

// List implements controlplane.ObjectSource.
func (s listedSource) List(context.Context, controlplane.ObjectType, string) ([]controlplane.ObservedObject, error) {
	observed := make([]controlplane.ObservedObject, 0, len(s.objects))
	for _, object := range s.objects {
		observed = append(observed, controlplane.ObservedObject{Object: object})
	}
	return observed, nil
}

// Get implements controlplane.ObjectSource.
func (s listedSource) Get(_ context.Context, _ controlplane.ObjectType, namespace, name string) (controlplane.ObservedObject, error) {
	for _, object := range s.objects {
		metadata, _ := object["metadata"].(map[string]any)
		if metadata["name"] == name && metadata["namespace"] == namespace {
			return controlplane.ObservedObject{Object: object}, nil
		}
	}
	return controlplane.ObservedObject{}, errors.New("not found")
}

// applicationFixture is one application object the fixture serves.
func applicationFixture(name, owner string, members ...string) map[string]any {
	entries := make([]any, 0, len(members))
	for _, member := range members {
		entries = append(entries, member)
	}
	return map[string]any{
		"metadata": map[string]any{"name": name, "namespace": "default"},
		"spec": map[string]any{
			"owner":      owner,
			"visibility": map[string]any{"members": entries, "allowGuests": false},
		},
	}
}

// TestAppReadScopeCarriesTheDeclaredRangeAndTheVisibleObjects pins the
// issue-time range: the classes the session's role satisfies, and for the
// application class only the objects the account reads now (KOSD-C-85).
func TestAppReadScopeCarriesTheDeclaredRangeAndTheVisibleObjects(t *testing.T) {
	const extension = "demo.widgets"
	roles := fixedRoles{
		reads: []access.DeclaredRead{
			{Grants: []apptoken.Grant{{Group: api.Group, Kind: "Application"}}},
			{Grants: []apptoken.Grant{{Group: "widgets.example.test", Kind: "Widget"}}, RequiredRole: write.RoleOwner},
		},
		origin: "https://shop.example.test",
	}
	source := listedSource{objects: []map[string]any{
		applicationFixture("shop-app", "owner", "member"),
		applicationFixture("theirs-app", "someone-else"),
	}}
	policy := &access.Policy{Objects: source, Extensions: roles}

	member := access.Session{Username: "member", Role: write.RoleMember}
	grants, objects, err := policy.AppReadScope(context.Background(), member, extension)
	if err != nil {
		t.Fatalf("read the member's scope: %v", err)
	}
	if len(grants) != 1 || grants[0].Kind != "Application" {
		t.Fatalf("grants = %+v, want the class the member's role satisfies", grants)
	}
	if len(objects) != 1 || objects[0].Name != "shop-app" || objects[0].Namespace != "default" {
		t.Fatalf("objects = %+v, want the application the member can read", objects)
	}

	owner := access.Session{Username: "owner", Role: write.RoleOwner}
	ownerGrants, _, err := policy.AppReadScope(context.Background(), owner, extension)
	if err != nil {
		t.Fatalf("read the owner's scope: %v", err)
	}
	if len(ownerGrants) != 2 {
		t.Fatalf("owner grants = %+v, want both declared classes", ownerGrants)
	}

	// An authorization that cannot be determined is answered as unavailable, so
	// a token is never minted with a range that could be read as a refusal.
	if _, _, err := (&access.Policy{Extensions: roles}).AppReadScope(context.Background(), member, extension); !errors.Is(err, controlplane.ErrUnavailable) {
		t.Fatalf("a policy without an object source = %v, want unavailable", err)
	}
	if _, _, err := (&access.Policy{Objects: source}).AppReadScope(context.Background(), member, extension); !errors.Is(err, controlplane.ErrUnavailable) {
		t.Fatalf("a policy without a declaration = %v, want unavailable", err)
	}
}
