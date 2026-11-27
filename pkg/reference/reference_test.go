package reference

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// boundaryWorld serves the ConfigMaps the records live in.
func boundaryWorld() *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(schema.GroupVersion{Version: "v1"}.WithKind("ConfigMap"), &unstructured.Unstructured{})
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		{Version: "v1", Resource: "configmaps"}: "ConfigMapList",
	})
}

// TestDoSerialisesOneKey checks that two callers of the same key never run
// together: that is the boundary a reference write and the gate release share.
func TestDoSerialisesOneKey(t *testing.T) {
	boundary := New(boundaryWorld(), "kos-system")
	boundary.Wait = 5 * time.Second
	var active, overlaps int32
	var wait sync.WaitGroup
	for index := 0; index < 4; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			err := boundary.Do(context.Background(), "shop", func(context.Context) error {
				if atomic.AddInt32(&active, 1) > 1 {
					atomic.AddInt32(&overlaps, 1)
				}
				time.Sleep(10 * time.Millisecond)
				atomic.AddInt32(&active, -1)
				return nil
			})
			if err != nil {
				t.Errorf("hold the boundary: %v", err)
			}
		}()
	}
	wait.Wait()
	if overlaps != 0 {
		t.Fatalf("the boundary let %d callers overlap", overlaps)
	}
}

// TestDoReportsBusy checks that a caller gives up instead of running beside a
// live holder.
func TestDoReportsBusy(t *testing.T) {
	boundary := New(boundaryWorld(), "kos-system")
	boundary.Wait = 50 * time.Millisecond
	held := make(chan struct{})
	release := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		_ = boundary.Do(context.Background(), "shop", func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	err := boundary.Do(context.Background(), "shop", func(context.Context) error {
		t.Error("the second caller ran beside the holder")
		return nil
	})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("second caller = %v, want %v", err, ErrBusy)
	}
	close(release)
	wait.Wait()
}

// TestDoRunsWithoutAClient checks the degraded path: a boundary without a
// cluster runs the work directly instead of failing it.
func TestDoRunsWithoutAClient(t *testing.T) {
	var boundary *Boundary
	ran := false
	if err := boundary.Do(context.Background(), "shop", func(context.Context) error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("run without a boundary: %v", err)
	}
	if !ran {
		t.Fatal("the work did not run")
	}
}
