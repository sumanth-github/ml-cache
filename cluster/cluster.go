package cluster

import (
	"fmt"
	"hash/crc32"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// VirtualNode represents a point on the hash ring
type VirtualNode struct {
	Hash uint32
	Node *Node
}

// Cluster manages multiple nodes with virtual nodes
type Cluster struct {
	Nodes                    []*Node
	VirtualNodes             []VirtualNode
	Replicas                 int
	mu                       sync.RWMutex
	MetricHotKeyReplications prometheus.Counter
	keyOverrides             map[string]int
}

type NodeStatus struct {
	ID             int     `json:"id"`
	UsedBytes      int64   `json:"used_bytes"`
	Capacity       int64   `json:"capacity"`
	UsagePct       float64 `json:"usage_pct"`
	TotalEvictions int64   `json:"total_evictions"`
	MLEvictions    int64   `json:"ml_evictions"`
	TTLExpired     int64   `json:"ttl_expired"`
}

type ClusterStatus struct {
	Nodes        []NodeStatus `json:"nodes"`
	AverageUsage float64      `json:"average_usage"`
}

// NewClusterWithVirtualNodes creates a cluster with virtual nodes
func NewClusterWithVirtualNodes(nodes []*Node, replicas int) *Cluster {
	c := &Cluster{
		Nodes:        nodes,
		Replicas:     replicas,
		keyOverrides: make(map[string]int),
		MetricHotKeyReplications: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cluster_hot_key_replications_total",
			Help: "Total hot key replications across cluster",
		}),
	}

	for _, n := range nodes {
		for i := 0; i < replicas; i++ {
			hash := crc32.ChecksumIEEE([]byte(fmt.Sprintf("node-%d-%d", n.ID, i)))
			c.VirtualNodes = append(c.VirtualNodes, VirtualNode{Hash: hash, Node: n})
		}
	}

	sort.Slice(c.VirtualNodes, func(i, j int) bool {
		return c.VirtualNodes[i].Hash < c.VirtualNodes[j].Hash
	})
	if err := prometheus.Register(c.MetricHotKeyReplications); err != nil {
		if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
			log.Fatalf("failed to register metric: %v", err)
		}
	}

	return c
}

func (c *Cluster) SetKeyOverride(key string, nodeID int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keyOverrides[key] = nodeID
}

// getNodeForKey returns node via consistent hashing
func (c *Cluster) getNodeForKey(key string) *Node {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if nodeID, ok := c.keyOverrides[key]; ok {
		for _, n := range c.Nodes {
			if n.ID == nodeID {
				return n
			}
		}
	}

	if len(c.VirtualNodes) == 0 {
		return nil
	}

	hash := crc32.ChecksumIEEE([]byte(key))
	idx := sort.Search(len(c.VirtualNodes), func(i int) bool {
		return c.VirtualNodes[i].Hash >= hash
	})
	if idx == len(c.VirtualNodes) {
		idx = 0
	}
	return c.VirtualNodes[idx].Node
}

// Set inserts a key-value pair - FIXED: Removed excessive ML triggering
func (c *Cluster) Set(key string, value []byte) {
	node := c.getNodeForKey(key)
	if node == nil {
		return
	}
	// The cache.Set() method already handles ML eviction internally via triggerMLEvictionIfNeeded()
	node.Set(key, value)
	// REMOVED: node.TriggerMLEviction() - This was causing the infinite loop!
}

// SetWithTTL inserts a key-value pair with TTL
func (c *Cluster) SetWithTTL(key string, value []byte, ttlSeconds int64) error {
	node := c.getNodeForKey(key)
	if node == nil {
		return fmt.Errorf("no node available in cluster")
	}
	return node.SetWithTTL(key, value, ttlSeconds)
}

// Get retrieves a key
func (c *Cluster) Get(key string) ([]byte, bool) {
	node := c.getNodeForKey(key)
	if node == nil {
		return nil, false
	}
	return node.Get(key)
}

// Delete removes a key
func (c *Cluster) Delete(key string) error {
	node := c.getNodeForKey(key)
	if node == nil {
		return fmt.Errorf("no node available")
	}
	return node.Delete(key)
}

// Status prints cluster-wide stats
func (c *Cluster) Status() {
	fmt.Println("\n--- Cluster Metrics ---")
	for _, n := range c.Nodes {
		fmt.Printf("[Node %d] Used=%d/%d | Evicted=%d | ML Evictions=%d | TTL Cleared=%d\n",
			n.ID, n.Cache.UsedBytes(), n.Capacity, n.TotalEvictions, n.MLEvictions, n.TTLExpired)
	}
}

func (c *Cluster) StatusJSON() ClusterStatus {
	status := ClusterStatus{AverageUsage: c.AverageUsage()}
	for _, n := range c.Nodes {
		status.Nodes = append(status.Nodes, NodeStatus{
			ID:             n.ID,
			UsedBytes:      n.Cache.UsedBytes(),
			Capacity:       n.Capacity,
			UsagePct:       float64(n.Cache.UsedBytes()) / float64(n.Capacity) * 100,
			TotalEvictions: n.TotalEvictions,
			MLEvictions:    n.MLEvictions,
			TTLExpired:     n.TTLExpired,
		})
	}
	return status
}

// TopNHotKeys replication
func (c *Cluster) StartHotKeyReplication(topN int, interval time.Duration) {
	for _, n := range c.Nodes {
		go func(node *Node) {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for range ticker.C {
				keys := node.TopNHotKeys(topN)
				log.Printf("[Hot Key Replication] Node %d hot keys: %v", node.ID, keys)
				for _, key := range keys {
					go c.replicateKeyToNeighbor(node, key)
				}
			}
		}(n)
	}
}

func (c *Cluster) replicateKeyToNeighbor(src *Node, key string) {
	value, ok := src.Get(key)
	if !ok {
		return
	}

	neighbor := c.getNeighborNode(src, key)
	if neighbor == nil || neighbor == src {
		return
	}

	// Check if neighbor already has this key to avoid unnecessary writes
	if _, exists := neighbor.Get(key); !exists {
		if err := neighbor.Set(key, value); err == nil {
			// ✅ INCREMENT BOTH COUNTERS
			neighbor.MetricHotReplications.Inc() // Node-level metric
			c.MetricHotKeyReplications.Inc()     // Cluster-level metric
			log.Printf("[Hot Key Replication] Replicated %s: Node %d -> Node %d",
				key, src.ID, neighbor.ID)
		} else {
			log.Printf("[Hot Key Replication] Failed to replicate %s: %v", key, err)
		}
	} else {
		log.Printf("[Hot Key Replication] Key %s already exists in Node %d", key, neighbor.ID)
	}
}

func (c *Cluster) getNeighborNode(src *Node, key string) *Node {
	c.mu.RLock()
	defer c.mu.RUnlock()
	hash := crc32.ChecksumIEEE([]byte(key))
	idx := sort.Search(len(c.VirtualNodes), func(i int) bool {
		return c.VirtualNodes[i].Hash >= hash
	})
	if idx == len(c.VirtualNodes) {
		idx = 0
	}
	nextIdx := (idx + 1) % len(c.VirtualNodes)
	return c.VirtualNodes[nextIdx].Node
}

// AverageUsage returns cluster-wide average usage
func (c *Cluster) AverageUsage() float64 {
	totalUsed, totalCap := int64(0), int64(0)
	for _, n := range c.Nodes {
		totalUsed += n.Cache.UsedBytes()
		totalCap += n.Capacity
	}
	if totalCap == 0 {
		return 0
	}
	return float64(totalUsed) / float64(totalCap)
}

// LeastUsedNode returns the node with lowest usage
func (c *Cluster) LeastUsedNode() *Node {
	var minNode *Node
	minUsage := int64(1<<63 - 1)
	for _, n := range c.Nodes {
		u := n.UsedBytes()
		if u < minUsage {
			minUsage = u
			minNode = n
		}
	}
	return minNode
}

// DetectSkewedNodes identifies nodes with uneven access patterns
func (c *Cluster) DetectSkewedNodes() map[int]float64 {
	avgUsage := c.AverageUsage()
	skewedNodes := make(map[int]float64)

	for _, n := range c.Nodes {
		usage := float64(n.UsedBytes()) / float64(n.Capacity)
		skewRatio := usage / avgUsage

		// Consider nodes skewed if >2x average usage
		if skewRatio > 2.0 && usage > 0.7 {
			skewedNodes[n.ID] = skewRatio
			log.Printf("[Skew Detection] Node %d skewed: %.2fx average (%.2f usage)",
				n.ID, skewRatio, usage)
		}
	}
	return skewedNodes
}

// AdaptiveRebalance performs intelligent key migration
func (c *Cluster) AdaptiveRebalance() {
	skewedNodes := c.DetectSkewedNodes()
	if len(skewedNodes) == 0 {
		return
	}

	for nodeID, skewRatio := range skewedNodes {
		node := c.Nodes[nodeID]
		target := c.LeastUsedNode()

		if target != nil && target.ID != nodeID {
			// Migrate multiple hot keys for severely skewed nodes
			keysToMigrate := int(skewRatio)               // More skewed = more migrations
			for i := 0; i < keysToMigrate && i < 5; i++ { // Max 5 migrations per round
				if key, ok := node.MigrateHotKey(target); ok {
					c.SetKeyOverride(key, target.ID)
					log.Printf("[Adaptive Rebalance] Migrated hot key %s Node %d -> Node %d",
						key, nodeID, target.ID)
				}
			}
		}
	}
}
