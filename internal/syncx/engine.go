// Package syncx implements the incremental sync engine. It consumes upstream
// change events, buffers out-of-order arrivals, applies them in strict
// sequence order, persists the cursor, and supports full rebuilds and
// bounded-backoff retries.
package syncx

import (
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/Diudiuseven/cms-content-sync-go/internal/config"
	"github.com/Diudiuseven/cms-content-sync-go/internal/model"
	"github.com/Diudiuseven/cms-content-sync-go/internal/publish"
	"github.com/Diudiuseven/cms-content-sync-go/internal/store"
)

// Upstream is the subset of the upstream source API used by the engine.
type Upstream interface {
	EventsAfter(after int64) ([]*model.Event, error)
	Snapshot() (*model.Snapshot, error)
}

// Engine coordinates incremental sync, rebuild and publishing.
type Engine struct {
	mu       sync.Mutex
	upstream Upstream
	drafts   *store.DraftStore
	cursors  *store.CursorStore
	publisher *publish.Publisher
	config   *config.Config

	cursor  int64
	buffer  map[int64]*model.Event
	running bool
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

// NewEngine creates a sync engine.
func NewEngine(
	upstream Upstream,
	drafts *store.DraftStore,
	cursors *store.CursorStore,
	publisher *publish.Publisher,
	cfg *config.Config,
) *Engine {
	if cfg == nil {
		cfg = config.Default()
	}
	return &Engine{
		upstream:  upstream,
		drafts:    drafts,
		cursors:   cursors,
		publisher: publisher,
		config:    cfg,
		buffer:    make(map[int64]*model.Event),
	}
}

// Cursor returns the last committed sequence number.
func (e *Engine) Cursor() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cursor
}

// Drafts exposes the draft store (for reads during rebuild).
func (e *Engine) Drafts() *store.DraftStore {
	return e.drafts
}

// Start loads the persisted cursor and begins background sync.
func (e *Engine) Start() {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return
	}
	e.running = true
	e.stopCh = make(chan struct{})
	e.cursor = e.cursors.Load()
	e.buffer = make(map[int64]*model.Event)
	e.mu.Unlock()

	e.wg.Add(1)
	go e.loop()
}

// Stop halts background sync.
func (e *Engine) Stop() {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return
	}
	e.running = false
	close(e.stopCh)
	e.mu.Unlock()
	e.wg.Wait()
}

func (e *Engine) loop() {
	defer e.wg.Done()
	interval := e.config.PublishInterval
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			if _, err := e.SyncOnce(); err != nil {
				continue
			}
			if e.config.AutoPublish {
				_, _ = e.Publish()
			}
		}
	}
}

// Deliver injects a single event (possibly out of order, duplicated or late)
// into the engine. It is safe for concurrent use.
func (e *Engine) Deliver(ev *model.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ingestLocked(ev)
}

// SyncOnce fetches all events after the current cursor and applies them.
func (e *Engine) SyncOnce() (int, error) {
	var events []*model.Event
	err := e.withRetry(func() error {
		var fetchErr error
		events, fetchErr = e.upstream.EventsAfter(e.cursor)
		return fetchErr
	})
	if err != nil {
		return 0, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range events {
		e.ingestLocked(ev)
	}
	return len(events), nil
}

// ingestLocked buffers an event and applies the contiguous prefix in strict
// sequence order. Duplicate or late events (seq <= cursor) are ignored, making
// repeated delivery side-effect free.
func (e *Engine) ingestLocked(ev *model.Event) {
	if ev.Seq <= e.cursor {
		// Already applied: duplicate or late arrival. No side effects.
		return
	}
	e.buffer[ev.Seq] = ev
	for {
		next := e.cursor + 1
		ev, ok := e.buffer[next]
		if !ok {
			break
		}
		delete(e.buffer, next)
		e.applyLocked(ev)
		e.cursor = next
		e.cursors.Save(e.cursor)
	}
}

// applyLocked applies a single event to the draft store. The write is atomic.
func (e *Engine) applyLocked(ev *model.Event) {
	key := ev.Key()
	switch ev.Type {
	case model.EventDelete:
		e.drafts.Delete(key, ev.Seq)
	case model.EventUpsert:
		existing, ok := e.drafts.Get(key)
		if !ok {
			existing = &model.Content{
				ID:     ev.ID,
				Locale: ev.Locale,
				Fields: make(map[string]model.FieldValue),
			}
		}
		if existing.Fields == nil {
			existing.Fields = make(map[string]model.FieldValue)
		}
		// Merge partial fields in sequence order.
		for k, v := range ev.Fields {
			existing.Fields[k] = v
		}
		existing.SchemaID = ev.SchemaID
		existing.SchemaVersion = ev.SchemaVersion
		existing.Deleted = false
		existing.LastSeq = ev.Seq
		e.drafts.Upsert(existing)
	}
}

// Publish validates and atomically publishes the current drafts.
func (e *Engine) Publish() (*store.Version, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	contents := e.drafts.Snapshot()
	return e.publisher.Publish(contents, e.cursor)
}

// RebuildReport explains the result of a full rebuild.
type RebuildReport struct {
	BeforeVersion int64
	AfterVersion  int64
	BeforeCursor  int64
	AfterCursor   int64
	ContentsTotal int
	Aligned       bool
	Added         []model.ContentKey
	Removed       []model.ContentKey
	Changed       []model.ContentKey
}

// Rebuild performs a full reconciliation against the upstream source. It
// fetches a complete snapshot, merges any local changes that are newer than
// the snapshot, replaces drafts atomically, and publishes a new version.
// Reads continue against the previously published version throughout.
func (e *Engine) Rebuild() (*RebuildReport, error) {
	var snap *model.Snapshot
	err := e.withRetry(func() error {
		var fetchErr error
		snap, fetchErr = e.upstream.Snapshot()
		return fetchErr
	})
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	var beforeVersion int64
	var beforeCursor int64
	if cur, err := e.publisher.Current(); err == nil {
		beforeVersion = cur.Number
		beforeCursor = cur.Cursor
	}
	beforeContents := e.drafts.Snapshot()

	// Merge: snapshot is authoritative up to snap.Seq; keep local drafts whose
	// LastSeq is greater (they represent changes that arrived after the
	// snapshot point and must not be lost).
	merged := make(map[model.ContentKey]*model.Content, len(snap.Contents))
	for k, c := range snap.Contents {
		merged[k] = c
	}
	local := e.drafts.Snapshot()
	for k, c := range local {
		if c.LastSeq > snap.Seq {
			merged[k] = c
		}
	}
	e.drafts.Replace(merged)

	if snap.Seq > e.cursor {
		e.cursor = snap.Seq
	}
	// Drop buffered events that are now covered by the snapshot.
	for seq := range e.buffer {
		if seq <= e.cursor {
			delete(e.buffer, seq)
		}
	}
	e.cursors.Save(e.cursor)

	afterContents := e.drafts.Snapshot()
	version, err := e.publisher.Publish(afterContents, e.cursor)
	if err != nil {
		return nil, err
	}

	report := &RebuildReport{
		BeforeVersion: beforeVersion,
		AfterVersion:  version.Number,
		BeforeCursor:  beforeCursor,
		AfterCursor:   version.Cursor,
		ContentsTotal: len(afterContents),
	}
	report.Aligned, report.Added, report.Removed, report.Changed = diffContents(beforeContents, afterContents)
	return report, nil
}

// diffContents compares two content sets and reports whether they are equal
// (aligned) plus the added/removed/changed keys.
func diffContents(before, after map[model.ContentKey]*model.Content) (bool, []model.ContentKey, []model.ContentKey, []model.ContentKey) {
	added := make([]model.ContentKey, 0)
	removed := make([]model.ContentKey, 0)
	changed := make([]model.ContentKey, 0)
	all := make(map[model.ContentKey]struct{})
	for k := range before {
		all[k] = struct{}{}
	}
	for k := range after {
		all[k] = struct{}{}
	}
	for k := range all {
		b, bok := before[k]
		a, aok := after[k]
		switch {
		case bok && !aok:
			removed = append(removed, k)
		case !bok && aok:
			added = append(added, k)
		case !contentsEqual(b, a):
			changed = append(changed, k)
		}
	}
	aligned := len(added) == 0 && len(removed) == 0 && len(changed) == 0
	return aligned, added, removed, changed
}

func contentsEqual(a, b *model.Content) bool {
	if a.Deleted != b.Deleted {
		return false
	}
	if len(a.Fields) != len(b.Fields) {
		return false
	}
	for k, v := range a.Fields {
		w, ok := b.Fields[k]
		if !ok || v.Set != w.Set || v.Value != w.Value {
			return false
		}
	}
	return true
}

// withRetry runs fn with bounded exponential backoff and jitter.
func (e *Engine) withRetry(fn func() error) error {
	delay := e.config.RetryBase
	var attempt int
	for {
		err := fn()
		if err == nil {
			return nil
		}
		attempt++
		if attempt >= e.config.MaxRetries {
			return fmt.Errorf("retry exhausted after %d attempts: %w", attempt, err)
		}
		jitter := 1 + (rand.Float64()*2-1)*e.config.RetryJitter
		sleep := time.Duration(float64(delay) * jitter)
		time.Sleep(sleep)
		delay = time.Duration(float64(delay) * 2)
		if delay > e.config.RetryMax {
			delay = e.config.RetryMax
		}
	}
}
