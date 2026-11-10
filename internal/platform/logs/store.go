package logs

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

const (
	appendBatchSize = 256
	trimInterval    = 5 * time.Second
	bytesInterval   = 30 * time.Second
	jobTimeout      = 10 * time.Second
)

type jobKind uint8

const (
	jobAppend jobKind = iota
	jobQuery
	jobClear
	jobStats
	jobConfigure
)

type job struct {
	kind    jobKind
	entry   Entry
	query   Query
	caps    Caps
	enabled bool
	done    chan jobResult
}

type jobResult struct {
	result Result
	stats  Stats
	err    error
}

type jobQueue struct {
	mu   sync.Mutex
	jobs []job
	wake chan struct{}
}

func newJobQueue() *jobQueue {
	return &jobQueue{wake: make(chan struct{}, 1)}
}

func (q *jobQueue) push(item job) {
	q.mu.Lock()
	q.jobs = append(q.jobs, item)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *jobQueue) takeAll() []job {
	q.mu.Lock()
	jobs := q.jobs
	q.jobs = nil
	q.mu.Unlock()
	return jobs
}

// LogStore owns the in-memory ring and an optional durable SQLite copy. Appends
// only touch the ring and a queue; all database work happens on one worker, so
// a slow database can never stall a request.
type LogStore struct {
	ring *Ring
	path string
	jobs *jobQueue
	stop chan struct{}
	done chan struct{}

	lifecycle sync.RWMutex
	closed    atomic.Bool
	enabled   atomic.Bool

	// Now is injectable for timestamping and retention tests.
	Now func() time.Time

	logger *log.Logger

	// Worker-owned state.
	db            *SQLiteStore
	caps          Caps
	lastTrim      time.Time
	lastBytesTrim time.Time
}

var _ Store = (*LogStore)(nil)

// Open builds a store. Durable persistence is optional; when enabled is false
// (or opening the database fails) the store stays a working memory ring and no
// logs.sqlite3 is created.
func Open(path string, enabled bool, caps Caps, logger *log.Logger) *LogStore {
	store := &LogStore{
		ring:   NewRing(DefaultCapacity),
		path:   path,
		jobs:   newJobQueue(),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		caps:   caps.normalized(),
		logger: logger,
	}
	go store.run()
	if err := store.Configure(enabled, caps.MaxRows, caps.MaxDays, caps.MaxBytes); err != nil {
		store.logf("logs: database disabled after configure error: %v", err)
	}
	return store
}

// Append normalizes, redacts, and stores one entry. Level and tag inference run
// on the original message before redaction, matching v1.6.5.
func (s *LogStore) Append(entry Entry) error {
	if s == nil {
		return fmt.Errorf("logs: nil store")
	}
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	if s.closed.Load() {
		return ErrClosed
	}
	entry = normalizeEntry(entry, s.now())
	stored := s.ring.Append(entry)
	if s.enabled.Load() {
		s.jobs.push(job{kind: jobAppend, entry: stored})
	}
	return nil
}

// Query reads the durable store when active, falling back to the memory ring
// if the database errors or does not answer before the v1 timeout.
func (s *LogStore) Query(query Query) (Result, error) {
	if s == nil {
		return Result{Logs: make([]Entry, 0)}, fmt.Errorf("logs: nil store")
	}
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	if s.closed.Load() {
		return Result{Logs: make([]Entry, 0)}, ErrClosed
	}
	if !s.enabled.Load() {
		return s.ring.Query(query), nil
	}
	result, err := s.submitQuery(query)
	if err == nil {
		return result, nil
	}
	s.logf("logs: query failed, falling back to memory: %v", err)
	return s.ring.Query(query), nil
}

func (s *LogStore) submitQuery(query Query) (Result, error) {
	done := make(chan jobResult, 1)
	s.jobs.push(job{kind: jobQuery, query: query, done: done})
	timer := time.NewTimer(jobTimeout)
	defer timer.Stop()
	select {
	case outcome := <-done:
		return outcome.result, outcome.err
	case <-timer.C:
		return Result{Logs: make([]Entry, 0)}, fmt.Errorf("logs: query timed out")
	}
}

// Clear empties both the ring and the durable store.
func (s *LogStore) Clear() error {
	if s == nil {
		return fmt.Errorf("logs: nil store")
	}
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	if s.closed.Load() {
		return ErrClosed
	}
	s.ring.Clear()
	if !s.enabled.Load() {
		return nil
	}
	done := make(chan jobResult, 1)
	s.jobs.push(job{kind: jobClear, done: done})
	timer := time.NewTimer(jobTimeout)
	defer timer.Stop()
	select {
	case outcome := <-done:
		return outcome.err
	case <-timer.C:
		return fmt.Errorf("logs: clear timed out")
	}
}

// Stats reports durable statistics when active and memory statistics otherwise.
func (s *LogStore) Stats() (Stats, error) {
	if s == nil {
		return Stats{}, fmt.Errorf("logs: nil store")
	}
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	if s.closed.Load() {
		return Stats{}, ErrClosed
	}
	if !s.enabled.Load() {
		return s.memoryStats(), nil
	}
	done := make(chan jobResult, 1)
	s.jobs.push(job{kind: jobStats, done: done})
	timer := time.NewTimer(jobTimeout)
	defer timer.Stop()
	select {
	case outcome := <-done:
		if outcome.err == nil {
			return outcome.stats, nil
		}
		s.logf("logs: stats failed, falling back to memory: %v", outcome.err)
		return s.memoryStats(), nil
	case <-timer.C:
		s.logf("logs: stats timed out, falling back to memory")
		return s.memoryStats(), nil
	}
}

func (s *LogStore) memoryStats() Stats {
	snapshot := s.ring.Snapshot()
	stats := Stats{Rows: int64(len(snapshot))}
	if len(snapshot) > 0 {
		stats.Oldest = snapshot[0].TS
		stats.Newest = snapshot[len(snapshot)-1].TS
	}
	return stats
}

// Configure applies panel settings, starting or stopping the durable worker
// part as the switch flips. Disabling deliberately leaves the file and rows in
// place so enabling again resumes the existing history.
func (s *LogStore) Configure(enabled bool, maxRows, maxDays, maxBytes int64) error {
	if s == nil {
		return fmt.Errorf("logs: nil store")
	}
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	if s.closed.Load() {
		return ErrClosed
	}
	done := make(chan jobResult, 1)
	s.jobs.push(job{
		kind:    jobConfigure,
		enabled: enabled,
		caps:    Caps{MaxRows: maxRows, MaxDays: maxDays, MaxBytes: maxBytes}.normalized(),
		done:    done,
	})
	timer := time.NewTimer(jobTimeout)
	defer timer.Stop()
	select {
	case outcome := <-done:
		return outcome.err
	case <-timer.C:
		return fmt.Errorf("logs: configure timed out")
	}
}

// Enabled reports whether the durable store is active.
func (s *LogStore) Enabled() bool {
	return s != nil && s.enabled.Load()
}

// Mode is "db" when the durable store is active and "memory" otherwise.
func (s *LogStore) Mode() string {
	if s.Enabled() {
		return "db"
	}
	return "memory"
}

// MaxID reports the highest assigned ID.
func (s *LogStore) MaxID() int64 {
	if s == nil {
		return 0
	}
	return s.ring.MaxID()
}

// Path reports the configured database path.
func (s *LogStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Close flushes queued appends, closes the database, and stops the worker.
func (s *LogStore) Close() error {
	if s == nil {
		return nil
	}
	s.lifecycle.Lock()
	if s.closed.Swap(true) {
		s.lifecycle.Unlock()
		<-s.done
		return nil
	}
	close(s.stop)
	s.lifecycle.Unlock()
	<-s.done
	return nil
}

func (s *LogStore) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *LogStore) logf(format string, args ...any) {
	if s != nil && s.logger != nil {
		s.logger.Printf(format, args...)
	}
}

func (s *LogStore) run() {
	ticker := time.NewTicker(trimInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			s.process(s.jobs.takeAll())
			s.closeDatabase()
			close(s.done)
			return
		case <-s.jobs.wake:
			s.process(s.jobs.takeAll())
		case now := <-ticker.C:
			s.maybeTrim(now)
		}
	}
}

func (s *LogStore) process(jobs []job) {
	batch := make([]Entry, 0, appendBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if s.db == nil {
			batch = batch[:0]
			return
		}
		if err := s.db.AppendBatch(batch); err != nil {
			// A failed batch is logged and dropped; it must not stop later
			// appends or the panel's read path.
			s.logf("logs: append batch failed: %v", err)
		}
		batch = batch[:0]
		s.maybeTrim(s.now())
	}

	for _, item := range jobs {
		if item.kind == jobAppend {
			if s.db == nil || !s.enabled.Load() {
				continue
			}
			batch = append(batch, item.entry)
			if len(batch) >= appendBatchSize {
				flush()
			}
			continue
		}
		flush()
		switch item.kind {
		case jobQuery:
			if s.db == nil {
				s.reply(item, jobResult{result: Result{Logs: make([]Entry, 0)}, err: fmt.Errorf("logs: database unavailable")})
				continue
			}
			result, err := s.db.Query(item.query)
			s.reply(item, jobResult{result: result, err: err})
		case jobStats:
			if s.db == nil {
				s.reply(item, jobResult{err: fmt.Errorf("logs: database unavailable")})
				continue
			}
			stats, err := s.db.Stats()
			s.reply(item, jobResult{stats: stats, err: err})
		case jobClear:
			if s.db == nil {
				s.reply(item, jobResult{})
				continue
			}
			s.reply(item, jobResult{err: s.db.Clear()})
		case jobConfigure:
			s.reply(item, jobResult{err: s.applyConfigure(item.enabled, item.caps)})
		}
	}
	flush()
}

func (s *LogStore) reply(item job, outcome jobResult) {
	if item.done == nil {
		return
	}
	item.done <- outcome
}

func (s *LogStore) applyConfigure(enabled bool, caps Caps) error {
	s.caps = caps.normalized()
	if !enabled {
		if s.db != nil {
			if err := s.db.Close(); err != nil {
				s.logf("logs: close database while disabling: %v", err)
			}
			s.db = nil
		}
		s.enabled.Store(false)
		return nil
	}
	if s.db == nil {
		store, err := NewSQLite(s.path)
		if err != nil {
			s.enabled.Store(false)
			return err
		}
		s.db = store
		maxID, err := store.MaxID()
		if err != nil {
			s.logf("logs: read max id: %v", err)
		} else {
			s.ring.EnsureNextID(maxID)
		}
	}
	s.enabled.Store(true)
	now := s.now()
	if err := s.db.Trim(s.caps); err != nil {
		s.logf("logs: trim after configure: %v", err)
	}
	s.lastTrim = now
	if s.caps.MaxBytes > 0 {
		if err := s.db.TrimBytes(s.caps.MaxBytes); err != nil {
			s.logf("logs: byte trim after configure: %v", err)
		}
		s.lastBytesTrim = now
	}
	return nil
}

func (s *LogStore) maybeTrim(now time.Time) {
	if s.db == nil || !s.enabled.Load() {
		return
	}
	if s.lastTrim.IsZero() || now.Sub(s.lastTrim) >= trimInterval {
		if err := s.db.Trim(s.caps); err != nil {
			s.logf("logs: trim failed: %v", err)
		}
		s.lastTrim = now
	}
	if s.caps.MaxBytes > 0 && (s.lastBytesTrim.IsZero() || now.Sub(s.lastBytesTrim) >= bytesInterval) {
		if err := s.db.TrimBytes(s.caps.MaxBytes); err != nil {
			s.logf("logs: byte trim failed: %v", err)
		}
		s.lastBytesTrim = now
	}
}

func (s *LogStore) closeDatabase() {
	if s.db != nil {
		if err := s.db.Close(); err != nil {
			s.logf("logs: close database: %v", err)
		}
		s.db = nil
	}
	s.enabled.Store(false)
}
