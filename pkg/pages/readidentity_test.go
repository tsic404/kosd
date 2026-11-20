package pages

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/identity"
	"github.com/tsic404/kosd/pkg/write"
)

// serverItem is one server-source item the identity cases read.
func serverItem(id string) extension.PageItem {
	return extension.PageItem{ID: id, Label: id, Source: extension.PageSourceMetric, Reference: "cpu", Reason: "只读展示"}
}

// readObservation is one item as the reader saw its identity.
type readObservation struct {
	itemID      string
	readID      ReadID
	found       bool
	readContext context.Context
}

// observationLog collects reader observations across goroutines.
type observationLog struct {
	mutex    sync.Mutex
	observed []readObservation
}

// record appends the identity ctx carried for one item.
func (l *observationLog) record(ctx context.Context, itemID string) {
	readID, found := ReadIdentity(ctx)
	l.mutex.Lock()
	defer l.mutex.Unlock()
	l.observed = append(l.observed, readObservation{itemID: itemID, readID: readID, found: found, readContext: ctx})
}

// hasExpired reports whether the context one item was read under no longer
// carries an identity, which is what a finished read must leave behind.
func (o readObservation) hasExpired() bool {
	if o.readContext == nil {
		return true
	}
	_, found := ReadIdentity(o.readContext)
	return !found
}

// of returns the observations of the named items in read order.
func (l *observationLog) of(itemID string) []readObservation {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	matching := make([]readObservation, 0, len(l.observed))
	for _, observation := range l.observed {
		if observation.itemID == itemID {
			matching = append(matching, observation)
		}
	}
	return matching
}

// TestConcurrentReadsSharingAContextGetDifferentIdentities checks that the read
// boundary derives an identity per read: two reads handed the same context, both
// in flight at once, must not be groupable as one read.
func TestConcurrentReadsSharingAContextGetDifferentIdentities(t *testing.T) {
	shared := context.Background()
	observedAt := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	log := &observationLog{}
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	reader := readingsFunc(func(ctx context.Context, _ identity.Session, item extension.PageItem) (Reading, *controlplane.Error) {
		log.record(ctx, item.ID)
		if item.ID == "first" {
			arrived <- struct{}{}
			<-release
		}
		return Reading{Value: item.ID, ObservedAt: observedAt}, nil
	})
	service := New(declarations{{
		ID: "update", Title: "Update", Location: "panel",
		Items: []extension.PageItem{serverItem("first"), serverItem("second")},
	}}, Config{Readings: reader})

	failures := make(chan *controlplane.Error, 2)
	read := func() {
		_, failure := service.Read(shared, sessionWith(write.RoleOwner), "update")
		failures <- failure
	}
	go read()
	go read()
	for arrivedCount := 0; arrivedCount < 2; arrivedCount++ {
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			close(release)
			t.Fatalf("only %d of 2 reads reached their first item", arrivedCount)
		}
	}
	close(release)
	for readCount := 0; readCount < 2; readCount++ {
		if failure := <-failures; failure != nil {
			t.Fatalf("read page: %v", failure)
		}
	}

	firsts := log.of("first")
	if len(firsts) != 2 {
		t.Fatalf("first item resolved %d times, want 2", len(firsts))
	}
	for _, observation := range firsts {
		if !observation.found {
			t.Fatalf("the read boundary carried no identity into the reader: %+v", observation)
		}
	}
	if firsts[0].readID == firsts[1].readID {
		t.Fatalf("two concurrent reads sharing a context report identity %d; they must differ", firsts[0].readID)
	}
	for _, observation := range firsts {
		if !observation.hasExpired() {
			t.Fatalf("the context of a finished read still carries identity %d", observation.readID)
		}
	}
	if readID, found := ReadIdentity(shared); found {
		t.Fatalf("the caller's context carries identity %d after reading", readID)
	}
}

// TestOneReadGivesEveryItemTheSameIdentity checks that the items of one page
// read are groupable: the reader sees one identity for all of them.
func TestOneReadGivesEveryItemTheSameIdentity(t *testing.T) {
	shared := context.Background()
	observedAt := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	log := &observationLog{}
	service := New(declarations{{
		ID: "update", Title: "Update", Location: "panel",
		Items: []extension.PageItem{serverItem("first"), serverItem("second"), serverItem("third")},
	}}, Config{Readings: readingsFunc(func(ctx context.Context, _ identity.Session, item extension.PageItem) (Reading, *controlplane.Error) {
		log.record(ctx, item.ID)
		return Reading{Value: item.ID, ObservedAt: observedAt}, nil
	})})

	page, failure := service.Read(shared, sessionWith(write.RoleOwner), "update")
	if failure != nil {
		t.Fatalf("read page: %v", failure)
	}
	if len(page.Items) != 3 {
		t.Fatalf("page has %d items, want 3", len(page.Items))
	}
	if readID, found := ReadIdentity(nil); found {
		t.Fatalf("a nil context carries identity %d", readID)
	}
	if readID, found := ReadIdentity(shared); found {
		t.Fatalf("the caller's context carries identity %d outside any read", readID)
	}

	for _, itemID := range []string{"first", "second", "third"} {
		observations := log.of(itemID)
		if len(observations) != 1 {
			t.Fatalf("%s resolved %d times, want 1", itemID, len(observations))
		}
		if !observations[0].found {
			t.Fatalf("%s reached the reader without an identity", itemID)
		}
		if observations[0].readID != log.of("first")[0].readID {
			t.Fatalf("%s saw identity %d, first saw %d; one read must carry one identity",
				itemID, observations[0].readID, log.of("first")[0].readID)
		}
		if !observations[0].hasExpired() {
			t.Fatalf("the context of a finished read still carries identity %d", observations[0].readID)
		}
	}
}

// TestAnAbandonedReadLeavesNoIdentityForLaterReads checks that a read that stops
// before its last item does not hand its identity to a later read.
func TestAnAbandonedReadLeavesNoIdentityForLaterReads(t *testing.T) {
	shared := context.Background()
	observedAt := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	var isBroken atomic.Bool
	isBroken.Store(true)
	log := &observationLog{}
	service := New(declarations{{
		ID: "update", Title: "Update", Location: "panel",
		Items: []extension.PageItem{serverItem("first"), serverItem("boom"), serverItem("third")},
	}}, Config{Readings: readingsFunc(func(ctx context.Context, _ identity.Session, item extension.PageItem) (Reading, *controlplane.Error) {
		log.record(ctx, item.ID)
		if item.ID == "boom" && isBroken.Load() {
			return Reading{}, &controlplane.Error{
				Code: controlplane.CodeInternal, Message: "读取失败", Stage: controlplane.StageRead,
			}
		}
		return Reading{Value: item.ID, ObservedAt: observedAt}, nil
	})})

	if _, failure := service.Read(shared, sessionWith(write.RoleOwner), "update"); failure == nil {
		t.Fatalf("the broken read must fail")
	}
	if len(log.of("third")) != 0 {
		t.Fatalf("the broken read resolved its last item; it must stop early")
	}
	abandoned := log.of("first")
	if len(abandoned) != 1 || !abandoned[0].found {
		t.Fatalf("the abandoned read's identity = %+v", abandoned)
	}
	if !abandoned[0].hasExpired() {
		t.Fatalf("the context of an abandoned read still carries identity %d", abandoned[0].readID)
	}

	isBroken.Store(false)
	page, failure := service.Read(shared, sessionWith(write.RoleOwner), "update")
	if failure != nil {
		t.Fatalf("read page after the abandoned read: %v", failure)
	}
	if len(page.Items) != 3 {
		t.Fatalf("page has %d items, want 3", len(page.Items))
	}
	later := log.of("first")
	if len(later) != 2 {
		t.Fatalf("first item resolved %d times, want 2", len(later))
	}
	if !later[1].found {
		t.Fatalf("the later read reached the reader without an identity")
	}
	if later[1].readID == abandoned[0].readID {
		t.Fatalf("the later read reused the abandoned read's identity %d", later[1].readID)
	}
	if !later[1].hasExpired() {
		t.Fatalf("the context of a finished read still carries identity %d", later[1].readID)
	}
	if readID, found := ReadIdentity(shared); found {
		t.Fatalf("the caller's context carries identity %d after an abandoned read", readID)
	}
}
