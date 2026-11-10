package logs

import (
	"strings"
	"sync"
)

// Ring is a bounded, concurrency-safe in-memory log buffer. Its query
// semantics match v1.6.5's LOG_BUFFER/get_logs: filters are applied first,
// total counts matches, and a positive limit keeps the newest rows while
// since_id is an ascending, unlimited incremental fetch.
type Ring struct {
	mu       sync.RWMutex
	capacity int
	entries  []Entry
	start    int
	size     int
	nextID   int64
}

// NewRing builds a ring. A non-positive capacity uses DefaultCapacity.
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	initial := capacity
	if initial > 4096 {
		initial = 4096
	}
	return &Ring{
		capacity: capacity,
		entries:  make([]Entry, initial),
	}
}

// Append stores one entry and assigns an ID when it has none. It returns the
// stored entry, which is what callers should also send to the durable store.
func (r *Ring) Append(entry Entry) Entry {
	if r == nil {
		return entry
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if entry.ID <= 0 {
		r.nextID++
		entry.ID = r.nextID
	} else if entry.ID > r.nextID {
		r.nextID = entry.ID
	}

	if len(r.entries) == 0 {
		return entry
	}
	if r.size == len(r.entries) {
		if len(r.entries) < r.capacity {
			r.grow()
		} else {
			// Full at capacity: overwrite the oldest slot.
			r.entries[r.start] = entry
			r.start = (r.start + 1) % len(r.entries)
			return entry
		}
	}
	index := (r.start + r.size) % len(r.entries)
	r.entries[index] = entry
	r.size++
	return entry
}

func (r *Ring) grow() {
	newSize := len(r.entries) * 2
	if newSize > r.capacity {
		newSize = r.capacity
	}
	if newSize <= len(r.entries) {
		return
	}
	next := make([]Entry, newSize)
	for index := 0; index < r.size; index++ {
		next[index] = r.entries[(r.start+index)%len(r.entries)]
	}
	r.entries = next
	r.start = 0
}

// Query returns a filtered copy of the ring in ascending ID order.
func (r *Ring) Query(query Query) Result {
	result := Result{Logs: make([]Entry, 0)}
	if r == nil {
		return result
	}
	r.mu.RLock()
	entries := make([]Entry, 0, r.size)
	for index := 0; index < r.size; index++ {
		entries = append(entries, r.entries[(r.start+index)%len(r.entries)])
	}
	r.mu.RUnlock()

	filtered := make([]Entry, 0, len(entries))
	needle := strings.ToLower(query.Search)
	for _, entry := range entries {
		if query.SinceID > 0 && entry.ID <= query.SinceID {
			continue
		}
		if query.Level != "" && !strings.EqualFold(entry.Level, query.Level) {
			continue
		}
		if query.Tag != "" && !strings.EqualFold(entry.Tag, query.Tag) {
			continue
		}
		if query.Search != "" &&
			!strings.Contains(strings.ToLower(entry.Msg), needle) &&
			!strings.Contains(strings.ToLower(entry.Tag), needle) {
			continue
		}
		filtered = append(filtered, entry)
	}

	result.Total = len(filtered)
	if query.SinceID <= 0 && query.Limit > 0 && len(filtered) > query.Limit {
		filtered = filtered[len(filtered)-query.Limit:]
	}
	result.Logs = filtered
	if len(filtered) > 0 {
		result.MaxID = filtered[len(filtered)-1].ID
	} else if query.SinceID > 0 {
		result.MaxID = query.SinceID
	}
	return result
}

// Clear drops every buffered row. The ID counter deliberately continues so
// panel since_id cursors do not see replayed IDs after a clear.
func (r *Ring) Clear() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.entries = make([]Entry, len(r.entries))
	r.start = 0
	r.size = 0
	r.mu.Unlock()
}

// Len reports the retained row count.
func (r *Ring) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.size
}

// MaxID reports the highest assigned ID.
func (r *Ring) MaxID() int64 {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.nextID
}

// EnsureNextID raises the ID counter without adding a row. It is used when a
// durable database is opened after a restart so new IDs continue after MAX(id).
func (r *Ring) EnsureNextID(id int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if id > r.nextID {
		r.nextID = id
	}
	r.mu.Unlock()
}

// Snapshot returns a copy of the retained rows in ascending ID order.
func (r *Ring) Snapshot() []Entry {
	result := r.Query(Query{})
	return result.Logs
}
