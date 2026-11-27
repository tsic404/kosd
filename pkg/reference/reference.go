// Package reference holds the exclusion shared by the two ends of C-19's rule:
// a write that would introduce a declarative reference to a kernel object, and
// the release of that object's uninstall gate. Both take the same short-lived
// cluster record, so a stored reference is always visible to the scan that
// decides the release, and a release always precedes -- and therefore refuses
// -- a write that starts afterwards.
package reference

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// BoundaryLabel marks the records that carry the exclusion. It is a plain
// label, not a C-02 marker: the record is coordination state, not a model
// object.
const BoundaryLabel = "kos.tsic.top/reference-boundary"

// RecordPrefix keeps every boundary record in one namespace.
const RecordPrefix = "kos-reference-"

// Defaults bound one acquisition. A holder that dies leaves an expired record
// that the next caller takes over, so a crash cannot wedge an uninstall.
const (
	DefaultHold = 30 * time.Second
	DefaultWait = 5 * time.Second
	retryDelay  = 20 * time.Millisecond
)

// ErrBusy reports a record another caller still holds. The caller retries; a
// refused write changes nothing.
var ErrBusy = errors.New("reference boundary is held by another write")

// Boundary is the cluster record both ends of C-19 take. A nil boundary runs
// the work without the exclusion.
type Boundary struct {
	// Dynamic writes and reads the records.
	Dynamic dynamic.Interface
	// Namespace holds the records; it must be the same for every caller of one
	// cluster.
	Namespace string
	// Clock supplies the acquisition time; defaults to time.Now.
	Clock func() time.Time
	// Hold is how long one acquisition stays valid; zero selects DefaultHold.
	Hold time.Duration
	// Wait bounds how long a caller waits for a busy record; zero selects
	// DefaultWait.
	Wait time.Duration
}

// New builds a boundary over one cluster.
func New(client dynamic.Interface, namespace string) *Boundary {
	return &Boundary{Dynamic: client, Namespace: namespace}
}

// Key names the kernel object two decisions must agree on.
func Key(group, kind, namespace, name string) string {
	return group + "/" + kind + "/" + namespace + "/" + name
}

// Do runs fn while holding the exclusion of key. A caller whose record is
// busy waits until the holder releases it or Wait expires.
func (b *Boundary) Do(ctx context.Context, key string, fn func(context.Context) error) error {
	if b == nil || b.Dynamic == nil {
		return fn(ctx)
	}
	release, err := b.acquire(ctx, key)
	if err != nil {
		return err
	}
	defer release()
	return fn(ctx)
}

// acquire creates the record, taking over one whose hold expired.
func (b *Boundary) acquire(ctx context.Context, key string) (func(), error) {
	now := b.now()
	deadline := now.Add(b.wait())
	for {
		created, failure := b.create(ctx, key, now)
		if failure == nil {
			uid, version := created.GetUID(), created.GetResourceVersion()
			return func() { b.drop(ctx, key, uid, version) }, nil
		}
		if !errors.Is(failure, errExists) {
			return nil, failure
		}
		current, err := b.record(ctx, key)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !b.expired(current, now) {
			if !b.now().Before(deadline) {
				return nil, ErrBusy
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryDelay):
				continue
			}
		}
		// The holder is gone; dropping its record lets the next create take the
		// exclusion. A concurrent caller that replaced it first wins.
		uid, version := current.GetUID(), current.GetResourceVersion()
		if err := b.drop(ctx, key, uid, version); err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
	}
}

// errExists reports a record another caller created first.
var errExists = errors.New("reference boundary record exists")

// create writes one record. A name taken by another caller answers errExists.
func (b *Boundary) create(ctx context.Context, key string, now time.Time) (*unstructured.Unstructured, error) {
	record := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      recordName(key),
			"namespace": b.Namespace,
			"labels":    map[string]any{BoundaryLabel: "true"},
		},
		"data": map[string]any{
			"target":    key,
			"holder":    holderToken(),
			"expiresAt": now.Add(b.hold()).UTC().Format(time.RFC3339Nano),
		},
	}}
	created, err := b.resource().Create(ctx, record, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil, errExists
	}
	if err != nil {
		return nil, err
	}
	return created, nil
}

// drop removes the record this caller holds. The preconditions keep a takeover
// that replaced the record from being deleted by its previous holder.
func (b *Boundary) drop(ctx context.Context, key string, uid types.UID, version string) error {
	options := metav1.DeleteOptions{}
	if uid != "" || version != "" {
		preconditions := &metav1.Preconditions{}
		if uid != "" {
			owner := uid
			preconditions.UID = &owner
		}
		if version != "" {
			preconditions.ResourceVersion = &version
		}
		options.Preconditions = preconditions
	}
	return b.resource().Delete(ctx, recordName(key), options)
}

// record reads the record of one key.
func (b *Boundary) record(ctx context.Context, key string) (*unstructured.Unstructured, error) {
	return b.resource().Get(ctx, recordName(key), metav1.GetOptions{})
}

// expired reports whether the record's hold has passed.
func (b *Boundary) expired(record *unstructured.Unstructured, now time.Time) bool {
	text, _, err := unstructured.NestedString(record.Object, "data", "expiresAt")
	if err != nil || text == "" {
		return true
	}
	expires, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return true
	}
	return !now.Before(expires)
}

// resource names the ConfigMap collection the records live in.
func (b *Boundary) resource() dynamic.ResourceInterface {
	return b.Dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}).Namespace(b.Namespace)
}

// now reads the boundary clock.
func (b *Boundary) now() time.Time {
	if b.Clock == nil {
		return time.Now()
	}
	return b.Clock()
}

// hold is how long one acquisition stays valid.
func (b *Boundary) hold() time.Duration {
	if b.Hold <= 0 {
		return DefaultHold
	}
	return b.Hold
}

// wait bounds how long one caller waits for a busy record.
func (b *Boundary) wait() time.Duration {
	if b.Wait <= 0 {
		return DefaultWait
	}
	return b.Wait
}

// recordName maps one object key to a stable record name.
func recordName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return RecordPrefix + hex.EncodeToString(sum[:8])
}

// holderToken names one acquisition so a record can be traced to its holder.
func holderToken() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}
