package cluster

import (
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sumanth-github/ml-cache/cache"
	"github.com/sumanth-github/ml-cache/eviction"
	"github.com/sumanth-github/ml-cache/pkg/wal"
)

// Node represents a single cache node in the cluster
type Node struct {
	ID             int
	Capacity       int64 // This field EXISTS
	Cache          *cache.Cache
	evictionPolicy eviction.EvictionPolicy
	mlURL          string

	evictionMu       sync.Mutex
	lastEviction     time.Time
	evictionCooldown time.Duration

	// Metrics
	MetricUsed            prometheus.Gauge
	MetricEvictions       prometheus.Counter
	MetricMLEvictions     prometheus.Counter
	MetricTTLCleared      prometheus.Counter
	MetricHotReplications prometheus.Counter

	// Counters
	TotalEvictions int64
	MLEvictions    int64
	TTLExpired     int64
	stopWorkers    chan struct{}
	onGet          func(hit bool)
}

// NewNode initializes a node with a cache instance
func NewNode(
	id int,
	capacity int64,
	policy eviction.EvictionPolicy,
	mlURL string,
	walInstance *wal.WAL,
	badgerDir string,
	snapshotInterval time.Duration,
) *Node {
	if walInstance == nil {
		walFile := fmt.Sprintf("wal_node%d.log", id)
		var err error
		walInstance, err = wal.NewWAL(walFile)
		if err != nil {
			log.Fatalf("Failed to create WAL: %v", err)
		}
	}

	if badgerDir == "" {
		badgerDir = fmt.Sprintf("./badger%d", id)
	}
	if snapshotInterval == 0 {
		snapshotInterval = 5 * time.Minute
	}

	// Create the node first
	node := &Node{
		ID:               id,
		Capacity:         capacity,
		evictionPolicy:   policy,
		mlURL:            mlURL,
		evictionCooldown: 2 * time.Second,
		stopWorkers:      make(chan struct{}),
		MetricUsed: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: fmt.Sprintf("node_%d_used_bytes", id),
			Help: "Current used bytes in the node",
		}),
		MetricEvictions: prometheus.NewCounter(prometheus.CounterOpts{
			Name: fmt.Sprintf("node_%d_evictions_total", id),
			Help: "Total evictions in the node",
		}),
		MetricMLEvictions: prometheus.NewCounter(prometheus.CounterOpts{
			Name: fmt.Sprintf("node_%d_ml_evictions_total", id),
			Help: "Total ML-based evictions in the node",
		}),
		MetricTTLCleared: prometheus.NewCounter(prometheus.CounterOpts{
			Name: fmt.Sprintf("node_%d_ttl_cleared_total", id),
			Help: "Total TTL-based clears in the node",
		}),
		MetricHotReplications: prometheus.NewCounter(prometheus.CounterOpts{
			Name: fmt.Sprintf("node_%d_hot_replications_total", id),
			Help: "Total hot key replications performed by this node",
		}),
	}

	// Create eviction callback that captures the node properly
	onEvict := func(key string) {
		fmt.Printf("[Node %d] Evicted: %s\n", id, key)
		// Update counters safely
		node.TotalEvictions++
		node.MetricEvictions.Inc()
		// Update used bytes metric safely
		if node.Cache != nil {
			node.MetricUsed.Set(float64(node.Cache.UsedBytes()))
		}
	}

	// Create cache with the callback
	c, err := cache.NewCache(capacity, policy, onEvict, walInstance, badgerDir, snapshotInterval)
	if err != nil {
		log.Fatalf("Failed to create cache: %v", err)
	}

	// Set the cache reference
	node.Cache = c

	return node
}

// Add this helper method to the Node struct
func (n *Node) SetupMLTrackingCallback() {
	if mlPolicy, ok := n.evictionPolicy.(*eviction.MLPolicy); ok {
		mlPolicy.SetStateProvider(func() eviction.CacheState {
			return n.Cache.BuildCacheState()
		})
		mlPolicy.SetEvictionCallbacks(
			func(key string, isML bool) {
				if isML {
					n.MLEvictions++
					n.MetricMLEvictions.Inc()
					fmt.Printf("[Node %d] ML eviction tracked: %s\n", n.ID, key)
				}
			},
			func(key string) {
				fmt.Printf("[Node %d] Fallback eviction tracked: %s\n", n.ID, key)
			},
		)
	}
}

// Keys returns a snapshot of all keys in the node
func (n *Node) Keys() []string {
	if n.Cache == nil {
		return []string{}
	}
	return n.Cache.Keys()
}

// Set inserts a key-value pair
func (n *Node) Set(key string, value []byte) error {
	err := n.Cache.Set(key, value)
	if err == nil {
		n.MetricUsed.Set(float64(n.Cache.UsedBytes()))
	}
	return err
}

// SetWithTTL inserts a key-value pair with TTL
func (n *Node) SetWithTTL(key string, value []byte, ttlSeconds int64) error {
	err := n.Cache.SetWithTTL(key, value, time.Duration(ttlSeconds)*time.Second)
	if err == nil {
		n.MetricUsed.Set(float64(n.Cache.UsedBytes()))
	}
	return err
}

func (n *Node) SetOnGet(callback func(hit bool)) {
	n.onGet = callback
}

// Get retrieves a key
func (n *Node) Get(key string) ([]byte, bool) {
	value, ok := n.Cache.Get(key)
	if n.onGet != nil {
		n.onGet(ok)
	}
	return value, ok
}

func (n *Node) StopWorkers() {
	close(n.stopWorkers)
	n.Cache.StopWorkers()
}

func (n *Node) SetOnEvict(callback func(key string)) {
	n.Cache.SetOnEvict(callback)
}

// Delete removes a key
func (n *Node) Delete(key string) error {
	err := n.Cache.Delete(key)
	if err == nil {
		n.MetricUsed.Set(float64(n.Cache.UsedBytes()))
	}
	return err
}

// TriggerMLEviction - SIMPLIFIED VERSION
func (n *Node) TriggerMLEviction() {
	n.evictionMu.Lock()
	defer n.evictionMu.Unlock()

	if time.Since(n.lastEviction) < n.evictionCooldown {
		return
	}
	n.lastEviction = time.Now()

	fmt.Printf("[Node %d] Triggering ML eviction\n", n.ID)

	// Use cache's built-in ML eviction system instead of direct HTTP calls
	if n.Cache != nil {
		n.Cache.TriggerMLEviction()
	}
}

// CleanupTTL removes expired TTL items
func (n *Node) CleanupTTL() {
	count := n.Cache.CleanupExpiredItems()
	n.TTLExpired += int64(count)
	if count > 0 {
		n.MetricTTLCleared.Add(float64(count))
		n.MetricUsed.Set(float64(n.Cache.UsedBytes()))
	}
}

// TopNHotKeys returns top N keys by frequency
func (n *Node) TopNHotKeys(N int) []string {
	keys := n.Cache.Keys()
	type kv struct {
		Key  string
		Freq int
	}

	kvs := make([]kv, 0, len(keys))
	hotKeyCount := 0
	for _, key := range keys {
		freq := n.Cache.Frequency(key)
		kvs = append(kvs, kv{key, freq})
		if freq > 1 { // Log keys with frequency > 1
			log.Printf("[Debug] Node %d: %s has frequency %d", n.ID, key, freq)
			hotKeyCount++
		}
	}

	sort.Slice(kvs, func(i, j int) bool {
		return kvs[i].Freq > kvs[j].Freq
	})

	res := []string{}
	for i := 0; i < N && i < len(kvs); i++ {
		if kvs[i].Freq > 1 {
			res = append(res, kvs[i].Key)
		}
	}
	return res
}

// Status prints node usage stats
func (n *Node) Status() {
	fmt.Printf("Node %d: used %d/%d | Evicted=%d | ML=%d | TTL=%d\n",
		n.ID, n.Cache.UsedBytes(), n.Capacity, n.TotalEvictions, n.MLEvictions, n.TTLExpired)
}

// UsedBytes returns current usage of the node
func (n *Node) UsedBytes() int64 {
	return n.Cache.UsedBytes()
}

// MigrateHotKey migrates the hottest key to target node
func (n *Node) MigrateHotKey(target *Node) (string, bool) {
	keys := n.Cache.Keys()
	if len(keys) == 0 {
		return "", false
	}

	// find hottest key by frequency
	var hottestKey string
	maxFreq := -1
	for _, k := range keys {
		f := n.Cache.Frequency(k)
		if f > maxFreq {
			maxFreq = f
			hottestKey = k
		}
	}

	if hottestKey == "" {
		return "", false
	}

	val, ok := n.Cache.Get(hottestKey)
	if !ok {
		return "", false
	}

	// remove from current node
	if err := n.Cache.Delete(hottestKey); err != nil {
		return "", false
	}

	// insert into target node
	if err := target.Cache.Set(hottestKey, val); err != nil {
		return "", false
	}

	return hottestKey, true
}
