// Package store provides in-process draft storage and immutable version storage.
package store

import (
	"fmt"
	"sync"
	"time"

	"github.com/Diudiuseven/cms-content-sync-go/internal/model"
)

// DraftStore holds the mutable working copy of content. All access is guarded
// by a read/write mutex so incremental sync, rebuild and reads can proceed
// concurrently without observing partial writes.
type DraftStore struct {
	mu       sync.RWMutex
	contents map[model.ContentKey]*model.Content
}

// NewDraftStore creates an empty draft store.
func NewDraftStore() *DraftStore {
	return &DraftStore{contents: make(map[model.ContentKey]*model.Content)}
}

// Get returns a copy of the content for the given key.
func (d *DraftStore) Get(key model.ContentKey) (*model.Content, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	c, ok := d.contents[key]
	if !ok {
		return nil, false
	}
	return cloneContent(c), true
}

// Upsert stores content. The write is atomic: either the whole content is
// replaced or nothing is.
func (d *DraftStore) Upsert(c *model.Content) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.contents[c.Key()] = cloneContent(c)
}

// Delete marks content as deleted (tombstone) at the given sequence.
func (d *DraftStore) Delete(key model.ContentKey, seq int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.contents[key] = &model.Content{
		ID:      key.ID,
		Locale:  key.Locale,
		Deleted: true,
		LastSeq: seq,
		Fields:  make(map[string]model.FieldValue),
	}
}

// Snapshot returns a deep copy of all current drafts.
func (d *DraftStore) Snapshot() map[model.ContentKey]*model.Content {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make(map[model.ContentKey]*model.Content, len(d.contents))
	for k, v := range d.contents {
		out[k] = cloneContent(v)
	}
	return out
}

// Replace atomically replaces all drafts with the provided contents.
func (d *DraftStore) Replace(contents map[model.ContentKey]*model.Content) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.contents = make(map[model.ContentKey]*model.Content, len(contents))
	for k, v := range contents {
		d.contents[k] = cloneContent(v)
	}
}

// Len returns the number of draft entries (including tombstones).
func (d *DraftStore) Len() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.contents)
}

// cloneContent returns a deep copy of content.
func cloneContent(c *model.Content) *model.Content {
	if c == nil {
		return nil
	}
	out := *c
	out.Fields = make(map[string]model.FieldValue, len(c.Fields))
	for k, v := range c.Fields {
		out.Fields[k] = v
	}
	return &out
}

// Version is an immutable, atomically published snapshot of all content.
// Readers always observe a complete version: they never see a field-level mix.
type Version struct {
	Number    int64                              `json:"number"`
	CreatedAt time.Time                          `json:"created_at"`
	Cursor    int64                              `json:"cursor"`
	Contents  map[model.ContentKey]*model.Content `json:"contents"`
}

// Get returns content within this version.
func (v *Version) Get(key model.ContentKey) (*model.Content, bool) {
	c, ok := v.Contents[key]
	if !ok || c.Deleted {
		return nil, false
	}
	return c, true
}

// VersionStore keeps the history of published versions and the current
// version pointer. The current pointer is swapped atomically.
type VersionStore struct {
	mu       sync.RWMutex
	versions map[int64]*Version
	current  int64
}

// NewVersionStore creates an empty version store.
func NewVersionStore() *VersionStore {
	return &VersionStore{versions: make(map[int64]*Version)}
}

// Publish creates a new immutable version from the given contents and cursor,
// then atomically makes it the current version.
func (s *VersionStore) Publish(contents map[model.ContentKey]*model.Content, cursor int64) *Version {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current++
	v := &Version{
		Number:    s.current,
		CreatedAt: time.Now(),
		Cursor:    cursor,
		Contents:  make(map[model.ContentKey]*model.Content, len(contents)),
	}
	for k, c := range contents {
		v.Contents[k] = cloneContent(c)
	}
	s.versions[v.Number] = v
	return v
}

// Current returns the currently published version.
func (s *VersionStore) Current() (*Version, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.current == 0 {
		return nil, fmt.Errorf("%w: no published version", model.ErrNotFound)
	}
	return s.versions[s.current], nil
}

// Get returns a specific historical version.
func (s *VersionStore) Get(number int64) (*Version, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.versions[number]
	if !ok {
		return nil, fmt.Errorf("%w: version %d", model.ErrNotFound, number)
	}
	return v, nil
}

// LatestNumber returns the latest published version number.
func (s *VersionStore) LatestNumber() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// List returns all published version numbers in ascending order.
func (s *VersionStore) List() []int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]int64, 0, len(s.versions))
	for n := range s.versions {
		out = append(out, n)
	}
	// simple insertion sort for small lists
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
