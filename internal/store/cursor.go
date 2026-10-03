package store

import (
	"sync"
)

// CursorStore persists the last committed sync position so the engine can
// resume after an interruption.
type CursorStore struct {
	mu     sync.Mutex
	cursor int64
}

// NewCursorStore creates an empty cursor store (cursor 0 = start).
func NewCursorStore() *CursorStore {
	return &CursorStore{}
}

// Save persists the cursor.
func (s *CursorStore) Save(cursor int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cursor = cursor
}

// Load returns the last committed cursor.
func (s *CursorStore) Load() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor
}
