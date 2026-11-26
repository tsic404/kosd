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

// ReadScopes implements access.ExtensionRoles; this fixture declares no reads.
func (d declaredClasses) ReadScopes(string) []access.DeclaredRead { return nil }

// AppOrigin implements access.ExtensionRoles; this fixture declares no app.
func (d declaredClasses) AppOrigin(string, string) (string, bool) { return "", false }

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

// TestApplicationAdmissionComparesBrowserOrigins pins KOSD-C-87's exact-origin
// rule against the form a browser serializes: a declaration that writes the
// HTTPS default port still admits the Origin a browser sends, while a
// non-default port stays its own origin and never admits another one.
func TestApplicationAdmissionComparesBrowserOrigins(t *testing.T) {
	grant := apptoken.Grant{Group: api.Group, Kind: "Application"}
	admit := func(declaredOrigin, requestOrigin, tokenOrigin string) error {
		policy := &access.Policy{Extensions: fixedRoles{
			reads:  []access.DeclaredRead{{Grants: []apptoken.Grant{grant}}},
			origin: declaredOrigin,
		}}
		_, err := policy.Decide(context.Background(), access.Session{
			Username: "alice",
			Role:     write.RoleMember,
			App: &access.AppSession{
				Extension:   "demo.widgets",
				AppID:       "shop",
				Origin:      requestOrigin,
				TokenOrigin: tokenOrigin,
				Grants:      []apptoken.Grant{grant},
			},
		}, controlplane.Request{
			Verb:  controlplane.VerbRead,
			Ref:   controlplane.ObjectRef{Kind: "Event"},
			Admit: true,
		})
		return err
	}

	cases := []struct {
		name                      string
		declared, request, issued string
		refused                   bool
	}{
		{
			name:     "an explicit default port in the declaration admits the browser origin",
			declared: "https://shop.example.test:443", request: "https://shop.example.test", issued: "https://shop.example.test",
		},
		{
			name:     "the same origin written every way is admitted",
			declared: "https://shop.example.test", request: "https://shop.example.test", issued: "https://shop.example.test:443",
		},
		{
			name:     "a non-default port admits the request that carries it",
			declared: "https://shop.example.test:8443", request: "https://shop.example.test:8443", issued: "https://shop.example.test:8443",
		},
		{
			name:     "a non-default port refuses the default-port origin",
			declared: "https://shop.example.test:8443", request: "https://shop.example.test", issued: "https://shop.example.test:8443",
			refused: true,
		},
		{
			name:     "a default-port declaration refuses a non-default request",
			declared: "https://shop.example.test", request: "https://shop.example.test:8443", issued: "https://shop.example.test:8443",
			refused: true,
		},
		{
			name:     "another host is still refused",
			declared: "https://shop.example.test:443", request: "https://admin.example.test", issued: "https://admin.example.test",
			refused: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := admit(testCase.declared, testCase.request, testCase.issued)
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
}
