package router

import (
	"encoding/json"
	"sync"
	"sync/atomic"
)

const (
	statusLive     int32 = 0
	statusDraining int32 = 1
)

// BackendEntry represents a registered inference backend.
type BackendEntry struct {
	ContainerID string
	BackendURL  string          // e.g. http://172.17.0.3:8000
	BasePath    string          // e.g. /v1
	ModelObject json.RawMessage // full model object cached from backend /v1/models

	inflight atomic.Int64
	status   atomic.Int32
}

func (e *BackendEntry) IncrInFlight() { e.inflight.Add(1) }
func (e *BackendEntry) DecrInFlight() { e.inflight.Add(-1) }
func (e *BackendEntry) InFlight() int64 { return e.inflight.Load() }

func (e *BackendEntry) IsDraining() bool        { return e.status.Load() == statusDraining }
func (e *BackendEntry) SetDraining()             { e.status.Store(statusDraining) }

// TryAcquire atomically checks draining status and increments the in-flight counter.
// Returns false if the backend is draining.
func (e *BackendEntry) TryAcquire() bool {
	if e.IsDraining() {
		return false
	}
	e.inflight.Add(1)
	// Re-check after increment to close the TOCTOU window.
	if e.IsDraining() {
		e.inflight.Add(-1)
		return false
	}
	return true
}

// Router is a thread-safe routing table mapping model IDs to backends.
type Router struct {
	mu      sync.RWMutex
	entries map[string]*BackendEntry
}

func New() *Router {
	return &Router{entries: make(map[string]*BackendEntry)}
}

// Add registers a backend for modelID. Returns the displaced entry if a collision occurred.
func (r *Router) Add(modelID string, entry *BackendEntry) *BackendEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.entries[modelID]
	r.entries[modelID] = entry
	return prev
}

// MarkDraining marks all entries for containerID as draining and returns them.
// Entries remain in the table until Remove is called.
func (r *Router) MarkDraining(containerID string) []*BackendEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var entries []*BackendEntry
	for _, e := range r.entries {
		if e.ContainerID == containerID {
			e.SetDraining()
			entries = append(entries, e)
		}
	}
	return entries
}

// Remove deletes all entries for containerID. Safe to call multiple times.
func (r *Router) Remove(containerID string) []*BackendEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	var removed []*BackendEntry
	for model, e := range r.entries {
		if e.ContainerID == containerID {
			removed = append(removed, e)
			delete(r.entries, model)
		}
	}
	return removed
}

// Get returns the BackendEntry for a model ID, or nil if not found.
func (r *Router) Get(modelID string) *BackendEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.entries[modelID]
}

// Models returns cached model objects for all live (non-draining) backends.
func (r *Router) Models() []json.RawMessage {
	r.mu.RLock()
	defer r.mu.RUnlock()
	models := make([]json.RawMessage, 0, len(r.entries))
	for _, e := range r.entries {
		if !e.IsDraining() {
			models = append(models, e.ModelObject)
		}
	}
	return models
}

// Snapshot returns a shallow copy of all entries, used for metrics callbacks.
func (r *Router) Snapshot() map[string]*BackendEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	snap := make(map[string]*BackendEntry, len(r.entries))
	for k, v := range r.entries {
		snap[k] = v
	}
	return snap
}

// Count returns the number of entries (including draining).
func (r *Router) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}
