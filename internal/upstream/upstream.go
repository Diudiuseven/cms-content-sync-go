// Package upstream simulates an external content source that emits change
// events and full snapshots. It supports deterministic reordering,
// duplication, delayed delivery and transient/partial failures so that the
// sync engine can be exercised locally without any external service.
package upstream

import (
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/Diudiuseven/cms-content-sync-go/internal/model"
)

// FailureKind enumerates injectable upstream failures.
type FailureKind int

const (
	FailNone FailureKind = iota
	FailTransient
	FailPartial
)

func (k FailureKind) String() string {
	switch k {
	case FailTransient:
		return "transient"
	case FailPartial:
		return "partial"
	default:
		return "none"
	}
}

// Source is the simulated upstream content source.
type Source struct {
	mu       sync.Mutex
	contents map[model.ContentKey]*model.Content
	eventLog []*model.Event
	nextSeq  int64
	now      func() time.Time

	// failure injection
	failKind  FailureKind
	failAfter int64 // fail requests with seq >= this
	failCount int   // how many times to fail before succeeding
	failures  int   // failures delivered so far

	// shuffle simulates network reordering on the pull path.
	shuffle bool
}

// NewSource creates an empty upstream source.
func NewSource() *Source {
	return &Source{
		contents: make(map[model.ContentKey]*model.Content),
		eventLog: make([]*model.Event, 0),
		nextSeq:  1,
		now:      time.Now,
	}
}

// SetFailure configures failure injection. Requests for events with seq >=
// failAfter will fail up to failCount times before succeeding.
func (s *Source) SetFailure(kind FailureKind, afterSeq int64, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failKind = kind
	s.failAfter = afterSeq
	s.failCount = count
	s.failures = 0
}

// ClearFailure removes failure injection.
func (s *Source) ClearFailure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failKind = FailNone
	s.failCount = 0
	s.failures = 0
}

// SetShuffle enables or disables network reordering on the pull path.
func (s *Source) SetShuffle(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shuffle = on
}

// Upsert applies a content change to the upstream source and returns the
// resulting event.
func (s *Source) Upsert(c *model.Content) *model.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	seq := s.nextSeq
	s.nextSeq++
	stored := cloneContent(c)
	stored.LastSeq = seq
	s.contents[c.Key()] = stored
	ev := &model.Event{
		Seq:           seq,
		Type:          model.EventUpsert,
		ID:            c.ID,
		Locale:        c.Locale,
		SchemaID:      c.SchemaID,
		SchemaVersion: c.SchemaVersion,
		Fields:        cloneFields(c.Fields),
		Timestamp:     s.now(),
	}
	s.appendEvent(ev)
	return ev
}

// Delete removes content from the upstream source and returns the event.
func (s *Source) Delete(id, locale string) *model.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	seq := s.nextSeq
	s.nextSeq++
	key := model.ContentKey{ID: id, Locale: locale}
	delete(s.contents, key)
	ev := &model.Event{
		Seq:       seq,
		Type:      model.EventDelete,
		ID:        id,
		Locale:    locale,
		Timestamp: s.now(),
	}
	s.appendEvent(ev)
	return ev
}

// EventsAfter returns all events with seq > after, in ascending order. If
// failure injection is active and the request would return an event at or
// beyond the failure threshold, it returns an error instead (simulating a
// transient/partial failure).
func (s *Source) EventsAfter(after int64) ([]*model.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	events := s.eventsAfterLocked(after)
	if s.failKind != FailNone && s.failures < s.failCount {
		for _, e := range events {
			if e.Seq >= s.failAfter {
				s.failures++
				return nil, fmt.Errorf("upstream %s failure at cursor %d", s.failKind, after)
			}
		}
	}
	if s.shuffle {
		rand.Shuffle(len(events), func(i, j int) { events[i], events[j] = events[j], events[i] })
	}
	return events, nil
}

// Snapshot returns a full point-in-time dump of upstream content.
func (s *Source) Snapshot() (*model.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failKind != FailNone && s.failures < s.failCount {
		s.failures++
		return nil, fmt.Errorf("upstream %s failure during snapshot", s.failKind)
	}

	out := &model.Snapshot{
		Seq:      s.nextSeq - 1,
		TakenAt:  s.now(),
		Contents: make(map[model.ContentKey]*model.Content, len(s.contents)),
	}
	for k, v := range s.contents {
		out.Contents[k] = cloneContent(v)
	}
	return out, nil
}

// MaxSeq returns the current maximum sequence number.
func (s *Source) MaxSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextSeq - 1
}

// Content returns a copy of the current upstream content for a key.
func (s *Source) Content(key model.ContentKey) (*model.Content, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.contents[key]
	if !ok {
		return nil, false
	}
	return cloneContent(c), true
}

// AllContents returns a deep copy of all current upstream contents.
func (s *Source) AllContents() map[model.ContentKey]*model.Content {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[model.ContentKey]*model.Content, len(s.contents))
	for k, v := range s.contents {
		out[k] = cloneContent(v)
	}
	return out
}

// eventsAfterLocked is a placeholder replaced by the event-log implementation
// below. It is kept to satisfy the compiler until the log is wired.
func (s *Source) eventsAfterLocked(after int64) []*model.Event {
	// The real source keeps an append-only event log; see eventlog.go.
	return s.logEventsAfter(after)
}

// sortedKeys is a small helper for deterministic ordering.
func sortedKeys(m map[model.ContentKey]*model.Content) []model.ContentKey {
	keys := make([]model.ContentKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ID != keys[j].ID {
			return keys[i].ID < keys[j].ID
		}
		return keys[i].Locale < keys[j].Locale
	})
	return keys
}

func cloneFields(fields map[string]model.FieldValue) map[string]model.FieldValue {
	out := make(map[string]model.FieldValue, len(fields))
	for k, v := range fields {
		out[k] = v
	}
	return out
}

func cloneContent(c *model.Content) *model.Content {
	if c == nil {
		return nil
	}
	out := *c
	out.Fields = cloneFields(c.Fields)
	return &out
}
