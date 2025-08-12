package evict

import "container/list"

// Evictor interface minimal
type Evictor interface {
	OnAccess(key string)
	OnInsert(key string)
	NeedsEviction() bool
	Evict() string
}

// LRUEvictor simple LRU with capacity
type LRUEvictor struct {
	cap   int
	ll    *list.List
	items map[string]*list.Element
}

type pair struct {
	k string
}

func NewLRUEvictor(cap int) *LRUEvictor {
	return &LRUEvictor{
		cap:   cap,
		ll:    list.New(),
		items: make(map[string]*list.Element),
	}
}

func (l *LRUEvictor) OnAccess(key string) {
	if el, ok := l.items[key]; ok {
		l.ll.MoveToFront(el)
	}
}

func (l *LRUEvictor) OnInsert(key string) {
	if el, ok := l.items[key]; ok {
		l.ll.MoveToFront(el)
		return
	}
	el := l.ll.PushFront(&pair{k: key})
	l.items[key] = el
}

func (l *LRUEvictor) NeedsEviction() bool {
	return l.ll.Len() > l.cap
}

func (l *LRUEvictor) Evict() string {
	el := l.ll.Back()
	if el == nil {
		return ""
	}
	p := el.Value.(*pair)
	l.ll.Remove(el)
	delKey := p.k
	delete(l.items, delKey)
	return delKey
}
