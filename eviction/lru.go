package eviction

import (
	"container/list"
	"sync"
)

type lruEntry struct {
	key string
}
type lruNode struct {
	key  string
	freq int
	elem *list.Element
}

// LRU is a thread-safe least-recently-used eviction policy.
// It keeps a doubly-linked list (front = most recent).
type LRU struct {
	mu    sync.Mutex
	list  *list.List               // front = most recent
	items map[string]*list.Element // key -> *list.Element
}

func NewLRU() *LRU {
	return &LRU{
		list:  list.New(),
		items: make(map[string]*list.Element),
	}
}

func (l *LRU) OnGet(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.items[key]; ok {
		l.list.MoveToFront(e)
	}
}

func (l *LRU) OnSet(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.items[key]; ok {
		l.list.MoveToFront(e)
		return
	}
	el := l.list.PushFront(&lruEntry{key: key})
	l.items[key] = el
}

func (l *LRU) OnDelete(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.items[key]; ok {
		l.list.Remove(e)
		delete(l.items, key)
	}
}

func (l *LRU) ChooseEviction() (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	el := l.list.Back()
	if el == nil {
		return "", false
	}
	return el.Value.(*lruEntry).key, true
}

func (l *LRU) Items() map[string]*lruNode {
	l.mu.Lock()
	defer l.mu.Unlock()

	copyMap := make(map[string]*lruNode, len(l.items))
	for k, e := range l.items {
		entry, ok := e.Value.(*lruEntry)
		if !ok {
			continue // safety check
		}
		copyMap[k] = &lruNode{
			key:  entry.key,
			elem: e,
			freq: 0, // LRU doesn’t track frequency, set 0
		}
	}
	return copyMap
}
