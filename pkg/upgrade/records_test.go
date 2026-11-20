package upgrade_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
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

// frameworkStub answers the framework API version preflight (C-16).
type frameworkStub struct {
	// preflight is the answer; failure is returned instead when set.
	preflight extension.FrameworkPreflight
	failure   error
	// targets is the last target set the domain submitted and calls counts the
	// submissions, so a case can tell whether the axis was consulted.
	targets []string
	calls   int
}

func (s *frameworkStub) Preflight(_ context.Context, target []string) (extension.FrameworkPreflight, error) {
	s.calls++
	s.targets = target
	if s.failure != nil {
		return extension.FrameworkPreflight{}, s.failure
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

// TestPlatformPreflightRecordsFrameworkDependents pins C-88's two framework
// items: the preflight write stores the removed framework API versions with the
// installed extensions that depend on them, on the framework API version axis
// beside the node version TargetVersion names. A request that declares no
// framework target set moves nothing on that axis and keeps both items empty.
func TestPlatformPreflightRecordsFrameworkDependents(t *testing.T) {
	ctx := context.Background()
	platform := &platformStub{reports: []upgrade.Report{{Nodes: []upgrade.NodePlan{{Node: "node-a"}}}}}
	frameworks := &frameworkStub{preflight: extension.FrameworkPreflight{
		Target:  []string{"2.0"},
		Removed: []string{"1.0", "1.1"},
		Dependents: []extension.FrameworkDependent{{
			Component: "kubevirt", Identifier: "thirdparty.kubevirt", FrameworkVersion: ">=1.0 <2.0",
		}},
		IsBlocked: true,
	}}
	service := upgrade.New(upgrade.Config{Store: openStore(t), Platform: platform, Frameworks: frameworks})

	request := platformRequest("v1.38.0")
	request.FrameworkTargets = []string{"2.0"}
	record, failure := service.Preflight(ctx, ownerSession, request)
	if failure != nil {
		t.Fatalf("the preflight: %v", failure)
	}
	if got := record.Result.RemovedFrameworkVersions; !reflect.DeepEqual(got, []string{"1.0", "1.1"}) {
		t.Fatalf("removed framework API versions = %v, want the removed set", got)
	}
	if record.Result.TargetVersion != "v1.38.0" {
		t.Fatalf("record target version = %q, want the node version", record.Result.TargetVersion)
	}
	want := upgrade.FrameworkDependent{Component: "kubevirt", Identifier: "thirdparty.kubevirt", FrameworkVersion: ">=1.0 <2.0"}
	if len(record.Result.FrameworkDependents) != 1 || record.Result.FrameworkDependents[0] != want {
		t.Fatalf("framework dependents = %v, want the stranded extension %+v", record.Result.FrameworkDependents, want)
	}
	if !record.Result.IsBlocked {
		t.Fatal("a record with a framework dependent is not blocked")
	}
	if !reflect.DeepEqual(frameworks.targets, []string{"2.0"}) {
		t.Fatalf("framework targets = %v, want the declared target set", frameworks.targets)
	}

	undeclared, failure := service.Preflight(ctx, ownerSession, platformRequest("v1.40.0"))
	if failure != nil {
		t.Fatalf("the preflight without a declared framework target: %v", failure)
	}
	if len(undeclared.Result.RemovedFrameworkVersions) != 0 || len(undeclared.Result.FrameworkDependents) != 0 {
		t.Fatalf("a preflight without a declared target recorded %v/%v, want both empty",
			undeclared.Result.RemovedFrameworkVersions, undeclared.Result.FrameworkDependents)
	}
	if frameworks.calls != 1 {
		t.Fatalf("the framework axis was consulted %d times, want only for the declared request", frameworks.calls)
	}
}

// TestPlatformPreflightMergesFrameworkBlocking pins C-88's single isBlocked:
// a framework dependent blocks the stored record beside the node preflight's
// own verdict, a cleared dependency stops blocking, and a node item the flow
// cannot judge keeps the record blocked while the framework axis is clear.
func TestPlatformPreflightMergesFrameworkBlocking(t *testing.T) {
	ctx := context.Background()
	platform := &platformStub{reports: []upgrade.Report{
		{Nodes: []upgrade.NodePlan{{Node: "node-a"}}},
		{Nodes: []upgrade.NodePlan{{Node: "node-a"}}},
		{
			Nodes:        []upgrade.NodePlan{{Node: "node-a"}},
			Undetermined: []string{"卷 apps/data 没有记录数据所在节点"},
			Blocked:      true,
		},
	}}
	frameworks := &frameworkStub{preflight: extension.FrameworkPreflight{
		Target:  []string{"2.0"},
		Removed: []string{"1.1"},
		Dependents: []extension.FrameworkDependent{{
			Component: "kubevirt", Identifier: "thirdparty.kubevirt", FrameworkVersion: ">=1.0 <2.0",
		}},
		IsBlocked: true,
	}}
	service := upgrade.New(upgrade.Config{Store: openStore(t), Platform: platform, Frameworks: frameworks})

	blocked := platformRequest("v1.38.0")
	blocked.FrameworkTargets = []string{"2.0"}
	record, failure := service.Preflight(ctx, ownerSession, blocked)
	if failure != nil {
		t.Fatalf("the preflight over a stranded extension: %v", failure)
	}
	if !record.Result.IsBlocked {
		t.Fatalf("record isBlocked = %v with dependents %v, want true", record.Result.IsBlocked, record.Result.FrameworkDependents)
	}
	if stored := recordsOf(t, service)[0]; !stored.Result.IsBlocked {
		t.Fatal("the stored record lost the framework block")
	}

	// Releasing the dependency clears the block on a later preflight.
	frameworks.preflight = extension.FrameworkPreflight{Target: []string{"2.0"}}
	cleared := platformRequest("v1.39.0")
	cleared.FrameworkTargets = []string{"2.0"}
	released, failure := service.Preflight(ctx, ownerSession, cleared)
	if failure != nil {
		t.Fatalf("the preflight after the dependency left: %v", failure)
	}
	if released.Result.IsBlocked || len(released.Result.FrameworkDependents) != 0 {
		t.Fatalf("the record is still blocked: %+v", released.Result)
	}

	// A node preflight that blocks on its own keeps its verdict; the clear
	// framework axis does not overwrite it.
	own := platformRequest("v1.40.0")
	own.FrameworkTargets = []string{"2.0"}
	nodeBlocked, failure := service.Preflight(ctx, ownerSession, own)
	if failure != nil {
		t.Fatalf("the preflight over an undetermined node flow: %v", failure)
	}
	if !nodeBlocked.Result.IsBlocked || len(nodeBlocked.Result.Undetermined) == 0 {
		t.Fatalf("the node preflight's own block was lost: blocked=%v undetermined=%v",
			nodeBlocked.Result.IsBlocked, nodeBlocked.Result.Undetermined)
	}
}

// TestPlatformPreflightRefusesAnUnreadableFrameworkAxis checks that a preflight
// which cannot judge the framework axis stores no record rather than one that
// claims nothing is removed (C-88).
func TestPlatformPreflightRefusesAnUnreadableFrameworkAxis(t *testing.T) {
	ctx := context.Background()
	platform := &platformStub{reports: []upgrade.Report{{Nodes: []upgrade.NodePlan{{Node: "node-a"}}}}}
	broken := &frameworkStub{failure: errors.New("list system components: cluster unreachable")}
	service := upgrade.New(upgrade.Config{Store: openStore(t), Platform: platform, Frameworks: broken})

	request := platformRequest("v1.38.0")
	request.FrameworkTargets = []string{"2.0"}
	_, failure := service.Preflight(ctx, ownerSession, request)
	if failure == nil || failure.Code != controlplane.CodeClusterUnavailable {
		t.Fatalf("a preflight over an unreadable framework axis = %v, want %q", failure, controlplane.CodeClusterUnavailable)
	}
	if records := recordsOf(t, service); len(records) != 0 {
		t.Fatalf("the refused preflight left %d records, want none", len(records))
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

// TestUpgradeReportsAChangedDependentDeclaration pins C-88's comparison of the
// framework axis: a stranded extension whose recorded target range changed is a
// difference the upgrade entry reports, even though its component name, the
// removed set and the block stayed the same. An unchanged declaration yields
// none.
func TestUpgradeReportsAChangedDependentDeclaration(t *testing.T) {
	ctx := context.Background()
	platform := &platformStub{reports: []upgrade.Report{
		{Nodes: []upgrade.NodePlan{{Node: "node-a"}}},
		{Nodes: []upgrade.NodePlan{{Node: "node-a"}}},
	}}
	frameworks := &frameworkStub{preflight: extension.FrameworkPreflight{
		Target:  []string{"2.0"},
		Removed: []string{"1.1"},
		Dependents: []extension.FrameworkDependent{{
			Component: "kubevirt", Identifier: "thirdparty.kubevirt", FrameworkVersion: ">=1.0 <2.0",
		}},
		IsBlocked: true,
	}}
	service := upgrade.New(upgrade.Config{Store: openStore(t), Platform: platform, Frameworks: frameworks})
	request := platformRequest("v1.38.0")
	request.FrameworkTargets = []string{"2.0"}
	record, failure := service.Preflight(ctx, ownerSession, request)
	if failure != nil {
		t.Fatalf("the preflight: %v", failure)
	}

	// The same declaration reports no difference.
	unchanged, failure := service.Upgrade(ctx, ownerSession, upgrade.UpgradeRequest{RecordID: record.ID})
	if failure != nil {
		t.Fatalf("the upgrade write over an unchanged declaration: %v", failure)
	}
	if len(unchanged.Upgrade.Differences) != 0 {
		t.Fatalf("an unchanged dependent declaration reported %v, want no difference", unchanged.Upgrade.Differences)
	}

	// The component keeps its name and stays a dependent, but its declared
	// range moved: the recorded basis changed and must be reported.
	frameworks.preflight.Dependents[0].FrameworkVersion = ">=1.1 <2.0"
	changed, failure := service.Upgrade(ctx, ownerSession, upgrade.UpgradeRequest{RecordID: record.ID})
	if failure != nil {
		t.Fatalf("the upgrade write over a changed declaration: %v", failure)
	}
	if len(changed.Upgrade.Differences) == 0 {
		t.Fatal("a changed dependent declaration was not reported as a difference")
	}
	joined := strings.Join(changed.Upgrade.Differences, "\n")
	if !strings.Contains(joined, ">=1.0 <2.0") || !strings.Contains(joined, ">=1.1 <2.0") {
		t.Fatalf("the difference %q does not name both declared ranges", joined)
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
