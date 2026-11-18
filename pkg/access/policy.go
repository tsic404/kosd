// Package access is the rule set behind the control plane's authorization seam:
// which objects a session role sees in the panel, which it may write, and who
// owns the objects it writes (C-59, C-60).
//
// The rules are evaluated per request. A role is never cached on the session
// record here: the caller passes the role the identity middleware resolved for
// this request, so a demotion takes effect on the next call (C-63).
package access

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/api/v1beta1"
	"github.com/tsic404/kosd/pkg/apptoken"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/write"
)

// Session is the identity behind one request: the account name and the role
// read for this call (C-59, C-63).
type Session struct {
	// Username is the login name; it is what spec.owner and the visibility
	// member list carry.
	Username string
	// Role is the account's current role.
	Role write.Role
	// App is the application identity when the request carries an application
	// token instead of a session (KOSD-C-85, KOSD-C-86).
	App *AppSession
}

// AppSession is the application identity one request carries (KOSD-C-86): the
// installed application, the request origin, and the read authorization the
// token was issued with.
type AppSession struct {
	// Extension identifies the installed application's extension.
	Extension string
	// AppID is the application the token belongs to.
	AppID string
	// Origin is the Origin header the request carried (KOSD-C-87).
	Origin string
	// TokenOrigin is the entry origin recorded when the token was issued
	// (KOSD-C-75). A request from another origin stays outside the binding.
	TokenOrigin string
	// Grants is the read authorization recorded when the token was issued
	// (KOSD-C-85). It is an upper bound, never a replacement for the current
	// declaration or the account's current visibility.
	Grants []apptoken.Grant
	// Objects is the issue-time readable range the token recorded for classes
	// whose visibility is decided per object (KOSD-C-85). Without it a class
	// grant admits no single object, so a later permission change cannot widen
	// an existing token.
	Objects []apptoken.Object
}

// DeclaredRead is one declared read permission: the classes it covers and the
// role it requires (KOSD-C-85).
type DeclaredRead struct {
	// Grants are the group and kind pairs the permission covers. An empty kind
	// covers every kind of the group.
	Grants []apptoken.Grant
	// RequiredRole is the session role the permission needs; empty admits every
	// signed-in role.
	RequiredRole write.Role
}

// signedIn reports whether the session carries a usable role.
func (s Session) signedIn() bool {
	return s.Username != "" && (s.Role == write.RoleOwner || s.Role == write.RoleMember || s.Role == write.RoleGuest)
}

// isOwner reports whether the session is the global Owner, which writes every
// object and is the only role that transfers one (C-59, C-60).
func (s Session) isOwner() bool { return s.Role == write.RoleOwner }

// memberKinds are the kinds a Member may write, and only when it owns them
// (C-59). An endpoint's owner is its target application's, resolved through the
// application rather than a field of its own (C-60).
var memberKinds = map[string]bool{
	"Application":     true,
	"Endpoint":        true,
	"Volume":          true,
	"BackupPolicy":    true,
	"RestoreJob":      true,
	"VolumeMigration": true,
}

// ownedKinds carry spec.owner; the list is C-60's and the write path's single
// source for which kinds the API stamps.
var ownedKinds = map[string]bool{
	"Application":     true,
	"Volume":          true,
	"BackupPolicy":    true,
	"RestoreJob":      true,
	"VolumeMigration": true,
}

// ownerOnlyKinds are managed by the global Owner alone: a Member's write is
// refused whatever its ownership (C-59).
var ownerOnlyKinds = map[string]bool{
	"Provider":              true,
	"SystemComponent":       true,
	"ApplicationDefinition": true,
	"NodeDevice":            true,
}

// ExtensionRoles answers what an extension's declaration grants (C-18): the
// session roles that may write its object classes, and which extension declares
// a class (C-14). The extension registry implements it.
type ExtensionRoles interface {
	// ObjectWriters returns the declared writer roles of one class; ok is false
	// when no declaration names the class.
	ObjectWriters(group, kind string) (writers []write.Role, ok bool)
	// DeclaredBy returns the identifier of the extension declaring the class.
	DeclaredBy(group, kind string) (identifier string, ok bool)
	// ReadScopes returns the read permissions one extension currently declares
	// (KOSD-C-85). An identifier that declares none reads nothing.
	ReadScopes(identifier string) []DeclaredRead
	// AppOrigin returns the entry origin one installed application declares
	// (KOSD-C-87).
	AppOrigin(identifier, appID string) (string, bool)
}

// Policy is the C-59/C-60 rule set over control-plane requests.
type Policy struct {
	// Objects reads the objects an ownership rule resolves through. A nil
	// source makes every cross-object lookup fail closed.
	Objects controlplane.ObjectSource
	// Extensions resolves declared extension write roles; nil means no class is
	// declared, so only the global Owner writes extension objects (C-59).
	Extensions ExtensionRoles
}

// Decide judges one request. The grant carries the actor for the audit stream
// even when the request is refused (C-14), and the owner the API writes when a
// permitted write creates or transfers one (C-60).
func (p *Policy) Decide(ctx context.Context, session Session, request controlplane.Request) (controlplane.Grant, error) {
	grant := controlplane.Grant{Actor: session.Username}
	if !session.signedIn() {
		return grant, fmt.Errorf("access: the request carries no role")
	}
	if session.App != nil {
		return p.decideApp(ctx, session, request)
	}
	switch request.Verb {
	case controlplane.VerbRead:
		if request.Object == nil {
			return grant, nil
		}
		visible, err := p.visible(ctx, session, request.Ref, request.Object)
		if err != nil {
			return grant, err
		}
		if !visible {
			return grant, controlplane.ErrHidden
		}
		return grant, nil
	case controlplane.VerbPlan, controlplane.VerbCreate, controlplane.VerbUpdate, controlplane.VerbApply:
		item, err := p.write(ctx, session, request)
		item.Actor = grant.Actor
		return item, err
	case controlplane.VerbDelete:
		return p.remove(ctx, session, request, grant)
	case controlplane.VerbDeletePod:
		// A Pod is not a KOS object: deleting one is judged as a write to the
		// application that owns it, so C-59's role write rule decides (C-77).
		return p.remove(ctx, session, request, grant)
	case controlplane.VerbEventRead:
		return grant, p.eventVisible(ctx, session, request)
	default:
		return grant, fmt.Errorf("access: verb %q is not judged", request.Verb)
	}
}

// decideApp judges one request that carries an application token (KOSD-C-85,
// KOSD-C-86). Only reads run, and one read must stay inside the intersection of
// the extension's current declaration, the token's issue-time grants and the
// account's current visibility.
func (p *Policy) decideApp(ctx context.Context, session Session, request controlplane.Request) (controlplane.Grant, error) {
	app := session.App
	grant := controlplane.Grant{Actor: session.Username, Extension: app.Extension}
	switch request.Verb {
	case controlplane.VerbRead, controlplane.VerbEventRead:
	default:
		return grant, denyf("an application token never writes")
	}
	// A collection read that names no object still asks whether the identity is
	// admitted to the surface at all. Answering that here keeps the question
	// apart from per-item visibility, which only ever hides one item
	// (KOSD-C-85, KOSD-C-86).
	if request.Admit {
		if !p.appAdmitted(session) || !p.appAuthorized(session) {
			return grant, controlplane.ErrUnadmitted
		}
		return grant, nil
	}
	// A collection read walks its items one by one; an item outside the class
	// or object range is dropped from the page. A single read is answered as a
	// refusal instead, because the caller named that object (KOSD-C-85).
	if !p.appMayRead(session, request.Ref) {
		if request.Filter {
			return grant, controlplane.ErrHidden
		}
		return grant, denyf("the application token does not grant reading %s", request.Ref.Kind)
	}
	if request.Verb == controlplane.VerbEventRead {
		return grant, p.eventVisible(ctx, session, request)
	}
	if request.Object == nil {
		return grant, nil
	}
	visible, err := p.visible(ctx, session, request.Ref, request.Object)
	if err != nil {
		return grant, err
	}
	if !visible {
		// An application token reads only inside its issue-time range; an object
		// outside that range is an out-of-scope read, which KOSD-C-85 and
		// KOSD-C-86 answer as permission denied rather than as an absent object.
		// A collection read drops the same object from its page.
		if request.Filter {
			return grant, controlplane.ErrHidden
		}
		return grant, denyf("the application token does not admit %s", request.Ref.String())
	}
	return grant, nil
}

// appAdmitted reports whether an application identity may call the read surface
// at all: the extension must currently declare it, and the request origin must
// be the declared entry origin recorded when the token was issued (KOSD-C-85,
// KOSD-C-87). It names no object, so it is the question a collection read asks
// before it walks any item.
func (p *Policy) appAdmitted(session Session) bool {
	if p.Extensions == nil || session.App == nil {
		return false
	}
	declared, ok := p.Extensions.AppOrigin(session.App.Extension, session.App.AppID)
	return ok && declared != "" && session.App.Origin == declared && session.App.Origin == session.App.TokenOrigin
}

// appAuthorized reports whether any read authorization survives the C-85
// intersection: a read the extension currently declares, a class the token was
// issued for, and a declared role the account still holds. A collection read
// asks this once, so an identity the intersection leaves empty is refused
// instead of answered with an empty page (KOSD-C-85, KOSD-C-86). The account's
// current readable range is per object, so it stays on the per-item side: an
// authorized identity with nothing visible still reads an empty page.
func (p *Policy) appAuthorized(session Session) bool {
	if p.Extensions == nil || session.App == nil {
		return false
	}
	for _, read := range p.Extensions.ReadScopes(session.App.Extension) {
		if !roleSatisfies(session.Role, read.RequiredRole) {
			continue
		}
		for _, issued := range session.App.Grants {
			for _, declared := range read.Grants {
				if grantOverlap(issued, declared) {
					return true
				}
			}
		}
	}
	return false
}

// grantOverlap reports whether two grants admit a common class: the same API
// group, with either side covering every kind or both naming the same one.
func grantOverlap(left, right apptoken.Grant) bool {
	if left.Group != right.Group {
		return false
	}
	return left.Kind == "" || right.Kind == "" || left.Kind == right.Kind
}

// appMayRead applies the limits a read carries: the request origin must be the
// application's declared entry origin, the extension must currently declare the
// class with a role the account still holds, the token must have been issued for
// it, and an object of a visibility-scoped class must sit inside the range the
// issuer recorded (KOSD-C-85, KOSD-C-87).
func (p *Policy) appMayRead(session Session, ref controlplane.ObjectRef) bool {
	if !p.appAdmitted(session) {
		return false
	}
	if !covers(session.App.Grants, ref.Group, ref.Kind) {
		return false
	}
	if ref.Name != "" && !objectScopeAdmits(session.App, ref) {
		return false
	}
	for _, read := range p.Extensions.ReadScopes(session.App.Extension) {
		if !roleSatisfies(session.Role, read.RequiredRole) {
			continue
		}
		if covers(read.Grants, ref.Group, ref.Kind) {
			return true
		}
	}
	return false
}

// objectScopeAdmits reports whether one object sits inside the range the token
// recorded at issue time (KOSD-C-85). A class whose visibility the API decides
// per object must name the object; every other class is covered by its class
// grant, because its visibility does not depend on the account.
func objectScopeAdmits(app *AppSession, ref controlplane.ObjectRef) bool {
	if !visibilityScoped(ref) {
		return true
	}
	for _, issued := range app.Objects {
		if issued.Matches(ref.Group, ref.Kind, ref.Namespace, ref.Name) {
			return true
		}
	}
	return false
}

// visibilityScoped reports whether one class decides visibility per object
// (C-59). An application follows its owner and its visibility list; every other
// class is visible to every signed-in role, so its class grant is already the
// whole range.
func visibilityScoped(ref controlplane.ObjectRef) bool {
	return ref.Group == api.Group && ref.Kind == "Application"
}

// covers reports whether any grant admits one group and kind.
func covers(grants []apptoken.Grant, group, kind string) bool {
	for _, grant := range grants {
		if grant.Covers(group, kind) {
			return true
		}
	}
	return false
}

// roleRank orders the session roles; an unknown or empty role fails every
// declared requirement.
var roleRank = map[write.Role]int{
	write.RoleGuest:  0,
	write.RoleMember: 1,
	write.RoleOwner:  2,
}

// roleSatisfies reports whether one role meets a declared requirement. An empty
// requirement admits every signed-in role.
func roleSatisfies(role, required write.Role) bool {
	rank, signedIn := roleRank[role]
	if !signedIn {
		return false
	}
	if required == "" {
		return true
	}
	minimum, known := roleRank[required]
	return known && rank >= minimum
}

// visible applies C-59's visibility predicate. Only applications have one:
// every other class is visible to every signed-in role.
func (p *Policy) visible(ctx context.Context, session Session, ref controlplane.ObjectRef, object map[string]any) (bool, error) {
	if ref.Group != api.Group || ref.Kind != "Application" {
		return true, nil
	}
	spec, _ := object["spec"].(map[string]any)
	owner, _ := spec["owner"].(string)
	// The object's owner always sees it, and the global Owner manages every
	// application (C-59).
	if session.isOwner() || (owner != "" && owner == session.Username) {
		return true, nil
	}
	visibility, _ := spec["visibility"].(map[string]any)
	if session.Role == write.RoleGuest {
		allow, _ := visibility["allowGuests"].(bool)
		return allow, nil
	}
	// A member sees the application only when the member list names its
	// account. An entry that names no account never matches a signed-in
	// username, so unresolvable names drop out without a lookup (C-59).
	return namesMember(visibility, session.Username), nil
}

// namesMember reports whether a member list names one username.
func namesMember(visibility map[string]any, username string) bool {
	if username == "" {
		return false
	}
	members, _ := visibility["members"].([]any)
	for _, entry := range members {
		if name, _ := entry.(string); name == username {
			return true
		}
	}
	return false
}

// eventVisible applies C-64's event rule on top of C-59: an application's
// events follow the application's visibility, while every other object's events
// reach only the global Owner and the member owning the object, never a Guest.
// An object the read could not resolve counts as one without an owner, so the
// rule never widens what the panel shows.
func (p *Policy) eventVisible(ctx context.Context, session Session, request controlplane.Request) error {
	ref := request.Ref
	if ref.Group == api.Group && ref.Kind == "Application" {
		visible, err := p.visible(ctx, session, ref, request.Object)
		if err != nil {
			return err
		}
		if !visible {
			return controlplane.ErrHidden
		}
		return nil
	}
	if session.isOwner() {
		return nil
	}
	if session.Role == write.RoleGuest {
		return controlplane.ErrHidden
	}
	if owner, has := ownerOf(request.Object); has && owner == session.Username {
		return nil
	}
	return controlplane.ErrHidden
}

// write judges a create, update, apply or plan (C-59, C-60).
func (p *Policy) write(ctx context.Context, session Session, request controlplane.Request) (controlplane.Grant, error) {
	ref := request.Ref
	if ref.Group != api.Group {
		return p.writeExtension(session, ref)
	}
	if session.Role == write.RoleGuest {
		return controlplane.Grant{}, denyf("%s is read-only", session.Role)
	}
	switch {
	case session.isOwner():
		return p.writeOwner(session, request)
	case session.Role == write.RoleMember:
		return p.writeMember(ctx, session, request)
	default:
		return controlplane.Grant{}, denyf("role %q writes nothing", session.Role)
	}
}

// writeOwner judges the global Owner's write. The Owner writes every kind; the
// only rule left is the owner field: the API stamps the session at creation and
// only a transfer may move it after that (C-60).
func (p *Policy) writeOwner(session Session, request controlplane.Request) (controlplane.Grant, error) {
	if !ownedKinds[request.Ref.Kind] {
		return controlplane.Grant{}, nil
	}
	if request.Current == nil {
		return controlplane.Grant{Owner: session.Username}, nil
	}
	current, _ := ownerOf(request.Current)
	if submitted, hasSubmitted := ownerOf(request.Object); hasSubmitted && submitted != "" && submitted != current {
		return controlplane.Grant{Owner: submitted}, nil
	}
	return controlplane.Grant{Owner: current}, nil
}

// writeMember judges a Member's write: it may write only the kinds C-59 gives
// it and only the objects it owns (C-60).
func (p *Policy) writeMember(ctx context.Context, session Session, request controlplane.Request) (controlplane.Grant, error) {
	ref := request.Ref
	switch {
	case !memberKinds[ref.Kind]:
		return controlplane.Grant{}, denyf("only the global Owner may write %s", ref.Kind)
	case ref.Kind == "Endpoint":
		if err := p.requireApplicationOwner(ctx, session, ref, request.Object, request.Current); err != nil {
			return controlplane.Grant{}, err
		}
		return controlplane.Grant{}, nil
	}
	owner := session.Username
	if request.Current != nil {
		current, _ := ownerOf(request.Current)
		if current == "" {
			return controlplane.Grant{}, denyf("an object without an owner is the global Owner's")
		}
		if current != session.Username {
			return controlplane.Grant{}, denyf("the object is owned by %q", current)
		}
		if submitted, hasSubmitted := ownerOf(request.Object); hasSubmitted && submitted != current {
			return controlplane.Grant{}, denyf("only the global Owner may transfer spec.owner")
		}
		owner = current
	}
	if err := p.checkReferences(ctx, session, ref, request.Object); err != nil {
		return controlplane.Grant{}, err
	}
	return controlplane.Grant{Owner: owner}, nil
}

// remove judges a delete. An object that is already gone is still judged by
// role, so a Guest is refused and a repeat delete stays idempotent (C-12).
func (p *Policy) remove(ctx context.Context, session Session, request controlplane.Request, grant controlplane.Grant) (controlplane.Grant, error) {
	ref := request.Ref
	if ref.Group != api.Group {
		item, err := p.writeExtension(session, ref)
		item.Actor = grant.Actor
		return item, err
	}
	if session.Role == write.RoleGuest {
		return grant, denyf("%s is read-only", session.Role)
	}
	if session.isOwner() {
		return grant, nil
	}
	if request.Current == nil {
		// Nothing is stored under the name; there is no ownership to protect.
		return grant, nil
	}
	switch {
	case ref.Kind == "Endpoint":
		if err := p.requireApplicationOwner(ctx, session, ref, nil, request.Current); err != nil {
			return grant, err
		}
		return grant, nil
	case memberKinds[ref.Kind]:
		current, _ := ownerOf(request.Current)
		if current == "" {
			return grant, denyf("an object without an owner is the global Owner's")
		}
		if current != session.Username {
			return grant, denyf("the object is owned by %q", current)
		}
		return grant, nil
	default:
		return grant, denyf("only the global Owner may delete %s", ref.Kind)
	}
}

// writeExtension judges an extension object's write by its declaration (C-18):
// the declared writer roles may write it, and an undeclared class is the global
// Owner's alone (C-59).
func (p *Policy) writeExtension(session Session, ref controlplane.ObjectRef) (controlplane.Grant, error) {
	if p.Extensions == nil {
		if session.isOwner() {
			return controlplane.Grant{}, nil
		}
		return controlplane.Grant{}, denyf("%s writes no object of class %s", session.Role, ref.Kind)
	}
	identifier, _ := p.Extensions.DeclaredBy(ref.Group, ref.Kind)
	grant := controlplane.Grant{Extension: identifier}
	writers, declared := p.Extensions.ObjectWriters(ref.Group, ref.Kind)
	if !declared {
		if session.isOwner() {
			return grant, nil
		}
		return grant, denyf("%s writes no undeclared class %s", session.Role, ref.Kind)
	}
	for _, writer := range writers {
		if writer == session.Role {
			return grant, nil
		}
	}
	return grant, denyf("the declaration does not let %s write %s", session.Role, ref.Kind)
}

// requireApplicationOwner holds an endpoint to its target application's owner:
// an endpoint has no owner of its own, and C-60 derives it from the application
// it exposes.
func (p *Policy) requireApplicationOwner(ctx context.Context, session Session, ref controlplane.ObjectRef, object, current map[string]any) error {
	name := ""
	if current != nil {
		name = nestedString(current, "spec", "targetApp")
	}
	if name == "" && object != nil {
		name = nestedString(object, "spec", "targetApp")
	}
	if name == "" {
		return denyf("the endpoint names no target application")
	}
	owner, found, err := p.ownerOf(ctx, "Application", ref.Namespace, name)
	if err != nil {
		return err
	}
	if !found {
		return denyf("the target application %q does not exist, so its owner cannot be resolved", name)
	}
	if owner != session.Username {
		return denyf("the target application is owned by %q", owner)
	}
	return nil
}

// checkReferences enforces C-60's cross-owner rules on one Member write: an
// application may not reference another account's volume, a backup policy's
// selector may not reach another account's applications or volumes, and a
// restore job may not read another account's policy (C-41). The rules only
// decide range; they never change ownership.
func (p *Policy) checkReferences(ctx context.Context, session Session, ref controlplane.ObjectRef, object map[string]any) error {
	if object == nil {
		return nil
	}
	switch ref.Kind {
	case "Application":
		for _, name := range stringList(nestedValue(object, "spec", "volumes")) {
			owner, found, err := p.ownerOf(ctx, "Volume", ref.Namespace, name)
			if err != nil {
				return err
			}
			if found && owner != "" && owner != session.Username {
				return denyf("volume %q is owned by %q", name, owner)
			}
		}
	case "RestoreJob":
		policyName := nestedString(object, "spec", "policy")
		if policyName == "" {
			return denyf("spec.policy is required: a restore reads one policy's run")
		}
		owner, found, err := p.ownerOf(ctx, "BackupPolicy", ref.Namespace, policyName)
		if err != nil {
			return err
		}
		if !found {
			return denyf("backup policy %q does not exist, so its owner cannot be resolved", policyName)
		}
		if owner != "" && owner != session.Username {
			return denyf("backup policy %q is owned by %q", policyName, owner)
		}
	case "BackupPolicy":
		selector := nestedString(object, "spec", "selector")
		if selector == "" {
			return nil
		}
		parsed, err := labels.Parse(selector)
		if err != nil {
			return denyf("spec.selector is not a label selector: %v", err)
		}
		for _, kind := range []string{"Application", "Volume"} {
			owners, err := p.matchedOwners(ctx, kind, parsed)
			if err != nil {
				return err
			}
			for _, owner := range owners {
				if owner != "" && owner != session.Username {
					return denyf("the selector reaches a %s owned by %q", kind, owner)
				}
			}
		}
	}
	return nil
}

// matchedOwners lists the owners of every object of one kernel kind the selector
// matches.
func (p *Policy) matchedOwners(ctx context.Context, kind string, selector labels.Selector) ([]string, error) {
	if p.Objects == nil {
		return nil, denyf("a selector cannot be judged without a reader")
	}
	objectType, ok := kernelObjectType(kind)
	if !ok {
		return nil, fmt.Errorf("access: %s is not a kernel kind", kind)
	}
	observed, err := p.Objects.List(ctx, objectType, "")
	if err != nil {
		return nil, err
	}
	owners := make([]string, 0, len(observed))
	for _, item := range observed {
		metadata, _ := item.Object["metadata"].(map[string]any)
		objectLabels, _ := metadata["labels"].(map[string]any)
		if !selector.Matches(labels.Set(flatLabels(objectLabels))) {
			continue
		}
		owner, _ := ownerOf(item.Object)
		owners = append(owners, owner)
	}
	return owners, nil
}

// ownerOf reads one kernel object's owner; found is false when the object does
// not exist.
func (p *Policy) ownerOf(ctx context.Context, kind, namespace, name string) (string, bool, error) {
	if p.Objects == nil {
		return "", false, denyf("ownership cannot be resolved without a reader")
	}
	objectType, ok := kernelObjectType(kind)
	if !ok {
		return "", false, fmt.Errorf("access: %s is not a kernel kind", kind)
	}
	observed, err := p.Objects.Get(ctx, objectType, namespace, name)
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	owner, _ := ownerOf(observed.Object)
	return owner, true, nil
}

// kernelObjectType builds the class of one kernel kind in the storage version.
func kernelObjectType(kind string) (controlplane.ObjectType, bool) {
	resource, ok := api.ResourceName(kind)
	if !ok {
		return controlplane.ObjectType{}, false
	}
	scope, ok := api.ScopeOf(kind)
	if !ok {
		return controlplane.ObjectType{}, false
	}
	return controlplane.ObjectType{
		Group:      api.Group,
		Version:    v1beta1.GroupVersion.Version,
		Kind:       kind,
		Resource:   resource,
		Namespaced: scope == api.ScopeNamespaced,
	}, true
}

// ownerOf reads spec.owner of a raw object; has is false when the field is
// absent.
func ownerOf(object map[string]any) (owner string, has bool) {
	value, ok := nestedValue(object, "spec", "owner").(string)
	return value, ok
}

// nestedValue reads a value below a chain of map keys.
func nestedValue(object map[string]any, path ...string) any {
	var current any = object
	for _, key := range path {
		fields, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = fields[key]
	}
	return current
}

// nestedString reads a nested string, treating a missing value as empty.
func nestedString(object map[string]any, path ...string) string {
	value, _ := nestedValue(object, path...).(string)
	return value
}

// stringList reads a list of strings, ignoring entries of another type.
func stringList(value any) []string {
	entries, ok := value.([]any)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if name, _ := entry.(string); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// flatLabels converts a raw label map for selector matching.
func flatLabels(value map[string]any) map[string]string {
	converted := make(map[string]string, len(value))
	for key, item := range value {
		if text, ok := item.(string); ok {
			converted[key] = text
		}
	}
	return converted
}

// denyf builds a refusal reason; the control plane renders it as the stable
// permission failure and keeps the reason in its log (C-12).
func denyf(format string, args ...any) error {
	return fmt.Errorf("access: "+format, args...)
}
