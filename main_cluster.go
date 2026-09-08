package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sumanth-github/ml-cache/cluster"
	"github.com/sumanth-github/ml-cache/eviction"
	"github.com/sumanth-github/ml-cache/pkg/wal"
)

var (
	// Per-node metrics
	clusterCacheHits = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cluster_cache_hits_total",
			Help: "Total cache hits per node",
		}, []string{"node", "policy"},
	)
	clusterCacheMisses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cluster_cache_misses_total",
			Help: "Total cache misses per node",
		}, []string{"node", "policy"},
	)
	clusterCacheEvictions = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cluster_cache_evictions_total",
			Help: "Total cache evictions per node",
		}, []string{"node", "policy"},
	)
	clusterCacheUsedBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cluster_cache_bytes_used",
			Help: "Current cache usage per node",
		}, []string{"node", "policy"},
	)

	// Cluster-wide aggregated metrics
	clusterTotalKeys = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cluster_total_keys",
		Help: "Total keys across all nodes",
	})
	clusterTotalMLEvictions = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cluster_total_ml_evictions",
		Help: "Total ML evictions across cluster",
	})
	clusterTotalEvictions = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cluster_total_evictions",
		Help: "Total evictions across cluster",
	})
	clusterTotalTTLExpired = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cluster_total_ttl_expired",
		Help: "Total TTL expired items across cluster",
	})
	clusterTotalUsedBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cluster_total_used_bytes",
		Help: "Total used bytes across cluster",
	})
	clusterTotalCapacity = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cluster_total_capacity",
		Help: "Total capacity across cluster",
	})
	clusterAverageUsage = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cluster_average_usage",
		Help: "Average usage ratio across cluster",
	})
	clusterMLEfficiency = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cluster_ml_efficiency_ratio",
		Help: "Ratio of ML evictions to total evictions",
	})
	//clusterHotKeyReplications = prometheus.NewCounter(prometheus.CounterOpts{
	//	Name: "cluster_hot_key_replications_total",
	//	Help: "Total hot key replications across cluster",
	//})
	clusterUptime = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cluster_uptime_seconds",
		Help: "Cluster uptime in seconds",
	})

	// Policy comparison metrics
	policyEvictions = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cache_evictions_by_policy_total",
			Help: "Total evictions by policy type",
		}, []string{"policy", "node"},
	)

	policyHitRate = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cache_hit_rate_by_policy",
			Help: "Hit rate percentage by policy",
		}, []string{"policy", "node"},
	)

	policyResponseTime = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "policy_response_time_seconds",
			Help: "Response time by policy type",
		}, []string{"policy"},
	)
)

func init() {
	prometheus.MustRegister(
		clusterCacheHits, clusterCacheMisses, clusterCacheEvictions, clusterCacheUsedBytes,
		clusterTotalKeys, clusterTotalMLEvictions, clusterTotalEvictions,
		clusterTotalTTLExpired, clusterTotalUsedBytes, clusterTotalCapacity,
		clusterAverageUsage, clusterMLEfficiency, clusterUptime,
		policyEvictions, policyHitRate, policyResponseTime,
	)
}

func createPolicyVariants() map[string]eviction.EvictionPolicy {
	policies := make(map[string]eviction.EvictionPolicy)

	// Pure policies
	policies["LFU"] = eviction.NewLFU()
	policies["LRU"] = eviction.NewLRU()

	// ML-wrapped policies
	policies["ML_LFU"] = eviction.NewMLWrapperWithConfig(
		eviction.NewLFU(),
		"http://localhost:8000/predict",
		300*time.Millisecond,
		true, "ml_lfu",
	)
	policies["ML_LRU"] = eviction.NewMLWrapperWithConfig(
		eviction.NewLRU(),
		"http://localhost:8000/predict",
		300*time.Millisecond,
		true,
		"ml_lru",
	)

	return policies
}

func main() {
	startTime := time.Now()
	log.Println("[Cluster] Starting multi-node cache cluster...")

	// ---- Initialize WALs for 4 nodes ----
	walInstances := make([]*wal.WAL, 4)
	for i := 0; i < 4; i++ {
		w, err := wal.NewWAL(fmt.Sprintf("wal_node_%d.log", i))
		if err != nil {
			log.Fatalf("[WAL] Failed to create WAL for node %d: %v", i, err)
		}
		walInstances[i] = w
	}

	// ---- Create nodes with different policies ----
	policies := createPolicyVariants()
	policyNames := []string{"LFU", "LRU", "ML_LFU", "ML_LRU"}

	nodes := []*cluster.Node{
		cluster.NewNode(0, 5000, policies["LFU"], "http://localhost:8000", walInstances[0], "./badger0", 5*time.Minute),
		cluster.NewNode(1, 5000, policies["LRU"], "http://localhost:8000", walInstances[1], "./badger1", 5*time.Minute),
		cluster.NewNode(2, 5000, policies["ML_LFU"], "http://localhost:8000", walInstances[2], "./badger2", 5*time.Minute),
		cluster.NewNode(3, 5000, policies["ML_LRU"], "http://localhost:8000", walInstances[3], "./badger3", 5*time.Minute),
	}

	log.Printf("[Cluster] Created 4 nodes with policies: %v", policyNames)

	for _, p := range policies {
		if mlp, ok := p.(*eviction.MLPolicy); ok {
			for _, m := range []prometheus.Collector{mlp.MetricCooldownSkipped, mlp.MetricCircuitOpenSkipped} {
				if err := prometheus.Register(m); err != nil {
					if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
						log.Fatalf("failed to register metric: %v", err)
					}
				}
			}
		}
	}

	for _, n := range nodes {
		metrics := []prometheus.Collector{
			n.MetricUsed, n.MetricEvictions, n.MetricMLEvictions,
			n.MetricTTLCleared, n.MetricHotReplications,
		}
		for _, m := range metrics {
			if err := prometheus.Register(m); err != nil {
				if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
					log.Fatalf("failed to register metric: %v", err)
				}
			}
		}
	}
	for i, n := range nodes {
		policyName := policyNames[i]
		if policyName == "ML_LFU" || policyName == "ML_LRU" {
			log.Printf("[Setup] Setting up ML tracking for Node %d (%s)", n.ID, policyName)
			n.SetupMLTrackingCallback()
		}
	}

	// ---- Create cluster with virtual nodes ----
	c := cluster.NewClusterWithVirtualNodes(nodes, 100)
	log.Println("[Cluster] Cluster created with 100 virtual nodes per real node")

	// ---- Set Prometheus callbacks per node with policy labels ----
	// ---- Set Prometheus callbacks per node with policy labels ----
	for i, n := range nodes {
		nodeLabel := fmt.Sprintf("%d", n.ID)
		policyLabel := policyNames[i]

		// FIX: Capture variables in closure
		func(nd *cluster.Node, nodeID, policy string) {
			// Eviction callback with policy tracking
			nd.SetOnEvict(func(key string) {
				log.Printf("[Eviction Callback] Node %s (%s) evicted: %s", nodeID, policy, key)
				clusterCacheEvictions.WithLabelValues(nodeID, policy).Inc()
				policyEvictions.WithLabelValues(policy, nodeID).Inc()
			})

			// GET callback with policy tracking
			nd.SetOnGet(func(hit bool) {
				if hit {
					clusterCacheHits.WithLabelValues(nodeID, policy).Inc()
				} else {
					clusterCacheMisses.WithLabelValues(nodeID, policy).Inc()
				}
			})
		}(n, nodeLabel, policyLabel)

		// Periodic update for used bytes and hit rates
		go func(nd *cluster.Node, label string, policy string) {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				usage := nd.UsedBytes()
				clusterCacheUsedBytes.WithLabelValues(label, policy).Set(float64(usage))

				// Update metrics counters
				nd.MetricUsed.Set(float64(usage))
			}
		}(n, nodeLabel, policyLabel)
	}

	// ---- Start Prometheus endpoint with enhanced metrics ----
	go func() {
		// Setup HTTP handlers
		http.Handle("/metrics", promhttp.Handler())

		// Health endpoint
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			status := map[string]interface{}{
				"status":     "healthy",
				"nodes":      len(nodes),
				"uptime":     time.Since(startTime).Seconds(),
				"ml_enabled": true,
				"policies":   policyNames,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(status)
		})

		// Cluster status endpoint
		http.HandleFunc("/cluster/status", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(c.StatusJSON())
		})

		log.Println("[Prometheus] Metrics exposed at :2112/metrics")
		log.Println("[Health] Health check at :2112/health")
		log.Println("[Status] Cluster status at :2112/cluster/status")

		// Start HTTP server in separate goroutine
		go func() {
			if err := http.ListenAndServe(":2112", nil); err != nil {
				log.Fatalf("[Prometheus] Failed to start server: %v", err)
			}
		}()

		// Periodic cluster metrics aggregation
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			// Aggregate cluster-wide metrics
			totalKeys := 0
			totalMLEvictions := int64(0)
			totalEvictions := int64(0)
			totalTTLExpired := int64(0)
			totalUsedBytes := int64(0)
			totalCapacity := int64(0)

			for i, n := range nodes {
				totalKeys += len(n.Cache.Keys())
				totalMLEvictions += n.MLEvictions
				totalEvictions += n.TotalEvictions
				totalTTLExpired += n.TTLExpired
				totalUsedBytes += n.UsedBytes()
				totalCapacity += n.Capacity

				// Log individual node stats with policy
				log.Printf("[Node %d] Policy: %s, Keys: %d, Used: %d/%d (%.1f%%), ML Evictions: %d",
					n.ID, policyNames[i], len(n.Cache.Keys()), n.UsedBytes(), n.Capacity,
					float64(n.UsedBytes())/float64(n.Capacity)*100, n.MLEvictions)
			}

			// Update cluster-level Prometheus gauges
			clusterTotalKeys.Set(float64(totalKeys))
			clusterTotalMLEvictions.Set(float64(totalMLEvictions))
			clusterTotalEvictions.Set(float64(totalEvictions))
			clusterTotalTTLExpired.Set(float64(totalTTLExpired))
			clusterTotalUsedBytes.Set(float64(totalUsedBytes))
			clusterTotalCapacity.Set(float64(totalCapacity))
			clusterAverageUsage.Set(c.AverageUsage())
			clusterUptime.Set(time.Since(startTime).Seconds())

			// Calculate efficiency metrics
			if totalEvictions > 0 {
				mlEfficiencyRatio := float64(totalMLEvictions) / float64(totalEvictions)
				clusterMLEfficiency.Set(mlEfficiencyRatio)
			}

			// Log cluster status for monitoring
			log.Printf("[Cluster Metrics] Keys=%d, Used=%d/%d (%.1f%%), ML Evictions=%d/%d (%.1f%%)",
				totalKeys, totalUsedBytes, totalCapacity,
				float64(totalUsedBytes)/float64(totalCapacity)*100,
				totalMLEvictions, totalEvictions,
				float64(totalMLEvictions)/float64(totalEvictions)*100)
		}
	}()

	// ---- Start Hot Key Replication ----
	log.Println("[Cluster] Starting hot key replication...")
	c.StartHotKeyReplication(3, 10*time.Second)

	// ---- Background workers ----
	for _, n := range nodes {
		go func(nd *cluster.Node) {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()

			for range ticker.C {
				usage := float64(nd.UsedBytes()) / float64(nd.Capacity)
				log.Printf("[Background Worker] Node %d usage: %.2f (%d/%d bytes)",
					nd.ID, usage, nd.UsedBytes(), nd.Capacity)

				if usage > 0.95 {
					log.Printf("[Background Worker] Node %d critically full (%.2f), triggering ML eviction", nd.ID, usage)
					nd.TriggerMLEviction()
				}

				// Simple adaptive partitioning
				avgUsage := c.AverageUsage()
				if usage > avgUsage*2.0 {
					target := c.LeastUsedNode()
					if target != nil && target.ID != nd.ID {
						targetUsage := float64(target.UsedBytes()) / float64(target.Capacity)
						if targetUsage < avgUsage*0.5 {
							if key, ok := nd.MigrateHotKey(target); ok {
								c.SetKeyOverride(key, target.ID)
								log.Printf("[Adaptive Partitioning] Migrated hot key %s Node %d -> Node %d", key, nd.ID, target.ID)
							}
						}
					}
				}
			}
		}(n)
	}

	// ---- Insert initial dataset ----
	log.Println("[Setup] Inserting initial dataset...")
	for i := 0; i < 50; i++ {
		key := fmt.Sprintf("key%d", i)
		val := fmt.Sprintf("value%d_data_content_longer_to_test_capacity", i)
		c.Set(key, []byte(val))

		if i%10 == 0 {
			time.Sleep(100 * time.Millisecond)
			log.Printf("[Setup] Inserted %d keys...", i+1)
		}
	}

	// ---- Status check ----
	log.Println("[Setup] Initial setup complete. Node status:")
	for i, n := range nodes {
		usage := float64(n.UsedBytes()) / float64(n.Capacity)
		log.Printf("  Node %d (%s): %d/%d bytes (%.2f%% full)",
			n.ID, policyNames[i], n.UsedBytes(), n.Capacity, usage*100)
	}

	// ---- Test ML eviction on ML nodes ----
	log.Println("[Test] Testing ML eviction on ML nodes...")
	nodes[2].TriggerMLEviction() // ML_LFU node
	nodes[3].TriggerMLEviction() // ML_LRU node
	time.Sleep(2 * time.Second)

	// ---- Start comparison load simulation ----
	// ---- Start comparison load simulation (repeats until shutdown) ----
	go func() {
		for {
			simulateComparisonLoad(c)
			log.Println("[Comparison Load] Cycle complete, restarting...")
		}
	}()

	// ---- Graceful shutdown ----
	cChan := make(chan os.Signal, 1)
	signal.Notify(cChan, os.Interrupt, syscall.SIGTERM)

	log.Println("[Cluster] Cluster is running. Press Ctrl+C to shutdown.")
	log.Println("[Cluster] Monitor at http://localhost:2112/metrics")
	log.Println("[Cluster] Health check at http://localhost:2112/health")
	log.Printf("[Cluster] Policy comparison: %v", policyNames)

	<-cChan
	log.Println("[Cluster] Shutting down gracefully...")
	for _, n := range nodes {
		n.StopWorkers()
	}

	// Close WAL instances
	for _, w := range walInstances {
		w.Close()
	}

	log.Println("[Cluster] Shutdown complete.")
}

func simulateComparisonLoad(c *cluster.Cluster) {
	time.Sleep(5 * time.Second) // Wait for initial setup
	log.Println("[Comparison Load] Starting policy comparison load...")

	// Hot keys (20% of keys, 80% of accesses)
	hotKeyPool := []string{"hot_key_1", "hot_key_2", "hot_key_3", "hot_key_4", "hot_key_5",
		"hot_key_6", "hot_key_7", "hot_key_8", "hot_key_9", "hot_key_10"}
	hotKeys := hotKeyPool[:5] // active hot set, rotates below

	for _, key := range hotKeys {
		padding := strings.Repeat("x", rand.Intn(200)) // vary payload size realistically
		value := fmt.Sprintf("hot_value_%s_content_%s", key, padding)
		c.Set(key, []byte(value))
	}

	for i := 0; i < 20000; i++ {
		if i%2000 == 0 && i > 0 {
			// rotate popularity: swap one hot key out for a previously-cold one every 2000 ops
			shift := (i / 2000) % (len(hotKeyPool) - 4)
			hotKeys = hotKeyPool[shift : shift+5]
		}
		if rand.Float64() < 0.9 {
			// Access hot key
			key := hotKeys[rand.Intn(len(hotKeys))]
			c.Get(key)
		} else {
			// 50% chance of new cold key, 50% chance of existing cold key
			if rand.Float64() < 0.5 {
				// Insert new cold key
				key := fmt.Sprintf("cold_key_%d", rand.Intn(10000))
				value := fmt.Sprintf("cold_value_%d_content_data", rand.Intn(10000))
				c.Set(key, []byte(value))
			} else {
				// Access existing cold key
				key := fmt.Sprintf("cold_key_%d", rand.Intn(100))
				c.Get(key)
			}
		}

		// Small delay to spread load over time
		if i%100 == 0 {
			time.Sleep(100 * time.Millisecond)
			log.Printf("[Comparison Load] Completed %d/20000 operations", i)
		}
	}

	log.Println("[Comparison Load] Policy comparison load simulation complete")
}
