package cache

import (
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v3"
	"github.com/sumanth-github/ml-cache/eviction"
	"github.com/sumanth-github/ml-cache/pkg/wal"
)

var ErrItemTooLarge = errors.New("item size exceeds cache capacity")

// --------------------
// Logging Setup
// --------------------
var logFile *os.File
var csvWriter *csv.Writer
var logMu sync.Mutex
var evictKey string
var ok bool

func init() {
	var err error
	logPath := os.Getenv("CACHE_ACCESS_LOG_PATH")
	if logPath == "" {
		logPath = "access_log.csv"
	}
	logFile, err = os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		panic(err)
	}
	csvWriter = csv.NewWriter(logFile)
	if fi, _ := logFile.Stat(); fi.Size() == 0 {
		csvWriter.Write([]string{"timestamp", "operation", "key", "hit", "size_bytes", "ttl_remaining", "cache_used"})
		csvWriter.Flush()
	}
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			logMu.Lock()
			csvWriter.Flush()
			logMu.Unlock()
		}
	}()
}

func logAccess(op, key string, hit int, size int64, ttlRemaining float64, cacheUsed int64) {
	logMu.Lock()
	defer logMu.Unlock()
	record := []string{
		strconv.FormatInt(time.Now().Unix(), 10),
		op,
		key,
		strconv.Itoa(hit),
		strconv.FormatInt(size, 10),
		strconv.FormatFloat(ttlRemaining, 'f', 2, 64),
		strconv.FormatInt(cacheUsed, 10),
	}
	csvWriter.Write(record)
}

// --------------------
// Item Struct
// --------------------
type Item struct {
	Value      []byte
	Size       int
	Frequency  int
	CreatedAt  int64
	LastAccess int64
	TTL        time.Duration
	ExpiresAt  time.Time
}

// --------------------
// Cache Struct
// --------------------
type Cache struct {
	mu          sync.RWMutex
	writeMu     sync.Mutex // serializes SetWithTTL/Delete end-to-end to prevent capacity overshoot
	items       map[string]*Item
	capacity    int64
	used        int64
	policy      eviction.EvictionPolicy
	onEvict     func(string)
	wal         *wal.WAL
	db          *badger.DB
	recovering  bool
	mlEvictCh   chan string
	stopWorkers chan struct{}
	snapshotInt time.Duration
}

// --------------------
// Constructor
// --------------------
func NewCache(capacity int64, policy eviction.EvictionPolicy, onEvict func(string), walInstance *wal.WAL, badgerDir string, snapshotInterval time.Duration) (*Cache, error) {
	opts := badger.DefaultOptions(badgerDir).WithLogger(nil)
	db, err := badger.Open(opts)
	if err != nil {
		return nil, err
	}

	c := &Cache{
		items:       make(map[string]*Item),
		capacity:    capacity,
		policy:      policy,
		onEvict:     onEvict,
		wal:         walInstance,
		db:          db,
		mlEvictCh:   make(chan string, 100),
		stopWorkers: make(chan struct{}),
		snapshotInt: snapshotInterval,
	}

	if err := c.LoadFromWAL(); err != nil {
		return nil, fmt.Errorf("failed WAL replay: %w", err)
	}

	go c.startMLEvictionWorker()
	go c.startTTLSweeper()
	go c.startSnapshotWorker()

	return c, nil
}

// --------------------
// Core Methods
// --------------------
func (c *Cache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	item, ok := c.items[key]
	if !ok {
		logAccess("GET", key, 0, 0, 0.0, c.used)
		return nil, false
	}

	item.Frequency++
	item.LastAccess = time.Now().Unix()
	c.policy.OnGet(key)

	out := append([]byte(nil), item.Value...)
	ttlRemaining := 0.0
	if !item.ExpiresAt.IsZero() {
		if r := time.Until(item.ExpiresAt).Seconds(); r > 0 {
			ttlRemaining = r
		}
	}
	logAccess("GET", key, 1, int64(item.Size), ttlRemaining, c.used)
	return out, true
}

func (c *Cache) Set(key string, value []byte) error {
	return c.SetWithTTL(key, value, 0)
}

func (c *Cache) SetWithTTL(key string, value []byte, ttl time.Duration) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if !c.recovering && c.wal != nil {
		if err := c.wal.Append(wal.Entry{
			Op:         "set",
			Key:        key,
			Value:      string(value),
			TTLSeconds: int64(ttl.Seconds()),
		}); err != nil {
			return err
		}
	}

	size := int64(len(value))
	if size > c.capacity {
		return ErrItemTooLarge
	}

	c.mu.Lock()
	if old, ok := c.items[key]; ok {
		c.used -= int64(old.Size)
	}
	usedAfterRemoval := c.used
	c.mu.Unlock()

	spaceNeeded := size
	if usedAfterRemoval+spaceNeeded > c.capacity {
		if err := c.evictToMakeSpace(spaceNeeded); err != nil {
			return fmt.Errorf("failed to make space for key %s: %w", key, err)
		}
	}

	c.mu.Lock()
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}

	c.items[key] = &Item{
		Value:      append([]byte(nil), value...),
		Size:       int(size),
		Frequency:  1,
		CreatedAt:  time.Now().Unix(),
		LastAccess: time.Now().Unix(),
		TTL:        ttl,
		ExpiresAt:  expiresAt,
	}
	c.used += size
	c.policy.OnSet(key)
	currentUsed := c.used
	c.mu.Unlock()

	if err := c.db.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte(key), value)
	}); err != nil {
		return fmt.Errorf("badger set failed: %w", err)
	}
	ttlRemaining := 0.0
	if ttl > 0 {
		ttlRemaining = ttl.Seconds()
	}
	logAccess("SET", key, 1, size, ttlRemaining, currentUsed)
	c.triggerMLEvictionIfNeeded()

	return nil
}

func (c *Cache) Delete(key string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.deleteLocked(key)
}

// deleteLocked performs the same work as Delete but assumes writeMu is already held by the caller.
func (c *Cache) deleteLocked(key string) error {
	if !c.recovering && c.wal != nil {
		if err := c.wal.Append(wal.Entry{Op: "delete", Key: key}); err != nil {
			return err
		}
	}

	c.mu.Lock()
	item, ok := c.items[key]
	if !ok {
		c.mu.Unlock()
		return nil
	}
	c.used -= int64(item.Size)
	delete(c.items, key)
	c.policy.OnDelete(key)
	c.mu.Unlock()

	if c.onEvict != nil {
		c.onEvict(key)
	}

	return c.db.Update(func(txn *badger.Txn) error {
		return txn.Delete([]byte(key))
	})
}

// --------------------
// Eviction
// --------------------
func (c *Cache) evictToMakeSpace(spaceNeeded int64) error {
	const maxEvictions = 10 // Reduced from 100
	evicted := 0
	attempted := make(map[string]bool)

	for evicted < maxEvictions {
		c.mu.RLock()
		currentUsed := c.used
		itemCount := len(c.items)
		c.mu.RUnlock()

		if currentUsed+spaceNeeded <= c.capacity {
			break // Space requirement satisfied
		}

		if itemCount == 0 {
			return fmt.Errorf("no items to evict")
		}

		// Choose eviction candidate

		if c.recovering {
			if mlp, isML := c.policy.(*eviction.MLPolicy); isML {
				evictKey, ok = mlp.ChooseEvictionDuringRecovery()
			} else {
				evictKey, ok = c.policy.ChooseEviction()
			}
		} else {
			evictKey, ok = c.policy.ChooseEviction()
		}
		if !ok {
			fmt.Printf("[Cache] No eviction candidate available\n")
			break // No candidate available
		}

		// Prevent infinite loops on the same key
		if attempted[evictKey] {
			fmt.Printf("[Cache] Key %s already attempted for eviction, breaking loop\n", evictKey)
			c.policy.OnDelete(evictKey)
			break
		}
		attempted[evictKey] = true

		// Verify the key actually exists before attempting deletion
		c.mu.RLock()
		_, exists := c.items[evictKey]
		c.mu.RUnlock()

		if !exists {
			fmt.Printf("[Cache] Policy returned non-existent key %s for eviction\n", evictKey)
			// This indicates a bug in the eviction policy
			break
		}

		// Perform the deletion
		if err := c.deleteLocked(evictKey); err != nil {
			fmt.Printf("[Cache] Failed to delete key %s during eviction: %v\n", evictKey, err)
			continue
		}

		evicted++
		fmt.Printf("[Cache] Successfully evicted key: %s (%d/%d)\n", evictKey, evicted, maxEvictions)
	}

	// Final check
	c.mu.RLock()
	finalUsed := c.used
	c.mu.RUnlock()

	if finalUsed+spaceNeeded > c.capacity {
		return fmt.Errorf("unable to free sufficient space: used=%d, needed=%d, capacity=%d, evicted=%d",
			finalUsed, spaceNeeded, c.capacity, evicted)
	}

	return nil
}

// --------------------
// WAL Recovery
// --------------------
func (c *Cache) LoadFromWAL() error {
	if c.wal == nil {
		return nil
	}

	entries, err := c.wal.ReadAll()
	if err != nil {
		return err
	}

	c.recovering = true
	defer func() { c.recovering = false }()

	for _, e := range entries {
		switch e.Op {
		case "set":
			c.SetWithTTL(e.Key, []byte(e.Value), time.Duration(e.TTLSeconds)*time.Second)
		case "delete":
			c.Delete(e.Key)
		}
	}
	return nil
}

// --------------------
// TTL
// --------------------
func (c *Cache) startTTLSweeper() {
	ticker := time.NewTicker(1 * time.Second)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-c.stopWorkers:
				return
			case <-ticker.C:
				c.CleanupExpiredItems()
			}
		}
	}()
}

// --------------------
// ML Eviction Worker
// --------------------
func (c *Cache) startMLEvictionWorker() {
	go func() {
		for {
			select {
			case <-c.stopWorkers:
				return
			case <-c.mlEvictCh:
				// FIX: Check if policy wraps an ML policy
				var mlPolicy *eviction.MLPolicy
				var ok bool

				// Direct ML policy
				if mlPolicy, ok = c.policy.(*eviction.MLPolicy); ok {
					// Direct ML policy
				} else {
					// Could be wrapped - add debug
					fmt.Printf("[Cache] Policy type: %T, not ML policy\n", c.policy)
					continue
				}

				if !mlPolicy.IsEnabled() {
					fmt.Printf("[Cache] ML policy found but not enabled\n")
					continue
				}

				c.mu.RLock()
				if len(c.items) == 0 {
					c.mu.RUnlock()
					continue
				}

				state := c.buildCacheStateUnsafe()
				c.mu.RUnlock()

				fmt.Printf("[Cache] Starting ML eviction with %d keys\n", len(state.Keys))
				evictKey, ok := mlPolicy.GetMLPrediction(state)
				if ok {
					fmt.Printf("[Cache] ML suggested evicting: %s\n", evictKey)
					c.Delete(evictKey)
				} else {
					fmt.Printf("[Cache] ML failed, using fallback\n")
					if fallbackKey, hasFallback := mlPolicy.ChooseEviction(); hasFallback {
						fmt.Printf("[Cache] Fallback evicting: %s\n", fallbackKey)
						c.Delete(fallbackKey)
					}
				}
			}
		}
	}()
}

// TriggerMLEviction allows external triggering of ML eviction
func (c *Cache) TriggerMLEviction() {
	select {
	case c.mlEvictCh <- "":
		fmt.Printf("[Cache] ML eviction triggered externally\n")
	default:
		fmt.Printf("[Cache] ML eviction channel full, skipping\n")
	}
}

func (c *Cache) triggerMLEvictionIfNeeded() {
	c.mu.RLock()
	usage := float64(c.used) / float64(c.capacity)
	c.mu.RUnlock()

	if usage <= 0.5 {
		return
	}

	// ML policies get proactive eviction via the async worker (real ML call).
	if _, isML := c.policy.(*eviction.MLPolicy); isML {
		select {
		case c.mlEvictCh <- "":
		default:
		}
		return
	}

	// Non-ML policies (LFU/LRU) get the same proactive threshold,
	// via the same eviction path they already use — keeps comparisons fair.
	c.mu.RLock()
	currentUsed := c.used
	c.mu.RUnlock()
	target := int64(float64(c.capacity) * 0.5)
	spaceToFree := currentUsed - target
	if spaceToFree > int64(float64(c.capacity)*0.05) {
		c.evictToMakeSpace(spaceToFree)
	}
}

// --------------------
// Snapshot Worker
// --------------------
func (c *Cache) startSnapshotWorker() {
	ticker := time.NewTicker(c.snapshotInt)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-c.stopWorkers:
				return
			case <-ticker.C:
				c.takeSnapshot()
			}
		}
	}()
}

func (c *Cache) takeSnapshot() {
	c.mu.RLock()
	defer c.mu.RUnlock()
	c.db.Update(func(txn *badger.Txn) error {
		for k, item := range c.items {
			e := badger.NewEntry([]byte(k), item.Value)
			txn.SetEntry(e)
		}
		return nil
	})
}

// --------------------
// Utility
// --------------------
func (c *Cache) Keys() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	keys := make([]string, 0, len(c.items))
	for k := range c.items {
		keys = append(keys, k)
	}
	return keys
}

// BuildCacheState snapshots live items into the format MLPolicy.GetMLPrediction expects.
func (c *Cache) BuildCacheState() eviction.CacheState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.buildCacheStateUnsafe()
}

func (c *Cache) UsedBytes() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.used
}

func (c *Cache) CapacityBytes() int64 {
	return c.capacity
}

func (c *Cache) StopWorkers() {
	close(c.stopWorkers)
	c.db.Close()
}

// GetCacheState returns the current cache state for ML prediction
func (c *Cache) GetCacheState() eviction.CacheState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.buildCacheStateUnsafe()
}

// --------------------
// ML/Policy State Builder
// --------------------
func (c *Cache) buildCacheStateUnsafe() eviction.CacheState {
	state := eviction.CacheState{
		Keys:     make([]string, 0, len(c.items)),
		Features: make(map[string]interface{}),
	}

	for k, v := range c.items {
		ttlRemaining := 0.0
		if !v.ExpiresAt.IsZero() {
			if remaining := time.Until(v.ExpiresAt).Seconds(); remaining > 0 {
				ttlRemaining = remaining
			}
		}

		state.Keys = append(state.Keys, k)

		// Ensure feature names exactly match what ML service expects
		state.Features[k] = map[string]interface{}{
			"freq":          v.Frequency,  // ML service expects 'freq'
			"last_access":   v.LastAccess, // ML service expects 'last_access'
			"size_bytes":    v.Size,       // ML service expects 'size_bytes'
			"ttl_remaining": ttlRemaining, // ML service expects 'ttl_remaining'
			"cache_used":    c.used,       // ML service expects 'cache_used'
		}
	}
	return state
}

// --------------------
// Missing Methods
// --------------------
func (c *Cache) Frequency(key string) int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if item, ok := c.items[key]; ok {
		return item.Frequency
	}
	return 0
}

func (c *Cache) CleanupExpiredItems() int {
	now := time.Now()
	expired := make([]string, 0)

	c.mu.RLock()
	for k, item := range c.items {
		if !item.ExpiresAt.IsZero() && item.ExpiresAt.Before(now) {
			expired = append(expired, k)
		}
	}
	c.mu.RUnlock()

	for _, key := range expired {
		c.Delete(key)
	}
	return len(expired)
}

func (c *Cache) ChooseEviction() (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.items) == 0 {
		return "", false
	}

	return c.policy.ChooseEviction()
}

// cache.go
func (c *Cache) SetOnEvict(callback func(key string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onEvict = callback
}
