package eviction

import (
	"container/list"
	"sync"
)

// LFU node representing a cache entry in frequency lists
type lfuNode struct {
	key  string
	freq int
	elem *list.Element
}

// LFU is a thread-safe least-frequently-used eviction policy.
type LFU struct {
	mu       sync.Mutex
	items    map[string]*lfuNode // key -> node
	freqList map[int]*list.List  // freq -> list of nodes with that freq
	minFreq  int                 // current minimum frequency
}

func NewLFU() *LFU {
	return &LFU{
		items:    make(map[string]*lfuNode),
		freqList: make(map[int]*list.List),
		minFreq:  0,
	}
}

// OnGet increments frequency of accessed key
func (l *LFU) OnGet(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if node, ok := l.items[key]; ok {
		l.incrementFreq(node)
	}
}

// OnSet inserts a new key or updates existing frequency
func (l *LFU) OnSet(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if node, ok := l.items[key]; ok {
		l.incrementFreq(node)
		return
	}

	// New node
	node := &lfuNode{
		key:  key,
		freq: 1,
	}
	if l.freqList[1] == nil {
		l.freqList[1] = list.New()
	}
	node.elem = l.freqList[1].PushFront(node)
	l.items[key] = node
	l.minFreq = 1
}

// OnDelete removes a key from LFU structures
func (l *LFU) OnDelete(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	node, ok := l.items[key]
	if !ok {
		return
	}
	oldFreq := node.freq
	l.freqList[node.freq].Remove(node.elem)
	if l.freqList[oldFreq].Len() == 0 {
		delete(l.freqList, oldFreq)
	}
	if node.freq == l.minFreq && l.freqList[oldFreq] == nil {
		// recompute minFreq
		l.minFreq = 0
		for f := range l.freqList {
			if l.minFreq == 0 || f < l.minFreq {
				l.minFreq = f
			}
		}
	}
	delete(l.items, key)
}

// ChooseEviction returns the key with the lowest frequency
func (l *LFU) ChooseEviction() (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	listAtMin := l.freqList[l.minFreq]
	if listAtMin == nil || listAtMin.Len() == 0 {
		return "", false
	}
	node := listAtMin.Back().Value.(*lfuNode)
	return node.key, true
}

// incrementFreq moves node to higher frequency list
func (l *LFU) incrementFreq(node *lfuNode) {
	oldFreq := node.freq
	node.freq++

	// Remove from old list
	l.freqList[oldFreq].Remove(node.elem)
	if l.freqList[oldFreq].Len() == 0 {
		delete(l.freqList, oldFreq)
		if oldFreq == l.minFreq {
			l.minFreq++
		}
	}

	// Add to new freq list
	if l.freqList[node.freq] == nil {
		l.freqList[node.freq] = list.New()
	}
	node.elem = l.freqList[node.freq].PushFront(node)
}

// GetFrequency returns the frequency of a key
func (l *LFU) GetFrequency(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if node, ok := l.items[key]; ok {
		return node.freq
	}
	return 0
}

// Items returns current LFU items (for ML state)
func (l *LFU) Items() map[string]*lfuNode {
	l.mu.Lock()
	defer l.mu.Unlock()
	copy := make(map[string]*lfuNode, len(l.items))
	for k, v := range l.items {
		copy[k] = v
	}
	return copy
}
