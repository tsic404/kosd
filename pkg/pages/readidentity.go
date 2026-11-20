package pages

import (
	"context"
	"sync/atomic"
)

// readIdentityKey marks the context value carrying one page read's identity.
type readIdentityKey struct{}

// readIdentityNumbers numbers page reads. Counting starts at one, so the zero
// ReadID always means "no read".
var readIdentityNumbers atomic.Uint64

// ReadID identifies one page read. Every item one Read call resolves carries
// the same value, and two reads never share one, even when their callers share
// a context.
type ReadID uint64

// readIdentity is one page read's identity. live is cleared when the read
// returns, so a context a reader kept cannot group later work under a read that
// already ended.
type readIdentity struct {
	id   ReadID
	live atomic.Bool
}

// ReadIdentity returns the identity of the page read ctx belongs to, and
// whether that read is still running. The identity is visible only while its
// read runs; a reader that gets false must not group the item with anything.
func ReadIdentity(ctx context.Context) (ReadID, bool) {
	if ctx == nil {
		return 0, false
	}
	identity, found := ctx.Value(readIdentityKey{}).(*readIdentity)
	if !found || !identity.live.Load() {
		return 0, false
	}
	return identity.id, true
}

// startReadIdentity derives the context one page read hands to its readers. The
// derived context stays local to Read, and the identity ends with the read, so
// a finished or abandoned read leaves nothing a later read could pick up.
func startReadIdentity(ctx context.Context) (context.Context, *readIdentity) {
	identity := &readIdentity{id: ReadID(readIdentityNumbers.Add(1))}
	identity.live.Store(true)
	return context.WithValue(ctx, readIdentityKey{}, identity), identity
}

// end retires the identity; readers receive false from ReadIdentity afterwards.
func (i *readIdentity) end() {
	i.live.Store(false)
}
