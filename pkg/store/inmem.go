package store

import (
	"sync"

	"github.com/sumanth-kadarla/ml-cache/pkg/evict"
	"github.com/sumanth-kadarla/ml-cache/pkg/wal"
)

// InMemStore is a threadsafe in-memory key-value store with LRU eviction and WAL
type InMemStore struct {
	mu      sync.RWMutex
	cap     int
	items   map[string]string
	evictor evict.Evictor
	wal     *wal.WAL
}

// NewInMemStore creates a store with given capacity and WAL instance
func NewInMemStore(cap int, w *wal.WAL) *InMemStore {
	im := &InMemStore{
		cap:     cap,
		items:   make(map[string]string),
		evictor: evict.NewLRUEvictor(cap),
		wal:     w,
	}
	// try recovery from wal
	im.recoverFromWAL()
	return im
}

func (s *InMemStore) recoverFromWAL() {
	if s.wal == nil {
		return
	}
	entries, _ := s.wal.ReadAll()
	for _, e := range entries {
		s.items[e.Key] = e.Value
		s.evictor.OnInsert(e.Key)
	}
}

func (s *InMemStore) Get(key string) (string, bool) {
	s.mu.RLock()
	v, ok := s.items[key]
	s.mu.RUnlock()
	if ok {
		s.evictor.OnAccess(key)
	}
	return v, ok
}

func (s *InMemStore) Set(key string, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.items[key]; !exists {
		// insertion may trigger eviction
		s.evictor.OnInsert(key)
		if s.evictor.NeedsEviction() {
			old := s.evictor.Evict()
			if old != "" {
				delete(s.items, old)
			}
		}
	}
	s.items[key] = value
	if s.wal != nil {
		s.wal.Append(wal.Entry{Key: key, Value: value})
	}
}
