package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/identity"
	"github.com/tsic404/kosd/pkg/write"
)

// The upgrade preflight record of C-88: one row per preflight, holding the
// request and the result the control plane computed. The control plane owns the
// record and the two Owner writes around it; executing the upgrade stays with
// the upgrade flow.

// Kind names what one upgrade moves: the platform's nodes or the packages of
// installed extensions.
type Kind string

const (
	// KindPlatform moves the platform's nodes to the target version.
	KindPlatform Kind = "platform"
	// KindExtension replaces the packages of installed components.
	KindExtension Kind = "extension"
)

// Audit vocabulary of the two writes (C-14). The record is not a kernel object
// class: the kind only names the trail entry, the way the settings domain names
// its item.
const (
	// auditKind names an upgrade preflight record in the audit trail.
	auditKind = "UpgradePreflight"
	// VerbPreflight marks the write that computed and stored the preflight.
	VerbPreflight = "preflight"
	// VerbUpgrade marks the write that referenced a stored record.
	VerbUpgrade = "upgrade"
	// auditRequestName stands in for the object name of a refused preflight:
	// no record exists yet, and a trail entry still names its subject.
	auditRequestName = "request"
)

// maxReferenceLength bounds one package reference a target names.
const maxReferenceLength = 512

// PreflightRequest is what the Owner submits to the preflight write (C-88): the
// target and the declared downtime, plus what the upgrade moves — the nodes a
// platform upgrade takes out, or the packages an extension upgrade replaces.
type PreflightRequest struct {
	// Kind selects the preflight the write runs.
	Kind Kind `json:"kind"`
	// TargetVersion is the version the platform upgrade moves the nodes to.
	TargetVersion string `json:"targetVersion,omitempty"`
	// Downtime is the declared estimate of the outage, as Go parses it ("15m").
	Downtime string `json:"downtime"`
	// Nodes are the nodes a platform upgrade takes out, in the order it takes
	// them.
	Nodes []string `json:"nodes,omitempty"`
	// Targets are the component packages an extension upgrade replaces.
	Targets []Target `json:"targets,omitempty"`
}

// Target names one component package an extension upgrade moves to (C-55).
type Target struct {
	// Component is the installed system component the target belongs to.
	Component string `json:"component"`
	// Reference addresses the target package.
	Reference string `json:"reference"`
	// Digest pins the target package content when the caller knows it.
	Digest string `json:"digest,omitempty"`
}

// NodeOutcome is what one node's platform preflight reported (C-55).
type NodeOutcome struct {
	// Node is the node the upgrade takes out.
	Node string `json:"node"`
	// Applications are the node-local applications the outage stops.
	Applications []string `json:"applications"`
	// Volumes are the node-local volumes whose data sits on the node.
	Volumes []string `json:"volumes,omitempty"`
}

// ComponentOutcome is what one component's extension preflight reported (C-55),
// including the item-by-item contribution difference C-88 records.
type ComponentOutcome struct {
	// Component is the installed system component that moves.
	Component string `json:"component"`
	// From and To are the package references before and after.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Changes are the contributions the target drops and adds.
	Changes []extension.ContributionChange `json:"changes,omitempty"`
	// Problems are the checks the target would fail.
	Problems []string `json:"problems,omitempty"`
	// IsBlocked reports whether this component blocks the upgrade.
	IsBlocked bool `json:"isBlocked,omitempty"`
}

// PreflightResult is the result the preflight write records beside the request
// (C-88).
type PreflightResult struct {
	// TargetVersion is the version the preflight judged for.
	TargetVersion string `json:"targetVersion,omitempty"`
	// DeclaredDowntime is the estimate the Owner submitted.
	DeclaredDowntime string `json:"declaredDowntime"`
	// EstimatedDowntime is the estimate the preflight reports. The flow has no
	// measurement of its own, so it carries the declared value, which is what
	// the maintenance preflight reports as the per-node estimate (C-55, C-65).
	EstimatedDowntime string `json:"estimatedDowntime"`
	// AffectedApplications are the applications the outage takes out of
	// service, sorted and without repeats.
	AffectedApplications []string `json:"affectedApplications"`
	// Nodes carries one plan per node a platform upgrade takes out.
	Nodes []NodeOutcome `json:"nodes,omitempty"`
	// Components carries one entry per extension target.
	Components []ComponentOutcome `json:"components,omitempty"`
	// Undetermined are the items the preflight could not judge; any of them
	// blocks the upgrade (C-55, C-65).
	Undetermined []string `json:"undetermined,omitempty"`
	// IsBlocked reports whether an item stops the upgrade.
	IsBlocked bool `json:"isBlocked"`
}

// NodeUpgrade is what one node's upgrade did, as the upgrade write records it
// (C-88).
type NodeUpgrade struct {
	// Node is the node the result belongs to.
	Node string `json:"node"`
	// Version is the version the node reported when the result was written.
	Version string `json:"version,omitempty"`
	// Result states what happened to the node, for example "restored".
	Result string `json:"result"`
}

// UpgradeRun is the upgrade write that referenced one preflight record (C-88):
// every node's result lands on the same record, and a repeat write merges by
// node instead of creating a second record.
type UpgradeRun struct {
	// RecordID is the preflight record the upgrade was started from.
	RecordID string `json:"recordId"`
	// StartedAt is when the first upgrade write referenced the record.
	StartedAt time.Time `json:"startedAt"`
	// UpdatedAt is when the newest upgrade write touched the record.
	UpdatedAt time.Time `json:"updatedAt"`
	// Differences are the record's difference from the cluster as the upgrade
	// entry read it (C-88). They never block the write or the display.
	Differences []string `json:"differences,omitempty"`
	// Nodes carries one result per node, in node order.
	Nodes []NodeUpgrade `json:"nodes,omitempty"`
}

// Record is one stored upgrade preflight record as the API serves it (C-88).
type Record struct {
	// ID is the record identity.
	ID string `json:"id"`
	// Request is what the Owner submitted.
	Request PreflightRequest `json:"request"`
	// Result is what the preflight computed and stored with the request.
	Result PreflightResult `json:"result"`
	// ObservedAt is when the preflight observed the cluster (C-13).
	ObservedAt time.Time `json:"observedAt"`
	// Stale tells whether ObservedAt is older than C-13's freshness bound at
	// read time.
	Stale bool `json:"stale"`
	// IsSuperseded marks a record a later preflight replaced; it stays readable
	// and is never deleted (C-88).
	IsSuperseded bool `json:"isSuperseded"`
	// Upgrade is the upgrade write that referenced this record, when one did.
	Upgrade *UpgradeRun `json:"upgrade,omitempty"`
}

// Store is the control-plane database seam this domain keeps its records in
// (C-88). identity.Store implements it.
type Store interface {
	// AppendPreflight stores one record and reports whether it is new.
	AppendPreflight(ctx context.Context, candidate identity.PreflightCandidate) (identity.PreflightRow, bool, error)
	// Preflights lists every record, newest first.
	Preflights(ctx context.Context) ([]identity.PreflightRow, error)
	// AmendPreflight re-encodes one record's payload.
	AmendPreflight(ctx context.Context, id string, amend func(payload []byte) ([]byte, error)) (identity.PreflightRow, bool, error)
}

// PlatformPreflight evaluates a platform upgrade preflight (C-55). *Flow
// implements it.
type PlatformPreflight interface {
	Preflight(ctx context.Context, request Request) (Report, *controlplane.Error)
}

// ExtensionPreflight evaluates an extension upgrade preflight (C-55).
// *extension.Installer implements it.
type ExtensionPreflight interface {
	UpgradePreflight(ctx context.Context, targets []extension.UpgradeTarget) (extension.UpgradePreflight, error)
}

// Installed reports the window applications installed components declare
// (KOSD-C-73); the extension projection implements it.
type Installed interface {
	Apps() []extension.InstalledApp
}

// Config wires the upgrade record domain.
type Config struct {
	// Store keeps the records; required.
	Store Store
	// Platform evaluates platform upgrade preflights; required for a platform
	// request.
	Platform PlatformPreflight
	// Extensions evaluates extension upgrade preflights; required for an
	// extension request.
	Extensions ExtensionPreflight
	// Installed names the applications an extension upgrade affects; nil leaves
	// an extension record without affected applications.
	Installed Installed
	// Auditor records every write attempt (C-14); nil records nothing.
	Auditor controlplane.Auditor
	// Log receives the trail failures a caller must not learn about.
	Log *slog.Logger
	// Now supplies the clock the observation, staleness and audit times come
	// from; defaults to time.Now.
	Now func() time.Time
}

// Service is the upgrade preflight record domain over one store (C-88).
type Service struct {
	store      Store
	platform   PlatformPreflight
	extensions ExtensionPreflight
	installed  Installed
	auditor    controlplane.Auditor
	log        *slog.Logger
	now        func() time.Time
}

// New builds the record domain.
func New(config Config) *Service {
	service := &Service{
		store:      config.Store,
		platform:   config.Platform,
		extensions: config.Extensions,
		installed:  config.Installed,
		auditor:    config.Auditor,
		log:        config.Log,
		now:        config.Now,
	}
	if service.now == nil {
		service.now = time.Now
	}
	return service
}

// Preflight runs one upgrade preflight and stores the request with its result
// (C-88). Only the Owner may call it (C-59); a Member or Guest attempt leaves
// no record. The same request submitted again returns the stored record
// unchanged, so one request keeps one record (C-12, C-88).
func (s *Service) Preflight(ctx context.Context, session identity.Session, request PreflightRequest) (Record, *controlplane.Error) {
	if failure := s.ownerOnly(ctx, session, VerbPreflight, ""); failure != nil {
		return Record{}, failure
	}
	request, failure := normalizePreflight(request)
	if failure != nil {
		s.record(ctx, session, VerbPreflight, "", controlplane.ResultRejected)
		return Record{}, failure
	}
	result, failure := s.evaluate(ctx, request)
	if failure != nil {
		s.record(ctx, session, VerbPreflight, "", controlplane.ResultRejected)
		return Record{}, failure
	}
	signature, err := signatureOf(request)
	if err != nil {
		s.record(ctx, session, VerbPreflight, "", controlplane.ResultFailed)
		return Record{}, internal("预检请求无法编码")
	}
	payload, err := marshalPayload(recordPayload{Request: request, Result: result})
	if err != nil {
		s.record(ctx, session, VerbPreflight, "", controlplane.ResultFailed)
		return Record{}, internal("预检记录无法编码")
	}
	row, _, err := s.store.AppendPreflight(ctx, identity.PreflightCandidate{
		Signature:  signature,
		Payload:    payload,
		ObservedAt: s.now().UTC(),
	})
	if err != nil {
		s.record(ctx, session, VerbPreflight, "", controlplane.ResultFailed)
		return Record{}, databaseUnavailable()
	}
	s.record(ctx, session, VerbPreflight, row.ID, controlplane.ResultAccepted)
	record, err := s.view(row)
	if err != nil {
		return Record{}, internal("预检记录无法读取")
	}
	return record, nil
}

// Upgrade records one upgrade that references a stored preflight record
// (C-88). Only the Owner may call it (C-59). A record that does not exist is
// refused with the reason; every node's result is merged into the referenced
// record, so a multi-node upgrade never creates a second record. The record's
// difference from the cluster is compared here and recorded without blocking
// the write (C-88).
func (s *Service) Upgrade(ctx context.Context, session identity.Session, request UpgradeRequest) (Record, *controlplane.Error) {
	if failure := s.ownerOnly(ctx, session, VerbUpgrade, request.RecordID); failure != nil {
		return Record{}, failure
	}
	request, failure := normalizeUpgrade(request)
	if failure != nil {
		s.record(ctx, session, VerbUpgrade, request.RecordID, controlplane.ResultRejected)
		return Record{}, failure
	}
	records, err := s.store.Preflights(ctx)
	if err != nil {
		s.record(ctx, session, VerbUpgrade, request.RecordID, controlplane.ResultFailed)
		return Record{}, databaseUnavailable()
	}
	var stored *Record
	for index := range records {
		if records[index].ID != request.RecordID {
			continue
		}
		record, err := s.view(records[index])
		if err != nil {
			s.record(ctx, session, VerbUpgrade, request.RecordID, controlplane.ResultFailed)
			return Record{}, internal("预检记录无法读取")
		}
		stored = &record
		break
	}
	if stored == nil {
		s.record(ctx, session, VerbUpgrade, request.RecordID, controlplane.ResultRejected)
		return Record{}, recordNotFound(request.RecordID)
	}
	differences := s.differences(ctx, *stored)
	now := s.now().UTC()
	row, found, err := s.store.AmendPreflight(ctx, stored.ID, func(payload []byte) ([]byte, error) {
		storedPayload, err := unmarshalPayload(payload)
		if err != nil {
			return nil, err
		}
		storedPayload.Upgrade = mergeUpgrade(storedPayload.Upgrade, stored.ID, now, differences, request.Nodes)
		return marshalPayload(storedPayload)
	})
	if err != nil {
		s.record(ctx, session, VerbUpgrade, request.RecordID, controlplane.ResultFailed)
		return Record{}, databaseUnavailable()
	}
	if !found {
		s.record(ctx, session, VerbUpgrade, request.RecordID, controlplane.ResultRejected)
		return Record{}, recordNotFound(request.RecordID)
	}
	s.record(ctx, session, VerbUpgrade, stored.ID, controlplane.ResultAccepted)
	record, err := s.view(row)
	if err != nil {
		return Record{}, internal("预检记录无法读取")
	}
	return record, nil
}

// Records lists every stored record, newest first. Superseded records are
// included (C-88).
func (s *Service) Records(ctx context.Context) ([]Record, error) {
	rows, err := s.store.Preflights(ctx)
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(rows))
	for _, row := range rows {
		record, err := s.view(row)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

// Evaluate answers the preflight for one request without storing it, so the
// upgrade write can compare the record with the cluster as it stands now
// (C-88). It is the same evaluation the preflight write stores.
func (s *Service) Evaluate(ctx context.Context, request PreflightRequest) (PreflightResult, *controlplane.Error) {
	normalized, failure := normalizePreflight(request)
	if failure != nil {
		return PreflightResult{}, failure
	}
	return s.evaluate(ctx, normalized)
}

// evaluate runs the preflight the request's kind names.
func (s *Service) evaluate(ctx context.Context, request PreflightRequest) (PreflightResult, *controlplane.Error) {
	if request.Kind == KindExtension {
		return s.extensionPreflight(ctx, request)
	}
	return s.platformPreflight(ctx, request)
}

// platformPreflight runs C-55's preflight over the upgrade flow and flattens
// what it reported into the record's shape.
func (s *Service) platformPreflight(ctx context.Context, request PreflightRequest) (PreflightResult, *controlplane.Error) {
	if s.platform == nil {
		return PreflightResult{}, capabilityMissing("控制面没有接入平台升级预检")
	}
	report, failure := s.platform.Preflight(ctx, Request{
		Nodes:    request.Nodes,
		Version:  request.TargetVersion,
		Downtime: request.Downtime,
	})
	if failure != nil {
		return PreflightResult{}, failure
	}
	result := PreflightResult{
		TargetVersion:        request.TargetVersion,
		DeclaredDowntime:     request.Downtime,
		EstimatedDowntime:    request.Downtime,
		AffectedApplications: []string{},
		Nodes:                make([]NodeOutcome, 0, len(report.Nodes)),
		Undetermined:         report.Undetermined,
		IsBlocked:            report.Blocked,
	}
	affected := map[string]bool{}
	for _, plan := range report.Nodes {
		result.Nodes = append(result.Nodes, NodeOutcome{
			Node:         plan.Node,
			Applications: append([]string{}, plan.Applications...),
			Volumes:      append([]string{}, plan.Volumes...),
		})
		for _, name := range plan.Applications {
			affected[name] = true
		}
	}
	for name := range affected {
		result.AffectedApplications = append(result.AffectedApplications, name)
	}
	sort.Strings(result.AffectedApplications)
	return result, nil
}

// extensionPreflight runs C-55's extension preflight and keeps each target's
// contribution difference (C-88).
func (s *Service) extensionPreflight(ctx context.Context, request PreflightRequest) (PreflightResult, *controlplane.Error) {
	if s.extensions == nil {
		return PreflightResult{}, capabilityMissing("控制面没有接入扩展升级预检")
	}
	targets := make([]extension.UpgradeTarget, 0, len(request.Targets))
	for _, target := range request.Targets {
		targets = append(targets, extension.UpgradeTarget{
			Component: target.Component,
			Reference: target.Reference,
			Digest:    target.Digest,
		})
	}
	preflight, err := s.extensions.UpgradePreflight(ctx, targets)
	if err != nil {
		return PreflightResult{}, &controlplane.Error{
			Code:               controlplane.CodeInvalidField,
			Message:            "扩展升级预检被拒绝：" + err.Error(),
			Stage:              controlplane.StageValidate,
			RequiresUserAction: true,
			SideEffects:        controlplane.SideEffectsNone,
		}
	}
	result := PreflightResult{
		TargetVersion:        request.TargetVersion,
		DeclaredDowntime:     request.Downtime,
		EstimatedDowntime:    request.Downtime,
		AffectedApplications: s.extensionApplications(request.Targets),
		Components:           make([]ComponentOutcome, 0, len(preflight.Components)),
		IsBlocked:            preflight.IsBlocked,
	}
	for _, component := range preflight.Components {
		result.Components = append(result.Components, ComponentOutcome{
			Component: component.Component,
			From:      component.From,
			To:        component.To,
			Changes:   component.Changes,
			Problems:  component.Problems,
			IsBlocked: component.IsBlocked,
		})
	}
	return result, nil
}

// extensionApplications names the installed window applications the upgraded
// components provide, which is what an extension upgrade takes out of service.
func (s *Service) extensionApplications(targets []Target) []string {
	names := make([]string, 0)
	if s.installed == nil {
		return names
	}
	upgraded := make(map[string]bool, len(targets))
	for _, target := range targets {
		upgraded[target.Component] = true
	}
	seen := map[string]bool{}
	for _, application := range s.installed.Apps() {
		if !upgraded[application.Component] || application.Manifest.ID == "" {
			continue
		}
		if seen[application.Manifest.ID] {
			continue
		}
		seen[application.Manifest.ID] = true
		names = append(names, application.Manifest.ID)
	}
	sort.Strings(names)
	return names
}

// differences compares one stored record with the cluster as it stands now
// (C-88). A preflight that cannot be re-read is reported as uncompared rather
// than failing the upgrade: the comparison never blocks the entry.
func (s *Service) differences(ctx context.Context, record Record) []string {
	fresh, failure := s.evaluate(ctx, record.Request)
	if failure != nil {
		return []string{"未能读取集群当前状态，未比对预检记录与集群的差异：" + failure.Message}
	}
	return comparePreflight(record.Result, fresh)
}

// comparePreflight renders the differences between a stored result and a fresh
// one. An unchanged cluster yields nil.
func comparePreflight(stored, fresh PreflightResult) []string {
	differences := make([]string, 0)
	if !equalStrings(stored.AffectedApplications, fresh.AffectedApplications) {
		differences = append(differences, "受影响应用由 ["+strings.Join(stored.AffectedApplications, ", ")+
			"] 变为 ["+strings.Join(fresh.AffectedApplications, ", ")+"]")
	}
	if stored.IsBlocked != fresh.IsBlocked {
		differences = append(differences, "预检记录的阻塞结论与当前状态不同")
	}
	before := applicationsByNode(stored.Nodes)
	after := applicationsByNode(fresh.Nodes)
	nodes := make([]string, 0, len(before)+len(after))
	seen := map[string]bool{}
	for node := range before {
		seen[node] = true
		nodes = append(nodes, node)
	}
	for node := range after {
		if !seen[node] {
			nodes = append(nodes, node)
		}
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		if !equalStrings(before[node], after[node]) {
			differences = append(differences, "节点 "+node+" 的受影响应用由 ["+strings.Join(before[node], ", ")+
				"] 变为 ["+strings.Join(after[node], ", ")+"]")
		}
	}
	beforeTo := componentTargets(stored.Components)
	afterTo := componentTargets(fresh.Components)
	components := make([]string, 0, len(beforeTo)+len(afterTo))
	seen = map[string]bool{}
	for component := range beforeTo {
		seen[component] = true
		components = append(components, component)
	}
	for component := range afterTo {
		if !seen[component] {
			components = append(components, component)
		}
	}
	sort.Strings(components)
	for _, component := range components {
		if beforeTo[component] != afterTo[component] {
			differences = append(differences, "组件 "+component+" 的目标由 "+beforeTo[component]+" 变为 "+afterTo[component])
		}
	}
	if len(differences) == 0 {
		return nil
	}
	return differences
}

// applicationsByNode indexes each node's affected applications.
func applicationsByNode(nodes []NodeOutcome) map[string][]string {
	index := make(map[string][]string, len(nodes))
	for _, node := range nodes {
		index[node.Node] = append([]string{}, node.Applications...)
	}
	return index
}

// componentTargets indexes each component's target package.
func componentTargets(components []ComponentOutcome) map[string]string {
	index := make(map[string]string, len(components))
	for _, component := range components {
		index[component.Component] = component.To
	}
	return index
}

// equalStrings compares two sorted lists.
func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// recordPayload is what one row stores: the request, its result, and the
// upgrade write that referenced it.
type recordPayload struct {
	// Request is the submitted request.
	Request PreflightRequest `json:"request"`
	// Result is the stored preflight.
	Result PreflightResult `json:"result"`
	// Upgrade is the upgrade write, when one referenced the record.
	Upgrade *UpgradeRun `json:"upgrade,omitempty"`
}

// marshalPayload encodes one record body.
func marshalPayload(payload recordPayload) ([]byte, error) {
	return json.Marshal(payload)
}

// unmarshalPayload decodes one record body.
func unmarshalPayload(raw []byte) (recordPayload, error) {
	var payload recordPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return recordPayload{}, err
	}
	return payload, nil
}

// view renders one stored row as the API serves it, with the freshness marker
// C-13 fixes computed at read time.
func (s *Service) view(row identity.PreflightRow) (Record, error) {
	payload, err := unmarshalPayload(row.Payload)
	if err != nil {
		return Record{}, err
	}
	return Record{
		ID:           row.ID,
		Request:      payload.Request,
		Result:       payload.Result,
		ObservedAt:   row.ObservedAt,
		Stale:        s.now().Sub(row.ObservedAt) > controlplane.StaleThreshold,
		IsSuperseded: row.IsSuperseded,
		Upgrade:      payload.Upgrade,
	}, nil
}

// mergeUpgrade folds one upgrade write into the record's existing run. A
// repeated write for the same node replaces that node's result instead of
// appending a second entry, so the record keeps one result per node (C-88).
func mergeUpgrade(existing *UpgradeRun, recordID string, now time.Time, differences []string, results []NodeUpgrade) *UpgradeRun {
	run := &UpgradeRun{RecordID: recordID, StartedAt: now, Nodes: []NodeUpgrade{}}
	if existing != nil {
		run = &UpgradeRun{
			RecordID:  existing.RecordID,
			StartedAt: existing.StartedAt,
			Nodes:     append([]NodeUpgrade{}, existing.Nodes...),
		}
	}
	run.UpdatedAt = now
	run.Differences = differences
	for _, result := range results {
		replaced := false
		for index := range run.Nodes {
			if run.Nodes[index].Node == result.Node {
				run.Nodes[index] = result
				replaced = true
				break
			}
		}
		if !replaced {
			run.Nodes = append(run.Nodes, result)
		}
	}
	sort.Slice(run.Nodes, func(left, right int) bool { return run.Nodes[left].Node < run.Nodes[right].Node })
	return run
}

// signatureOf hashes the normalized request, which is what makes a repeated
// submission the same request (C-12, C-88).
func signatureOf(request PreflightRequest) (string, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// normalizePreflight validates one request and puts it in the canonical form
// the signature is computed over: sorted lists, no repeats, trimmed names.
func normalizePreflight(request PreflightRequest) (PreflightRequest, *controlplane.Error) {
	request.Kind = Kind(strings.TrimSpace(string(request.Kind)))
	request.TargetVersion = strings.TrimSpace(request.TargetVersion)
	request.Downtime = strings.TrimSpace(request.Downtime)
	switch request.Kind {
	case KindPlatform:
		if request.TargetVersion == "" {
			return PreflightRequest{}, invalidField("平台升级预检必须给出目标版本")
		}
		if len(request.Targets) > 0 {
			return PreflightRequest{}, invalidField("平台升级预检不接受扩展目标")
		}
		nodes, failure := normalizeNodes(request.Nodes)
		if failure != nil {
			return PreflightRequest{}, failure
		}
		request.Nodes = nodes
	case KindExtension:
		if len(request.Nodes) > 0 {
			return PreflightRequest{}, invalidField("扩展升级预检不接受节点")
		}
		targets, failure := normalizeTargets(request.Targets)
		if failure != nil {
			return PreflightRequest{}, failure
		}
		request.Targets = targets
	default:
		return PreflightRequest{}, invalidField("kind 只能是 platform 或 extension")
	}
	if request.Downtime == "" {
		return PreflightRequest{}, invalidField("downtime 必填：声明的停机时长，例如 15m")
	}
	downtime, err := time.ParseDuration(request.Downtime)
	if err != nil || downtime < 0 {
		return PreflightRequest{}, invalidField("downtime 必须是正时长，例如 15m")
	}
	return request, nil
}

// normalizeNodes validates the nodes a platform upgrade takes out.
func normalizeNodes(nodes []string) ([]string, *controlplane.Error) {
	if len(nodes) == 0 {
		return nil, invalidField("平台升级预检至少给一个节点")
	}
	seen := map[string]bool{}
	normalized := make([]string, 0, len(nodes))
	for _, node := range nodes {
		node = strings.TrimSpace(node)
		if !controlplane.RecordableName(node) {
			return nil, invalidField("节点名无效：" + node)
		}
		if seen[node] {
			return nil, invalidField("节点 " + node + " 重复列出")
		}
		seen[node] = true
		normalized = append(normalized, node)
	}
	sort.Strings(normalized)
	return normalized, nil
}

// normalizeTargets validates the component packages an extension upgrade
// replaces.
func normalizeTargets(targets []Target) ([]Target, *controlplane.Error) {
	if len(targets) == 0 {
		return nil, invalidField("扩展升级预检至少给一个组件")
	}
	seen := map[string]bool{}
	normalized := make([]Target, 0, len(targets))
	for _, target := range targets {
		target.Component = strings.TrimSpace(target.Component)
		target.Reference = strings.TrimSpace(target.Reference)
		target.Digest = strings.TrimSpace(target.Digest)
		if target.Component == "" {
			return nil, invalidField("扩展升级目标必须给出组件")
		}
		if target.Reference == "" {
			return nil, invalidField("组件 " + target.Component + " 必须给出目标包")
		}
		if len(target.Reference) > maxReferenceLength {
			return nil, invalidField("组件 " + target.Component + " 的目标包超过 " + strconv.Itoa(maxReferenceLength) + " 字节")
		}
		if seen[target.Component] {
			return nil, invalidField("组件 " + target.Component + " 重复列出")
		}
		seen[target.Component] = true
		normalized = append(normalized, target)
	}
	sort.Slice(normalized, func(left, right int) bool { return normalized[left].Component < normalized[right].Component })
	return normalized, nil
}

// UpgradeRequest is what the Owner submits to the upgrade write (C-88): the
// preflight record it references and each node's result.
type UpgradeRequest struct {
	// RecordID is the preflight record the upgrade references; required.
	RecordID string `json:"recordId"`
	// Nodes carries one result per node the upgrade moved.
	Nodes []NodeUpgrade `json:"nodes,omitempty"`
}

// normalizeUpgrade validates one upgrade write.
func normalizeUpgrade(request UpgradeRequest) (UpgradeRequest, *controlplane.Error) {
	request.RecordID = strings.TrimSpace(request.RecordID)
	if !controlplane.RecordableName(request.RecordID) {
		return UpgradeRequest{}, invalidField("recordId 必填且必须是预检记录标识")
	}
	seen := map[string]bool{}
	normalized := make([]NodeUpgrade, 0, len(request.Nodes))
	for _, result := range request.Nodes {
		result.Node = strings.TrimSpace(result.Node)
		result.Version = strings.TrimSpace(result.Version)
		result.Result = strings.TrimSpace(result.Result)
		if !controlplane.RecordableName(result.Node) {
			return UpgradeRequest{}, invalidField("节点结果必须给出节点名")
		}
		if result.Result == "" {
			return UpgradeRequest{}, invalidField("节点 " + result.Node + " 的结果不能为空")
		}
		if seen[result.Node] {
			return UpgradeRequest{}, invalidField("节点 " + result.Node + " 的重复结果")
		}
		seen[result.Node] = true
		normalized = append(normalized, result)
	}
	request.Nodes = normalized
	return request, nil
}

// ownerOnly refuses the write for every role but the global Owner (C-59).
func (s *Service) ownerOnly(ctx context.Context, session identity.Session, verb, recordID string) *controlplane.Error {
	if session.Username != "" && session.Role == write.RoleOwner {
		return nil
	}
	s.record(ctx, session, verb, recordID, controlplane.ResultRejected)
	return &controlplane.Error{
		Code:               controlplane.CodePermissionDenied,
		Message:            "平台与扩展升级的预检与升级写入只由 Owner 发起",
		Stage:              controlplane.StageAuthorize,
		RequiresUserAction: true,
		SideEffects:        controlplane.SideEffectsNone,
	}
}

// record appends one write attempt to the audit trail (C-14). The object is the
// record the attempt concerned; a refused preflight has none yet and names its
// subject instead.
func (s *Service) record(ctx context.Context, session identity.Session, verb, recordID, result string) {
	if s.auditor == nil {
		return
	}
	name := recordID
	if !controlplane.RecordableName(name) {
		name = auditRequestName
	}
	controlplane.RecordAudit(ctx, s.auditor, s.log, controlplane.AuditRecord{
		Actor:      session.Username,
		Object:     controlplane.ObjectRef{Group: api.Group, Kind: auditKind, Name: name},
		Verb:       verb,
		Result:     result,
		Source:     controlplane.SourceAPI,
		OccurredAt: s.now().UTC(),
	})
}

// recordNotFound refuses an upgrade write whose record does not exist (C-88).
func recordNotFound(id string) *controlplane.Error {
	return &controlplane.Error{
		Code:               controlplane.CodeNotFound,
		Message:            "预检记录 " + id + " 不存在：升级写入必须引用一条已存储的预检记录",
		Stage:              controlplane.StageValidate,
		RequiresUserAction: true,
		SideEffects:        controlplane.SideEffectsNone,
	}
}

// invalidField reports a field the model rejects.
func invalidField(message string) *controlplane.Error {
	return &controlplane.Error{
		Code:               controlplane.CodeInvalidField,
		Message:            message,
		Stage:              controlplane.StageValidate,
		RequiresUserAction: true,
		SideEffects:        controlplane.SideEffectsNone,
	}
}

// capabilityMissing reports a preflight this deployment did not wire.
func capabilityMissing(message string) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeCapabilityMissing,
		Message:     message,
		Stage:       controlplane.StageCapability,
		SideEffects: controlplane.SideEffectsNone,
	}
}

// internal reports a failure on this side of the API.
func internal(message string) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeInternal,
		Message:     message,
		Stage:       controlplane.StageApply,
		Retryable:   true,
		SideEffects: controlplane.SideEffectsNone,
	}
}

// databaseUnavailable reports the control-plane database the record needs.
func databaseUnavailable() *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeDatabaseUnavailable,
		Message:     "控制面数据库暂不可用，升级预检记录暂不可读写",
		Stage:       controlplane.StageApply,
		Retryable:   true,
		SideEffects: controlplane.SideEffectsNone,
	}
}
