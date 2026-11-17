package access_test

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/pkg/access"
	"github.com/tsic404/kosd/pkg/apptoken"
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

// fixedRoles is one extension's current declaration as the admission rule reads
// it: the read scopes it grants and the entry origin of the one application.
type fixedRoles struct {
	reads  []access.DeclaredRead
	origin string
}

// ObjectWriters implements access.ExtensionRoles.
func (f fixedRoles) ObjectWriters(string, string) ([]write.Role, bool) { return nil, false }

// DeclaredBy implements access.ExtensionRoles.
func (f fixedRoles) DeclaredBy(string, string) (string, bool) { return "", false }

// ReadScopes implements access.ExtensionRoles.
func (f fixedRoles) ReadScopes(string) []access.DeclaredRead { return f.reads }

// AppOrigin implements access.ExtensionRoles.
func (f fixedRoles) AppOrigin(string, string) (string, bool) {
	if f.origin == "" {
		return "", false
	}
	return f.origin, true
}

// TestApplicationCollectionAdmissionNeedsAReadAuthorization pins the second half
// of the collection-level question: besides the declared origin, the identity
// must still hold one read inside the C-85 intersection, and an empty
// intersection is refused rather than answered with an empty page. An item a
// valid identity may not see stays a per-item hide (KOSD-C-85, KOSD-C-86).
func TestApplicationCollectionAdmissionNeedsAReadAuthorization(t *testing.T) {
	const (
		extension = "demo.widgets"
		origin    = "https://shop.example.test"
	)
	application := apptoken.Grant{Group: api.Group, Kind: "Application"}
	widget := apptoken.Grant{Group: "widgets.example.test", Kind: "Widget"}
	read := func(role write.Role, grants ...apptoken.Grant) access.DeclaredRead {
		return access.DeclaredRead{Grants: grants, RequiredRole: role}
	}
	session := func(role write.Role, grants ...apptoken.Grant) access.Session {
		return access.Session{
			Username: "alice",
			Role:     role,
			App: &access.AppSession{
				Extension: extension, AppID: "shop",
				Origin: origin, TokenOrigin: origin, Grants: grants,
			},
		}
	}
	admit := func(policy *access.Policy, who access.Session) error {
		_, err := policy.Decide(context.Background(), who, controlplane.Request{
			Verb:  controlplane.VerbRead,
			Ref:   controlplane.ObjectRef{Kind: "Event"},
			Admit: true,
		})
		return err
	}
	declaring := func(reads ...access.DeclaredRead) *access.Policy {
		return &access.Policy{Extensions: fixedRoles{reads: reads, origin: origin}}
	}

	cases := []struct {
		name    string
		policy  *access.Policy
		session access.Session
		refused bool
	}{
		{
			name:    "a read the declaration grants and the token was issued for is admitted",
			policy:  declaring(read("", application)),
			session: session(write.RoleMember, application),
		},
		{
			name:    "a role the declaration asks for is admitted",
			policy:  declaring(read(write.RoleOwner, widget)),
			session: session(write.RoleOwner, widget),
		},
		{
			name:    "an empty grant leaves the intersection empty",
			policy:  declaring(read("", application)),
			session: session(write.RoleMember),
			refused: true,
		},
		{
			name:    "a declaration that declares no read leaves the intersection empty",
			policy:  declaring(),
			session: session(write.RoleMember, application),
			refused: true,
		},
		{
			name:    "a class the declaration does not grant leaves the intersection empty",
			policy:  declaring(read("", widget)),
			session: session(write.RoleMember, application),
			refused: true,
		},
		{
			name:    "a declaration role the account does not hold leaves the intersection empty",
			policy:  declaring(read(write.RoleOwner, widget)),
			session: session(write.RoleMember, widget),
			refused: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := admit(testCase.policy, testCase.session)
			if testCase.refused {
				if !errors.Is(err, controlplane.ErrUnadmitted) {
					t.Fatalf("admission = %v, want the identity unadmitted", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("admission = %v, want the identity admitted", err)
			}
		})
	}

	// An admitted identity still drops one object outside its issue-time range;
	// the per-item answer is a hide, never a surface refusal.
	within := session(write.RoleMember, application)
	if _, err := declaring(read("", application)).Decide(context.Background(), within, controlplane.Request{
		Verb:   controlplane.VerbRead,
		Ref:    controlplane.ObjectRef{Group: api.Group, Kind: "Application", Namespace: "apps", Name: "other"},
		Filter: true,
	}); !errors.Is(err, controlplane.ErrHidden) {
		t.Fatalf("per-item read = %v, want the object hidden", err)
	}
}
