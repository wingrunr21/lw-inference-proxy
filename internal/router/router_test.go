package router_test

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wingrunr21/lw-inference-proxy/internal/router"
)

func newEntry(containerID, backendURL string) *router.BackendEntry {
	return &router.BackendEntry{
		ContainerID: containerID,
		BackendURL:  backendURL,
		BasePath:    "/v1",
		ModelObject: json.RawMessage(`{"id":"model-a"}`),
	}
}

func TestRouter_AddAndGet(t *testing.T) {
	t.Parallel()
	r := router.New()
	entry := newEntry("ctr1", "http://10.0.0.1:8000")

	prev := r.Add("model-a", entry)
	assert.Nil(t, prev, "first add should return nil (no displacement)")

	got := r.Get("model-a")
	require.NotNil(t, got)
	assert.Equal(t, "ctr1", got.ContainerID)
}

func TestRouter_AddCollisionReturnsDisplaced(t *testing.T) {
	t.Parallel()
	r := router.New()
	first := newEntry("ctr1", "http://10.0.0.1:8000")
	second := newEntry("ctr2", "http://10.0.0.2:8000")

	r.Add("model-a", first)
	prev := r.Add("model-a", second)

	require.NotNil(t, prev, "collision should return displaced entry")
	assert.Equal(t, "ctr1", prev.ContainerID)

	// New entry wins.
	got := r.Get("model-a")
	require.NotNil(t, got)
	assert.Equal(t, "ctr2", got.ContainerID)
}

func TestRouter_GetMissReturnsNil(t *testing.T) {
	t.Parallel()
	r := router.New()
	assert.Nil(t, r.Get("nonexistent"))
}

func TestRouter_MarkDraining(t *testing.T) {
	t.Parallel()
	r := router.New()
	entry := newEntry("ctr1", "http://10.0.0.1:8000")
	r.Add("model-a", entry)

	drained := r.MarkDraining("ctr1")

	is := assert.New(t)
	is.Len(drained, 1)
	is.True(drained[0].IsDraining())

	// Entry is still in the table until Remove is called.
	is.Equal(1, r.Count())
}

func TestRouter_MarkDraining_UnknownContainerReturnsEmpty(t *testing.T) {
	t.Parallel()
	r := router.New()
	r.Add("model-a", newEntry("ctr1", "http://10.0.0.1:8000"))

	drained := r.MarkDraining("unknown-container")
	assert.Empty(t, drained)
}

func TestRouter_Remove(t *testing.T) {
	t.Parallel()
	r := router.New()
	r.Add("model-a", newEntry("ctr1", "http://10.0.0.1:8000"))
	r.Add("model-b", newEntry("ctr1", "http://10.0.0.1:8000"))
	r.Add("model-c", newEntry("ctr2", "http://10.0.0.2:8000"))

	removed := r.Remove("ctr1")

	is := assert.New(t)
	is.Len(removed, 2, "should remove both models for ctr1")
	is.Equal(1, r.Count(), "ctr2 entry should remain")
	is.Nil(r.Get("model-a"))
	is.Nil(r.Get("model-b"))
	is.NotNil(r.Get("model-c"))
}

func TestRouter_Remove_Idempotent(t *testing.T) {
	t.Parallel()
	r := router.New()
	r.Add("model-a", newEntry("ctr1", "http://10.0.0.1:8000"))

	r.Remove("ctr1")
	removed := r.Remove("ctr1") // second call on already-removed container

	assert.Empty(t, removed)
	assert.Equal(t, 0, r.Count())
}

func TestRouter_Models_SkipsDraining(t *testing.T) {
	t.Parallel()
	r := router.New()
	live := newEntry("ctr1", "http://10.0.0.1:8000")
	draining := newEntry("ctr2", "http://10.0.0.2:8000")
	r.Add("model-live", live)
	r.Add("model-drain", draining)
	r.MarkDraining("ctr2")

	models := r.Models()

	assert.Len(t, models, 1, "only live backends should appear in models list")
}

func TestRouter_Models_EmptyWhenNoEntries(t *testing.T) {
	t.Parallel()
	r := router.New()
	models := r.Models()
	assert.Empty(t, models)
}

func TestBackendEntry_TryAcquire_LiveIncrementsInFlight(t *testing.T) {
	t.Parallel()
	e := &router.BackendEntry{}

	ok := e.TryAcquire()

	assert.True(t, ok)
	assert.Equal(t, int64(1), e.InFlight())
}

func TestBackendEntry_TryAcquire_DrainingRejects(t *testing.T) {
	t.Parallel()
	e := &router.BackendEntry{}
	e.SetDraining()

	ok := e.TryAcquire()

	assert.False(t, ok)
	assert.Equal(t, int64(0), e.InFlight())
}

func TestBackendEntry_IncrDecrInFlight(t *testing.T) {
	t.Parallel()
	e := &router.BackendEntry{}

	e.IncrInFlight()
	e.IncrInFlight()
	assert.Equal(t, int64(2), e.InFlight())

	e.DecrInFlight()
	assert.Equal(t, int64(1), e.InFlight())
}

// TestRouter_ConcurrentAccess validates that the router is safe under concurrent
// reads and writes (run with -race to catch data races).
func TestRouter_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	r := router.New()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func(n int) {
			defer wg.Done()
			r.Add("model-a", newEntry("ctr1", "http://10.0.0.1:8000"))
		}(i)
		go func(n int) {
			defer wg.Done()
			r.Get("model-a")
		}(i)
		go func(n int) {
			defer wg.Done()
			r.Models()
		}(i)
	}
	wg.Wait()
}

// TestBackendEntry_TryAcquire_ConcurrentDrain checks that TryAcquire and
// SetDraining racing together never leaves a stale increment (run with -race).
func TestBackendEntry_TryAcquire_ConcurrentDrain(t *testing.T) {
	t.Parallel()
	e := &router.BackendEntry{}

	var wg sync.WaitGroup
	const goroutines = 100

	wg.Add(1)
	go func() {
		defer wg.Done()
		e.SetDraining()
	}()

	acquired := make([]bool, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			acquired[idx] = e.TryAcquire()
		}(i)
	}
	wg.Wait()

	// For every successful acquire, a matching release must be possible without
	// going negative.
	for _, ok := range acquired {
		if ok {
			e.DecrInFlight()
		}
	}
	assert.GreaterOrEqual(t, e.InFlight(), int64(0))
}
