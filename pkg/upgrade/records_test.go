package upgrade_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/identity"
	"github.com/tsic404/kosd/pkg/upgrade"
	"github.com/tsic404/kosd/pkg/write"
)

var (
	ownerSession  = identity.Session{Username: "owner", Role: write.RoleOwner}
	memberSession = identity.Session{Username: "member", Role: write.RoleMember}
	guestSession  = identity.Session{Username: "guest", Role: write.RoleGuest}
)

// platformStub answers the platform preflight and can change its answer between
// calls, which is how a case moves the cluster between the preflight and the
// upgrade write.
type platformStub struct {
	// reports are returned in order; the last one repeats.
	reports []upgrade.Report
	// failure, when set, is returned instead of a report.
	failure *controlplane.Error
	// calls counts the evaluations so a case can read the newest answer.
	calls int
}

func (s *platformStub) Preflight(context.Context, upgrade.Request) (upgrade.Report, *controlplane.Error) {
	if s.failure != nil {
		return upgrade.Report{}, s.failure
	}
	index := s.calls
	if index >= len(s.reports) {
		index = len(s.reports) - 1
	}
	s.calls++
	return s.reports[index], nil
}

// extensionStub answers the extension preflight.
type extensionStub struct {
	// preflight is the answer; failure is returned instead when set.
	preflight extension.UpgradePreflight
	failure   error
	// targets is the last target set the domain submitted.
	targets []extension.UpgradeTarget
}

func (s *extensionStub) UpgradePreflight(_ context.Context, targets []extension.UpgradeTarget) (extension.UpgradePreflight, error) {
	s.targets = targets
	if s.failure != nil {
		return extension.UpgradePreflight{}, s.failure
	}
	return s.preflight, nil
}

// installedStub reports the window applications installed components declare.
type installedStub []extension.InstalledApp

func (s installedStub) Apps() []extension.InstalledApp { return s }

// openStore opens a private control-plane database for one case.
func openStore(t *testing.T) *identity.Store {
	t.Helper()
	store, err := identity.Open(context.Background(), identity.OpenOptions{Path: filepath.Join(t.TempDir(), "kos.db")})
	if err != nil {
		t.Fatalf("open the control-plane database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// clock is a movable instant the record times come from.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

// platformRequest is one platform preflight.
func platformRequest(version string) upgrade.PreflightRequest {
	return upgrade.PreflightRequest{
		Kind:          upgrade.KindPlatform,
		TargetVersion: version,
		Downtime:      "15m",
		Nodes:         []string{"node-a", "node-b"},
	}
}

// TestPreflightKeepsOneRecordPerRequest pins the record lifecycle: one request
// keeps one record, a later request supersedes it without deleting it, and a
// record observed beyond C-13's bound reads as stale.
func TestPreflightKeepsOneRecordPerRequest(t *testing.T) {
	ctx := context.Background()
	now := &clock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	platform := &platformStub{reports: []upgrade.Report{{
		Nodes: []upgrade.NodePlan{
			{Node: "node-a", Applications: []string{"apps/shop"}, Downtime: "15m"},
			{Node: "node-b", Applications: []string{"apps/cache"}, Downtime: "15m"},
		},
	}}}
	service := upgrade.New(upgrade.Config{Store: openStore(t), Platform: platform, Now: now.Now})

	first, failure := service.Preflight(ctx, ownerSession, platformRequest("v1.38.0"))
	if failure != nil {
		t.Fatalf("the first preflight: %v", failure)
	}
	if !reflect.DeepEqual(first.Result.AffectedApplications, []string{"apps/cache", "apps/shop"}) {
		t.Fatalf("affected applications = %v, want both nodes' applications", first.Result.AffectedApplications)
	}
	if first.Result.DeclaredDowntime != "15m" || first.Result.EstimatedDowntime != "15m" {
		t.Fatalf("record downtime = %q/%q, want the declared estimate", first.Result.DeclaredDowntime, first.Result.EstimatedDowntime)
	}
	repeated, failure := service.Preflight(ctx, ownerSession, platformRequest("v1.38.0"))
	if failure != nil {
		t.Fatalf("the repeated preflight: %v", failure)
	}
	if repeated.ID != first.ID {
		t.Fatalf("a repeated request produced %q, want the stored %q", repeated.ID, first.ID)
	}
	if records := recordsOf(t, service); len(records) != 1 {
		t.Fatalf("the repeated request left %d records, want one", len(records))
	}

	second, failure := service.Preflight(ctx, ownerSession, platformRequest("v1.39.0"))
	if failure != nil {
		t.Fatalf("the second preflight: %v", failure)
	}
	records := recordsOf(t, service)
	if len(records) != 2 {
		t.Fatalf("the second preflight left %d records, want two", len(records))
	}
	if records[0].ID != second.ID || records[1].ID != first.ID {
		t.Fatalf("records = %q, %q, want newest %q first", records[0].ID, records[1].ID, second.ID)
	}
	if records[1].IsSuperseded == false {
		t.Fatal("the earlier record is not marked stale by the later preflight")
	}
	if !reflect.DeepEqual(records[1].Result, first.Result) {
		t.Fatalf("the superseded record's result changed: %v, want %v", records[1].Result, first.Result)
	}
	if records[0].IsSuperseded {
		t.Fatal("the newest record is marked stale")
	}

	now.now = now.now.Add(controlplane.StaleThreshold + time.Second)
	aged := recordsOf(t, service)
	if !aged[0].Stale || !aged[1].Stale {
		t.Fatalf("records observed beyond the bound read stale=%v/%v, want true", aged[0].Stale, aged[1].Stale)
	}
}

// TestPreflightDeduplicatesOnlyTheNewestRequest documents the dedupe scope: the
// newest record answers a repeat, and a request that an intervening preflight
// already superseded is a new preflight when it comes back.
func TestPreflightDeduplicatesOnlyTheNewestRequest(t *testing.T) {
	ctx := context.Background()
	platform := &platformStub{reports: []upgrade.Report{{Nodes: []upgrade.NodePlan{{Node: "node-a"}}}}}
	service := upgrade.New(upgrade.Config{Store: openStore(t), Platform: platform})

	first, failure := service.Preflight(ctx, ownerSession, platformRequest("v1.38.0"))
	if failure != nil {
		t.Fatalf("the first preflight: %v", failure)
	}
	if _, failure := service.Preflight(ctx, ownerSession, platformRequest("v1.39.0")); failure != nil {
		t.Fatalf("the second preflight: %v", failure)
	}
	again, failure := service.Preflight(ctx, ownerSession, platformRequest("v1.38.0"))
	if failure != nil {
		t.Fatalf("the returning preflight: %v", failure)
	}
	if again.ID == first.ID {
		t.Fatalf("a superseded request kept record %q, want a new preflight", again.ID)
	}
	if records := recordsOf(t, service); len(records) != 3 {
		t.Fatalf("the returning preflight left %d records, want three", len(records))
	}
}

// TestUpgradeMergesNodeResultsIntoTheReferencedRecord pins the upgrade write:
// every node's result lands on one record, a repeated node replaces its own
// entry, and a missing record is refused with the reason.
func TestUpgradeMergesNodeResultsIntoTheReferencedRecord(t *testing.T) {
	ctx := context.Background()
	platform := &platformStub{reports: []upgrade.Report{{
		Nodes: []upgrade.NodePlan{{Node: "node-a", Applications: []string{"apps/shop"}}},
	}}}
	service := upgrade.New(upgrade.Config{Store: openStore(t), Platform: platform})
	record, failure := service.Preflight(ctx, ownerSession, platformRequest("v1.38.0"))
	if failure != nil {
		t.Fatalf("the preflight: %v", failure)
	}

	upgraded, failure := service.Upgrade(ctx, ownerSession, upgrade.UpgradeRequest{
		RecordID: record.ID,
		Nodes:    []upgrade.NodeUpgrade{{Node: "node-b", Result: "restored"}},
	})
	if failure != nil {
		t.Fatalf("the first upgrade write: %v", failure)
	}
	if upgraded.Upgrade == nil || upgraded.Upgrade.RecordID != record.ID {
		t.Fatalf("the upgrade write does not reference the record: %v", upgraded.Upgrade)
	}
	upgraded, failure = service.Upgrade(ctx, ownerSession, upgrade.UpgradeRequest{
		RecordID: record.ID,
		Nodes:    []upgrade.NodeUpgrade{{Node: "node-a", Result: "restored"}},
	})
	if failure != nil {
		t.Fatalf("the second upgrade write: %v", failure)
	}
	if got := nodesOf(upgraded); !reflect.DeepEqual(got, []string{"node-a", "node-b"}) {
		t.Fatalf("the record carries node results %v, want both nodes", got)
	}
	upgraded, failure = service.Upgrade(ctx, ownerSession, upgrade.UpgradeRequest{
		RecordID: record.ID,
		Nodes:    []upgrade.NodeUpgrade{{Node: "node-a", Result: "failed"}},
	})
	if failure != nil {
		t.Fatalf("the repeated upgrade write: %v", failure)
	}
	if got := nodesOf(upgraded); !reflect.DeepEqual(got, []string{"node-a", "node-b"}) {
		t.Fatalf("a repeated node result left %v, want one entry per node", got)
	}
	if upgraded.Upgrade.Nodes[0].Result != "failed" {
		t.Fatalf("the repeated node result = %q, want the newest %q", upgraded.Upgrade.Nodes[0].Result, "failed")
	}
	if records := recordsOf(t, service); len(records) != 1 {
		t.Fatalf("the upgrade writes left %d records, want one", len(records))
	}

	_, failure = service.Upgrade(ctx, ownerSession, upgrade.UpgradeRequest{RecordID: "preflight-42"})
	if failure == nil || failure.Code != controlplane.CodeNotFound {
		t.Fatalf("an upgrade from a missing record = %v, want %q", failure, controlplane.CodeNotFound)
	}
	if records := recordsOf(t, service); len(records) != 1 {
		t.Fatalf("the refused upgrade left %d records, want one", len(records))
	}
}

// TestUpgradeComparesTheRecordWithTheCluster checks C-88's read-time
// comparison: the difference is recorded and never blocks the write, and an
// unreadable cluster is recorded as uncompared.
func TestUpgradeComparesTheRecordWithTheCluster(t *testing.T) {
	ctx := context.Background()
	platform := &platformStub{reports: []upgrade.Report{
		{Nodes: []upgrade.NodePlan{{Node: "node-a", Applications: []string{"apps/shop"}}}},
		{Nodes: []upgrade.NodePlan{{Node: "node-a", Applications: []string{"apps/shop", "apps/cache"}}}},
	}}
	service := upgrade.New(upgrade.Config{Store: openStore(t), Platform: platform})
	record, failure := service.Preflight(ctx, ownerSession, platformRequest("v1.38.0"))
	if failure != nil {
		t.Fatalf("the preflight: %v", failure)
	}
	upgraded, failure := service.Upgrade(ctx, ownerSession, upgrade.UpgradeRequest{RecordID: record.ID})
	if failure != nil {
		t.Fatalf("the upgrade write: %v", failure)
	}
	if len(upgraded.Upgrade.Differences) == 0 {
		t.Fatal("the moved cluster was not recorded as a difference")
	}

	platform.failure = &controlplane.Error{Code: controlplane.CodeClusterUnavailable, Message: "集群 API 暂不可达"}
	uncompared, failure := service.Upgrade(ctx, ownerSession, upgrade.UpgradeRequest{RecordID: record.ID})
	if failure != nil {
		t.Fatalf("the upgrade write over an unreadable cluster: %v", failure)
	}
	if len(uncompared.Upgrade.Differences) == 0 {
		t.Fatal("an unreadable cluster was not recorded as uncompared")
	}
}

// TestExtensionPreflightRecordsContributionDiff checks C-88's extension half:
// the record keeps each target's contribution difference and the applications
// the upgraded components provide.
func TestExtensionPreflightRecordsContributionDiff(t *testing.T) {
	ctx := context.Background()
	extensions := &extensionStub{preflight: extension.UpgradePreflight{Components: []extension.ComponentUpgrade{{
		Component: "kos-updater",
		From:      "1.0.0",
		To:        "2.0.0",
		Changes: []extension.ContributionChange{{
			Class:   extension.ClassPages,
			Removed: []string{"updates"},
			Added:   []string{"updates-v2"},
		}},
	}}}}
	installed := installedStub{{
		Component: "kos-updater",
		Extension: "kos-updater",
		Manifest:  extension.App{ID: "updates"},
	}}
	service := upgrade.New(upgrade.Config{Store: openStore(t), Extensions: extensions, Installed: installed})
	record, failure := service.Preflight(ctx, ownerSession, upgrade.PreflightRequest{
		Kind:     upgrade.KindExtension,
		Downtime: "5m",
		Targets:  []upgrade.Target{{Component: "kos-updater", Reference: "oci://updater:2.0.0"}},
	})
	if failure != nil {
		t.Fatalf("the extension preflight: %v", failure)
	}
	if len(record.Result.Components) != 1 {
		t.Fatalf("record components = %v, want the upgraded component", record.Result.Components)
	}
	changes := record.Result.Components[0].Changes
	if len(changes) != 1 || changes[0].Class != extension.ClassPages ||
		!reflect.DeepEqual(changes[0].Removed, []string{"updates"}) || !reflect.DeepEqual(changes[0].Added, []string{"updates-v2"}) {
		t.Fatalf("contribution changes = %+v, want the page difference", changes)
	}
	if !reflect.DeepEqual(record.Result.AffectedApplications, []string{"updates"}) {
		t.Fatalf("affected applications = %v, want the upgraded component's application", record.Result.AffectedApplications)
	}
	if len(extensions.targets) != 1 || extensions.targets[0].Component != "kos-updater" {
		t.Fatalf("preflight targets = %+v, want the submitted component", extensions.targets)
	}

	broken := &extensionStub{failure: errors.New("component kos-updater is not an installed system component")}
	brokenService := upgrade.New(upgrade.Config{Store: openStore(t), Extensions: broken})
	_, failure = brokenService.Preflight(ctx, ownerSession, upgrade.PreflightRequest{
		Kind:     upgrade.KindExtension,
		Downtime: "5m",
		Targets:  []upgrade.Target{{Component: "kos-updater", Reference: "oci://updater:2.0.0"}},
	})
	if failure == nil || failure.Code != controlplane.CodeInvalidField {
		t.Fatalf("a refused extension preflight = %v, want %q", failure, controlplane.CodeInvalidField)
	}
	if records := recordsOf(t, brokenService); len(records) != 0 {
		t.Fatalf("a refused extension preflight left %d records, want none", len(records))
	}
}

// TestNonOwnerWritesAreRefusedWithoutARecord checks C-59's new clause: both
// writes are the Owner's, and a Member or Guest attempt stores nothing.
func TestNonOwnerWritesAreRefusedWithoutARecord(t *testing.T) {
	ctx := context.Background()
	platform := &platformStub{reports: []upgrade.Report{{Nodes: []upgrade.NodePlan{{Node: "node-a"}}}}}
	service := upgrade.New(upgrade.Config{Store: openStore(t), Platform: platform})
	for _, session := range []identity.Session{memberSession, guestSession} {
		_, failure := service.Preflight(ctx, session, platformRequest("v1.38.0"))
		if failure == nil || failure.Code != controlplane.CodePermissionDenied {
			t.Fatalf("%s's preflight = %v, want %q", session.Role, failure, controlplane.CodePermissionDenied)
		}
	}
	if records := recordsOf(t, service); len(records) != 0 {
		t.Fatalf("refused preflights left %d records, want none", len(records))
	}
	record, failure := service.Preflight(ctx, ownerSession, platformRequest("v1.38.0"))
	if failure != nil {
		t.Fatalf("the owner's preflight: %v", failure)
	}
	for _, session := range []identity.Session{memberSession, guestSession} {
		_, failure := service.Upgrade(ctx, session, upgrade.UpgradeRequest{RecordID: record.ID})
		if failure == nil || failure.Code != controlplane.CodePermissionDenied {
			t.Fatalf("%s's upgrade write = %v, want %q", session.Role, failure, controlplane.CodePermissionDenied)
		}
	}
	records := recordsOf(t, service)
	if len(records) != 1 || records[0].Upgrade != nil {
		t.Fatalf("a refused upgrade write changed the record: %+v", records)
	}
}

// recordsOf reads the stored records.
func recordsOf(t *testing.T, service *upgrade.Service) []upgrade.Record {
	t.Helper()
	records, err := service.Records(context.Background())
	if err != nil {
		t.Fatalf("read the records: %v", err)
	}
	return records
}

// nodesOf reads one run's node names in order.
func nodesOf(record upgrade.Record) []string {
	if record.Upgrade == nil {
		return nil
	}
	nodes := make([]string, 0, len(record.Upgrade.Nodes))
	for _, node := range record.Upgrade.Nodes {
		nodes = append(nodes, node.Node)
	}
	return nodes
}
