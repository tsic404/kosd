package kosdtc120

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	taskmanager "github.com/tsic404/kos-taskmanager/app"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/identity"
	"github.com/tsic404/kosd/pkg/install"
	"github.com/tsic404/kosd/pkg/pages/cluster"
	"github.com/tsic404/kosd/pkg/write"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

// TestTC120InstalledClusterRoleReadsEvents pins the page read against the
// installation's own RBAC: the control-plane identity reads every item of the
// overview and detail pages through the ClusterRole the install renders. A
// cluster role without the core events rule fails here the way it fails the
// pages, while a cluster-scoped write verb would fail the read-only check.
func TestTC120InstalledClusterRoleReadsEvents(t *testing.T) {
	world := harness.Start(t)
	installer := harness.InstallerIdentity()
	world.GrantClusterAdmin(t, installer)
	accounts := harness.OpenIdentity(t, nil, nil)
	clients := world.InstallClients(t, installer)
	runner, err := install.New(install.Config{
		Plan:          harness.InstallerPlan(nil),
		Dynamic:       clients.Dynamic,
		Kube:          clients.Kube,
		APIExtensions: clients.APIExtensions,
		Definitions:   harness.Definitions(t),
		Accounts:      accounts.Service,
		QuietWindow:   200 * time.Millisecond,
		SettleTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the installer: %v", err)
	}
	if _, err := runner.Install(context.Background()); err != nil {
		t.Fatalf("install: %v", err)
	}

	// The pages read events across namespaces: one event about the application
	// stays in its namespace, one about a volume lives in a namespace the
	// installation does not hold a Role for, and one event is older than the
	// window C-64 fixes.
	const elsewhere = "shop-elsewhere"
	world.EnsureNamespace(t, elsewhere)
	now := readTime
	world.Create(t, &unstructured.Unstructured{Object: pageApplication("shop", "alice", "bob")})
	world.CreateIn(t, elsewhere, harness.KernelVolume(elsewhere, "data", "node-local", "1Gi", "local", "alice", "acceptance"))
	world.CreateClusterObject(t, "", "v1", "events", harness.Namespace,
		pageEvent("shop-newest", "kos.tsic.top/v1beta1", "Application", harness.Namespace, "shop", now.Add(-5*time.Minute)))
	world.CreateClusterObject(t, "", "v1", "events", harness.Namespace,
		pageEvent("shop-older", "kos.tsic.top/v1beta1", "Application", harness.Namespace, "shop", now.Add(-50*time.Minute)))
	world.CreateClusterObject(t, "", "v1", "events", harness.Namespace,
		pageEvent("shop-before-window", "kos.tsic.top/v1beta1", "Application", harness.Namespace, "shop", now.Add(-2*time.Hour)))
	world.CreateClusterObject(t, "", "v1", "events", elsewhere,
		pageEvent("volume-elsewhere", "kos.tsic.top/v1beta1", "Volume", elsewhere, "data", now.Add(-10*time.Minute)))

	// The pages read as the control plane's own identity, so the ClusterRole the
	// installation rendered decides the read; the test's cluster-admin does not.
	pageSource := &taskmanager.Readings{
		Dynamic: world.DynamicAs(t, harness.ControlPlaneIdentity()),
		Now:     func() time.Time { return now },
	}
	owner := identity.Session{Username: "owner", Role: write.RoleOwner}
	for _, page := range []struct {
		id    string
		items []extension.PageItem
	}{
		{id: taskmanager.PageOverview, items: taskmanager.OverviewPage().Items},
		{id: taskmanager.PageDetail, items: taskmanager.DetailPage().Items},
	} {
		if len(page.items) != 7 {
			t.Fatalf("page %s declares %d items, want 7", page.id, len(page.items))
		}
		for _, item := range page.items {
			item := item
			t.Run(page.id+"/"+item.ID, func(t *testing.T) {
				if _, failure := pageSource.Read(context.Background(), owner, item); failure != nil {
					t.Fatalf("read = %s: %s", failure.Code, failure.Message)
				}
			})
		}
	}

	// The overview reads the last hour newest first and drops the older event,
	// across namespaces (KOSD-AC-59).
	recent := pageItemValue(t, pageSource, owner, extension.PageItem{ID: taskmanager.ItemRecentEvents})
	if window := text(recent["window"]); window != controlplane.EventWindow.String() {
		t.Fatalf("window = %q, want %q", window, controlplane.EventWindow.String())
	}
	want := []string{"shop-newest", "volume-elsewhere", "shop-older"}
	if names := eventNamesOf(t, recent); !slices.Equal(names, want) {
		t.Fatalf("events = %v, want %v", names, want)
	}
	detail := pageItemValue(t, pageSource, owner, extension.PageItem{ID: taskmanager.ItemDetailEvents})
	row := detailRowOf(t, detail, "shop")
	if names := eventNamesOf(t, row); !slices.Equal(names, []string{"shop-newest", "shop-older"}) {
		t.Fatalf("detail events = %v, want the application's window", names)
	}

	// C-64's visibility: a non-application object's event reaches the Owner and
	// the member owning the object, never another member or a Guest. The
	// application's events follow the application's own visibility (C-59).
	page := &cluster.Reader{Dynamic: pageSource.Dynamic, Now: func() time.Time { return now }}
	for _, session := range []struct {
		name string
		who  identity.Session
		want []string
	}{
		{name: "owner", who: owner, want: want},
		{name: "owning-member", who: identity.Session{Username: "alice", Role: write.RoleMember}, want: want},
		{name: "listed-member", who: identity.Session{Username: "bob", Role: write.RoleMember}, want: []string{"shop-newest", "shop-older"}},
		{name: "guest", who: identity.Session{Username: "guest", Role: write.RoleGuest}},
	} {
		session := session
		t.Run("visibility/"+session.name, func(t *testing.T) {
			events, failure := page.Events(context.Background(), session.who, "", 0)
			if failure != nil {
				t.Fatalf("event read = %s: %s", failure.Code, failure.Message)
			}
			names := make([]string, 0, len(events))
			for _, event := range events {
				names = append(names, event.Name)
			}
			if !slices.Equal(names, session.want) {
				t.Fatalf("events = %v, want %v", names, session.want)
			}
		})
	}

	// The installed role carries only read verbs on cluster-scoped core facts
	// (KOSD-C-04).
	role, err := world.Kube.RbacV1().ClusterRoles().Get(context.Background(), install.ControlPlaneName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the installed cluster role: %v", err)
	}
	eventsRead := false
	for _, policyRule := range role.Rules {
		if slices.Contains(policyRule.APIGroups, api.Group) {
			continue
		}
		for _, verb := range policyRule.Verbs {
			if verb == "*" || verb == "create" || verb == "update" || verb == "patch" || verb == "delete" {
				t.Fatalf("the installed cluster role grants %q on %v; cluster-scoped writes belong to the install path", verb, policyRule.APIGroups)
			}
		}
		if slices.Contains(policyRule.APIGroups, "") && slices.Contains(policyRule.Resources, "events") {
			if !slices.Equal(policyRule.Verbs, []string{"get", "list", "watch"}) {
				t.Fatalf("events verbs = %v, want get, list and watch", policyRule.Verbs)
			}
			eventsRead = true
		}
	}
	if !eventsRead {
		t.Fatalf("the installed cluster role grants no cluster-scoped events read")
	}
}

// pageApplication is an application with an owner and a member list.
func pageApplication(name, owner string, members ...string) map[string]any {
	listed := make([]any, 0, len(members))
	for _, member := range members {
		listed = append(listed, member)
	}
	return map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name": name, "namespace": harness.Namespace,
			"labels": map[string]any{"managed-by": "acceptance"},
		},
		"spec": map[string]any{
			"namespace": harness.Namespace,
			"owner":     owner,
			"visibility": map[string]any{
				"members":     listed,
				"allowGuests": false,
			},
		},
	}
}

// pageEvent is one event about a named object.
func pageEvent(name, apiVersion, kind, namespace, object string, lastSeen time.Time) map[string]any {
	stamp := lastSeen.UTC().Format(time.RFC3339Nano)
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Event",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"involvedObject": map[string]any{
			"apiVersion": apiVersion, "kind": kind, "namespace": namespace, "name": object,
		},
		"type":               "Warning",
		"reason":             "AcceptanceEvent",
		"message":            "an event the page reads",
		"count":              1,
		"firstTimestamp":     stamp,
		"lastTimestamp":      stamp,
		"reportingComponent": "kosd",
		"reportingInstance":  "kosd-1",
	}
}

// pageItemValue reads one page item and returns its JSON projection.
func pageItemValue(t *testing.T, readings *taskmanager.Readings, session identity.Session, item extension.PageItem) map[string]any {
	t.Helper()
	reading, failure := readings.Read(context.Background(), session, item)
	if failure != nil {
		t.Fatalf("read %s = %s: %s", item.ID, failure.Code, failure.Message)
	}
	encoded, err := json.Marshal(reading.Value)
	if err != nil {
		t.Fatalf("encode %s: %v", item.ID, err)
	}
	decoded := map[string]any{}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode %s: %v", item.ID, err)
	}
	return decoded
}

// eventNamesOf reads the event names of one projection in order.
func eventNamesOf(t *testing.T, projection map[string]any) []string {
	t.Helper()
	raw, _ := projection["events"].([]any)
	names := make([]string, 0, len(raw))
	for _, entry := range raw {
		item, _ := entry.(map[string]any)
		names = append(names, text(item["name"]))
	}
	return names
}

// detailRowOf returns the detail row of one application.
func detailRowOf(t *testing.T, projection map[string]any, name string) map[string]any {
	t.Helper()
	rows, _ := projection["applications"].([]any)
	for _, entry := range rows {
		row, _ := entry.(map[string]any)
		if text(row["name"]) == name {
			return row
		}
	}
	t.Fatalf("detail page has no row for %s: %v", name, projection)
	return nil
}
