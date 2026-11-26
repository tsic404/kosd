package access

import (
	"context"
	"fmt"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/pkg/apptoken"
	"github.com/tsic404/kosd/pkg/controlplane"
)

// applicationClass is the one class whose visibility the API decides per
// object (C-59), and the only class a token's issue-time range has to name.
var applicationClass = controlplane.ObjectType{
	Group:      api.Group,
	Version:    "v1beta1",
	Kind:       "Application",
	Resource:   "applications",
	Namespaced: true,
}

// AppReadScope returns the read authorization one window token carries
// (KOSD-C-85): the classes the extension currently declares that the session's
// role satisfies, and, for the class whose visibility is decided per object,
// the objects the session can read right now.
//
// It is the upper bound a token keeps: a later permission change or a widened
// visibility can only act inside this range, never outside it, and an
// authorization that cannot be determined now is reported as unavailable rather
// than answered with a range that would widen the token.
func (p *Policy) AppReadScope(ctx context.Context, session Session, extension string) ([]apptoken.Grant, []apptoken.Object, error) {
	if p.Extensions == nil {
		return nil, nil, fmt.Errorf("%w: the extension declaration is not configured", controlplane.ErrUnavailable)
	}
	grants := make([]apptoken.Grant, 0, 4)
	seen := map[apptoken.Grant]bool{}
	for _, read := range p.Extensions.ReadScopes(extension) {
		if !roleSatisfies(session.Role, read.RequiredRole) {
			continue
		}
		for _, grant := range read.Grants {
			if !seen[grant] {
				seen[grant] = true
				grants = append(grants, grant)
			}
		}
	}
	if !covers(grants, api.Group, "Application") {
		return grants, nil, nil
	}
	objects, err := p.visibleObjects(ctx, session)
	if err != nil {
		return nil, nil, err
	}
	return grants, objects, nil
}

// visibleObjects names every application object a session can read now (C-59).
// A read that cannot tell is answered as unavailable, not as an empty range:
// the range a token carries is what a later widening must not pass (KOSD-C-86).
func (p *Policy) visibleObjects(ctx context.Context, session Session) ([]apptoken.Object, error) {
	if p.Objects == nil {
		return nil, fmt.Errorf("%w: the object source is not configured", controlplane.ErrUnavailable)
	}
	observed, err := p.Objects.List(ctx, applicationClass, "")
	if err != nil {
		return nil, fmt.Errorf("%w: list applications: %v", controlplane.ErrUnavailable, err)
	}
	scope := make([]apptoken.Object, 0, len(observed))
	for _, item := range observed {
		ref, ok := applicationRef(item.Object)
		if !ok {
			continue
		}
		visible, err := p.visible(ctx, session, ref, item.Object)
		if err != nil {
			return nil, err
		}
		if !visible {
			continue
		}
		scope = append(scope, apptoken.Object{
			Group:     ref.Group,
			Kind:      ref.Kind,
			Namespace: ref.Namespace,
			Name:      ref.Name,
		})
	}
	return scope, nil
}

// applicationRef reads the identity of one application object out of its raw
// form. An object without a name carries no readable range.
func applicationRef(object map[string]any) (controlplane.ObjectRef, bool) {
	metadata, _ := object["metadata"].(map[string]any)
	if metadata == nil {
		return controlplane.ObjectRef{}, false
	}
	name, _ := metadata["name"].(string)
	if name == "" {
		return controlplane.ObjectRef{}, false
	}
	namespace, _ := metadata["namespace"].(string)
	return controlplane.ObjectRef{
		Group: api.Group, Kind: "Application", Namespace: namespace, Name: name,
	}, true
}
