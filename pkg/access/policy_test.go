package access_test

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/pkg/access"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/write"
)

// fixedSource answers kernel-object reads from a map keyed by kind, namespace
// and name.
type fixedSource struct {
	objects map[string]map[string]any
}

func sourceKey(object controlplane.ObjectType, namespace, name string) string {
	return object.Kind + "/" + namespace + "/" + name
}

// Get implements controlplane.ObjectSource.
func (s fixedSource) Get(_ context.Context, object controlplane.ObjectType, namespace, name string) (controlplane.ObservedObject, error) {
	found, ok := s.objects[sourceKey(object, namespace, name)]
	if !ok {
		return controlplane.ObservedObject{}, apierrors.NewNotFound(schema.GroupResource{Group: object.Group, Resource: object.Resource}, name)
	}
	return controlplane.ObservedObject{Object: found}, nil
}

// List implements controlplane.ObjectSource.
func (s fixedSource) List(context.Context, controlplane.ObjectType, string) ([]controlplane.ObservedObject, error) {
	return nil, nil
}

// TestMemberRestoreJobNeedsItsOwnPolicy covers C-41's restore read path: a
// member may restore only the run of a policy it owns, while the global Owner is
// not limited by ownership (C-59, C-60).
func TestMemberRestoreJobNeedsItsOwnPolicy(t *testing.T) {
	source := fixedSource{objects: map[string]map[string]any{
		"BackupPolicy/apps/mine":   {"spec": map[string]any{"owner": "alice"}},
		"BackupPolicy/apps/theirs": {"spec": map[string]any{"owner": "bob"}},
	}}
	policy := &access.Policy{Objects: source}
	request := func(policyName string) controlplane.Request {
		return controlplane.Request{
			Verb: controlplane.VerbCreate,
			Ref:  controlplane.ObjectRef{Group: api.Group, Kind: "RestoreJob", Namespace: "apps", Name: "restore"},
			Object: map[string]any{
				"metadata": map[string]any{"name": "restore", "namespace": "apps"},
				"spec":     map[string]any{"policy": policyName, "targetApplication": "restored"},
			},
		}
	}
	member := access.Session{Username: "alice", Role: write.RoleMember}
	if _, err := policy.Decide(context.Background(), member, request("mine")); err != nil {
		t.Fatalf("a member's own policy was refused: %v", err)
	}
	if _, err := policy.Decide(context.Background(), member, request("theirs")); err == nil {
		t.Fatal("a member restored another account's policy")
	}
	owner := access.Session{Username: "root", Role: write.RoleOwner}
	if _, err := policy.Decide(context.Background(), owner, request("theirs")); err != nil {
		t.Fatalf("the global Owner was refused: %v", err)
	}
}

// declaredClasses answers the extension seam from fixed declarations.
type declaredClasses struct {
	group, kind string
	writers     []write.Role
	declared    bool
}

func (d declaredClasses) ObjectWriters(group, kind string) ([]write.Role, bool) {
	if group != d.group || kind != d.kind {
		return nil, false
	}
	return d.writers, d.declared
}

func (d declaredClasses) DeclaredBy(group, kind string) (string, bool) {
	if group != d.group || kind != d.kind {
		return "", false
	}
	return "demo.records", true
}

// TestExtensionWriterSetFormsAreJudgedApart covers C-18's two declared forms: an
// empty writer set denies every session role, the global Owner included, while
// an absent set leaves the class to C-59's default (KOSD-AC-80).
func TestExtensionWriterSetFormsAreJudgedApart(t *testing.T) {
	const group = "records.example.test"
	request := controlplane.Request{
		Verb: controlplane.VerbCreate,
		Ref:  controlplane.ObjectRef{Group: group, Kind: "Audit", Namespace: "apps", Name: "one"},
		Object: map[string]any{
			"metadata": map[string]any{"name": "one", "namespace": "apps"},
			"spec":     map[string]any{"size": "small"},
		},
	}
	sessions := map[string]access.Session{
		"owner":  {Username: "root", Role: write.RoleOwner},
		"member": {Username: "alice", Role: write.RoleMember},
		"guest":  {Username: "guest", Role: write.RoleGuest},
	}

	declaredEmpty := &access.Policy{Extensions: declaredClasses{group: group, kind: "Audit", declared: true}}
	for name, session := range sessions {
		if _, err := declaredEmpty.Decide(context.Background(), session, request); err == nil {
			t.Fatalf("%s wrote a class whose writer set is declared empty", name)
		}
	}

	undeclared := &access.Policy{Extensions: declaredClasses{group: group, kind: "Audit"}}
	if _, err := undeclared.Decide(context.Background(), sessions["owner"], request); err != nil {
		t.Fatalf("the global Owner was refused a class left to C-59's default: %v", err)
	}
	for _, name := range []string{"member", "guest"} {
		if _, err := undeclared.Decide(context.Background(), sessions[name], request); err == nil {
			t.Fatalf("%s wrote a class left to C-59's default", name)
		}
	}
}
