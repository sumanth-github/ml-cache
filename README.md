# ML-Cache: Distributed Cache Cluster with ML-Driven Eviction

A distributed, in-memory cache cluster written in Go, built to explore whether a machine-learned eviction policy can outperform classic LRU/LFU under realistic access patterns. The cluster runs four nodes side by side — LFU, LRU, ML-wrapped-LFU, and ML-wrapped-LRU — so the policies can be compared head-to-head under identical load, all instrumented with Prometheus.

This is a learning/portfolio project built incrementally, with a deliberate focus on production-quality concerns: concurrency safety, durability (WAL + snapshotting), observability, and graceful degradation when the ML service is slow or down.

---

## Table of Contents

- [Architecture Overview](#architecture-overview)
- [How Eviction Works](#how-eviction-works)
- [ML Eviction Pipeline](#ml-eviction-pipeline)
- [Persistence & Recovery](#persistence--recovery)
- [Cluster Topology & Hot Key Handling](#cluster-topology--hot-key-handling)
- [Observability](#observability)
- [Project Structure](#project-structure)
- [Getting Started](#getting-started)
- [API Reference](#api-reference)
- [Load Testing](#load-testing)
- [Known Issues & Limitations](#known-issues--limitations)
- [Key Design Decisions & Lessons Learned](#key-design-decisions--lessons-learned)
- [Roadmap](#roadmap)

---

## Architecture Overview

```
                        ┌─────────────────────────┐
                        │   Cluster (consistent    │
                        │   hashing, virtual nodes)│
                        └────────────┬─────────────┘
                                     │
        ┌──────────────┬────────────┼────────────┬──────────────┐
        │              │            │             │              │
   ┌────▼───┐     ┌────▼───┐   ┌────▼───┐    ┌────▼───┐          │
   │ Node 0 │     │ Node 1 │   │ Node 2 │    │ Node 3 │          │
   │  LFU   │     │  LRU   │   │ ML_LFU │    │ ML_LRU │          │
   └────┬───┘     └────┬───┘   └────┬───┘    └────┬───┘          │
        │              │            │             │              │
   ┌────▼──────────────▼────────────▼─────────────▼───┐          │
   │  Each node: in-memory cache + WAL + BadgerDB       │          │
   │  snapshot + eviction policy + Prometheus metrics   │          │
   └─────────────────────────────────────────────────┬─┘          │
                                                       │            │
                                              ┌────────▼────────────▼───┐
                                              │  ML Eviction Service     │
                                              │  (FastAPI + sklearn)     │
                                              │  POST /predict           │
                                              └──────────────────────────┘
```

The cluster runs **4 nodes**, each assigned a different eviction policy so all four can be compared under the same simulated load in real time:

| Node | Policy   | Description                                  |
|------|----------|-----------------------------------------------|
| 0    | `LFU`    | Pure least-frequently-used                    |
| 1    | `LRU`    | Pure least-recently-used                      |
| 2    | `ML_LFU` | LFU base policy, ML-first eviction wrapper     |
| 3    | `ML_LRU` | LRU base policy, ML-first eviction wrapper     |

Keys are distributed across nodes via **consistent hashing with virtual nodes** (100 virtual nodes per physical node), giving even key distribution and small blast radius on rebalancing.

---

## How Eviction Works

Each node's `Cache` holds items in an in-memory map guarded by an `RWMutex`. On every `Set`, if usage exceeds capacity, `evictToMakeSpace` repeatedly asks the configured `EvictionPolicy` for a candidate key and deletes it until enough space is freed (capped at 10 evictions per call to avoid runaway loops).

All eviction policies implement a common interface:

```go
type EvictionPolicy interface {
    OnGet(key string)
    OnSet(key string)
    OnDelete(key string)
    ChooseEviction() (key string, ok bool)
}
```

- **LFU** — bucketed frequency lists (`freq -> doubly linked list of keys`), O(1) increment and eviction of the true minimum-frequency key.
- **LRU** — standard doubly linked list + map, O(1) access-order tracking.
- **MLPolicy** — wraps either base policy. On `ChooseEviction`, it first asks the ML service for a prediction; if the ML call fails, times out, is in cooldown, or the circuit breaker is open, it **falls back to the wrapped base policy** transparently.

### Proactive vs. reactive eviction

Beyond the "evict on overflow" path, `Cache.triggerMLEvictionIfNeeded()` runs after every `Set` and proactively evicts once usage crosses **50%** of capacity — before the cache is actually full. ML-wrapped nodes get this pushed onto an async worker (via a buffered channel) so the real HTTP round-trip to the ML service doesn't block the write path. Non-ML nodes get the same 50% proactive threshold via the same synchronous eviction path, so the comparison between policies stays apples-to-apples.

---

## ML Eviction Pipeline

### Request flow

1. `Cache.buildCacheStateUnsafe()` snapshots all live items into a `CacheState{Keys, Features}` payload.
2. `MLPolicy.GetMLPrediction()` POSTs this to the ML service's `/predict` endpoint (default `http://localhost:8000/predict`), with per-policy timeout (300ms in `main_cluster.go`).
3. The ML service (`ml_eviction_service.py`, FastAPI) converts features into a DataFrame matching the trained model's schema, runs `model.predict_proba`, and returns the key with the highest eviction probability.
4. On success, that key is evicted. On any failure (timeout, non-200, malformed response, model not loaded), the policy transparently falls back to the wrapped base policy (LFU or LRU).

### Feature schema

Five features are used consistently across training and serving:

| Feature            | Description                                             |
|---------------------|-----------------------------------------------------------|
| `freq`              | Access frequency counter                                 |
| `last_access_gap`   | Seconds since the item's previous access (causal, not raw timestamp) |
| `size_bytes`        | Item size in bytes                                        |
| `ttl_remaining`      | Seconds remaining before TTL expiry (0 if no TTL set)      |
| `cache_used`         | Total bytes currently used in the node's cache             |

`buildCacheStateUnsafe` is the **single source of truth** for feature construction on the Go side — both the proactive ML worker and the on-demand `MLPolicy.ChooseEviction()` path route through it, so training and serving features can't silently drift apart.

### Reliability mechanisms

- **Cooldown** — no more than one ML call per 100ms per policy instance, to avoid hammering the service under heavy write load.
- **Circuit breaker** — after 3 consecutive failures, the circuit opens for 10 seconds and all calls fall back to the base policy without attempting the network call.
- **Timeout** — HTTP client timeout (300ms in production config) bounds worst-case latency impact on the write path.

### Model training

1. `clean_access_log.py` — strips malformed/corrupted rows from the raw `access_log.csv` produced by the cache's access logger.
2. `build_training_data.py` — walks the cleaned log causally (no lookahead into the future beyond computing reuse distance as a label), builds the 5-feature schema per GET-hit event, and labels the bottom 30% by reuse distance as "keep" (0) and the rest as "evict candidate" (1).
3. `train_eviction_model_v2.py` — trains a `RandomForestClassifier` (100 trees, max depth 10, balanced class weights) on an 80/20 stratified split, prints a classification report and feature importances, and saves `eviction_model.pkl`.

---

## Persistence & Recovery

Each node has its own **write-ahead log** (`wal_node_N.log`) and **BadgerDB** instance (`./badgerN`) for durability:

- Every `Set`/`Delete` is appended to the WAL (tab-delimited: `op\tkey\tvalue\tttl_seconds`) and flushed synchronously before the in-memory mutation is applied.
- On startup, `Cache.LoadFromWAL()` replays all WAL entries in order to rebuild in-memory state. During replay (`c.recovering = true`), the ML policy is **bypassed** — `ChooseEvictionDuringRecovery()` routes straight to the base policy, so recovery never depends on a possibly-unavailable ML service being up.
- BadgerDB provides a secondary on-disk store; a background snapshot worker periodically flushes current in-memory items into Badger as a point-in-time backup.

---

## Cluster Topology & Hot Key Handling

- **Consistent hashing with virtual nodes**: each physical node gets 100 virtual points on the hash ring (`crc32` of `node-{id}-{replica}`), giving smooth key distribution.
- **Key overrides**: `Cluster.SetKeyOverride(key, nodeID)` lets specific keys be pinned to a node regardless of hash — used by hot-key migration so a moved key doesn't immediately hash back to its original node.
- **Hot key replication**: every node periodically identifies its top-N most frequently accessed keys and asynchronously replicates them to a neighboring node on the ring, reducing single-node hotspot risk.
- **Adaptive rebalancing**: `Cluster.DetectSkewedNodes()` flags nodes running >2x average cluster usage; `AdaptiveRebalance()` migrates hot keys (up to 5 per round) from skewed nodes to the least-used node, updating key overrides so future reads/writes route correctly.

---

## Observability

Prometheus metrics are exposed on `:2112/metrics`. Metrics are organized at three levels:

**Per-node** (`node.go`):
- `node_{id}_used_bytes`, `node_{id}_evictions_total`, `node_{id}_ml_evictions_total`, `node_{id}_ttl_cleared_total`, `node_{id}_hot_replications_total`

**Per-policy comparison** (`main_cluster.go`), labeled by `node` and `policy`:
- `cluster_cache_hits_total`, `cluster_cache_misses_total`, `cluster_cache_evictions_total`, `cluster_cache_bytes_used`
- `cache_evictions_by_policy_total`, `cache_hit_rate_by_policy`, `policy_response_time_seconds`

**Cluster-wide aggregates**, updated every 10s:
- `cluster_total_keys`, `cluster_total_ml_evictions`, `cluster_total_evictions`, `cluster_total_ttl_expired`, `cluster_total_used_bytes`, `cluster_total_capacity`, `cluster_average_usage`, `cluster_ml_efficiency_ratio`, `cluster_uptime_seconds`

**ML reliability metrics** (`ml_policy.go`):
- `ml_skipped_cooldown_total`, `ml_circuit_open_total` — one pair per ML-wrapped policy instance, letting you see how often ML predictions are being skipped due to rate limiting vs. circuit breaker trips.

Additional endpoints:
- `GET /health` — basic liveness + policy list
- `GET /cluster/status` — JSON snapshot of per-node usage, evictions, and ML eviction counts

The ML service (`ml_eviction_service.py`) exposes its own metrics at `:8000/metrics`, including `ml_predictions_total` and `ml_prediction_duration_seconds`, plus a human-readable status page at `/` and `/debug/features` for inspecting what the loaded model expects.

Both Go and Python services are scraped by Prometheus (`prometheus.yml`) and can be visualized in Grafana; both ship via `docker-compose.yml`.

---

## Project Structure

```
.
├── main_cluster.go              # Entrypoint: cluster wiring, metrics registration, load simulation
├── cluster/
│   ├── cluster.go                # Consistent hashing, hot key replication, rebalancing
│   └── node.go                   # Per-node cache wrapper, metrics, ML trigger hooks
├── cache/
│   └── cache.go                  # Core cache: get/set/delete, eviction loop, WAL replay, ML worker
├── eviction/
│   ├── policy.go                 # EvictionPolicy interface
│   ├── lfu.go                    # LFU implementation
│   ├── lru.go                    # LRU implementation
│   └── ml_policy.go               # ML wrapper: HTTP calls, cooldown, circuit breaker
├── pkg/wal/
│   └── wal.go                     # Write-ahead log: append, replay
├── metrics/
│   └── metrics.go                 # Shared Prometheus counters
├── ml_eviction_service.py         # FastAPI ML prediction service
├── build_training_data.py         # Builds labeled training set from access log
├── train_eviction_model_v2.py     # Trains RandomForestClassifier, saves eviction_model.pkl
├── clean_access_log.py            # Sanitizes raw access log CSV
├── prometheus.yml                 # Prometheus scrape config
├── docker-compose.yml             # Prometheus + Grafana stack
└── load_test.sh                   # Simple bash load generator
```

---

## Getting Started

### Prerequisites

- Go 1.24+
- Python 3.9+ with `fastapi`, `uvicorn`, `pandas`, `scikit-learn`, `joblib`, `prometheus_client`
- Docker + Docker Compose (for Prometheus/Grafana)

### 1. Train the model (optional — a pretrained model can be dropped in instead)

```bash
python3 clean_access_log.py access_log.csv access_log_clean.csv
python3 build_training_data.py
python3 train_eviction_model_v2.py
```

This produces `eviction_model.pkl`, expected at the ML service's working directory (or `ml_files/eviction_model.pkl`).

### 2. Start the ML eviction service

```bash
python3 ml_eviction_service.py
# or: uvicorn ml_eviction_service:app --host 0.0.0.0 --port 8000
```

Visit `http://localhost:8000` for a status page, or `http://localhost:8000/debug/features` to confirm the loaded model's expected feature schema.

### 3. Start the cache cluster

```bash
go run main_cluster.go
```

This boots 4 nodes (LFU, LRU, ML_LFU, ML_LRU), each with its own WAL and BadgerDB directory, inserts a small seed dataset, and starts a continuous background load simulator that mixes hot-key and cold-key traffic to exercise all eviction policies comparably.

### 4. Start Prometheus + Grafana

```bash
docker-compose up -d
```

Prometheus: `http://localhost:9090` · Grafana: `http://localhost:3000` (default login `admin` / `admin`)

---

## API Reference

### Cache cluster (`:2112`)

| Endpoint          | Method | Description                        |
|--------------------|--------|-------------------------------------|
| `/metrics`          | GET    | Prometheus metrics                  |
| `/health`           | GET    | Liveness + policy list               |
| `/cluster/status`   | GET    | Per-node usage/eviction JSON snapshot |

> Note: the Go service's key `set`/`get`/`delete` HTTP routes referenced by `load_test.sh` (e.g. `/v1/set`) are expected on a separate API port; wire these up alongside the metrics server if not already present in your local branch.

### ML service (`:8000`)

| Endpoint            | Method | Description                                  |
|----------------------|--------|------------------------------------------------|
| `/`                    | GET    | HTML status page                                |
| `/health`              | GET    | Model load status + prediction count             |
| `/debug/features`       | GET    | Expected feature names/count for loaded model      |
| `/predict`             | POST   | Main eviction prediction endpoint                  |
| `/metrics`             | GET    | Prometheus metrics                               |

`POST /predict` request shape:

```json
{
  "keys": ["key1", "key2"],
  "features": {
    "key1": {"freq": 5, "last_access": 1000, "size_bytes": 100, "ttl_remaining": 0, "cache_used": 1000},
    "key2": {"freq": 2, "last_access": 500,  "size_bytes": 200, "ttl_remaining": 0, "cache_used": 1000}
  }
}
```

Response:

```json
{"evict_key": "key2", "probability": 0.83}
```

---

## Load Testing

`load_test.sh` inserts 15,000 keys with ~10KB random payloads via repeated `curl` calls to exercise capacity limits and eviction under sustained write pressure. The built-in `simulateComparisonLoad` in `main_cluster.go` runs continuously once the cluster starts, generating a realistic 90/10 hot/cold key access pattern with periodic hot-key rotation — this is what actually drives the live policy comparison across the 4 nodes.

---

## Known Issues & Limitations

This section is kept intentionally honest — it reflects the current state of an actively-being-debugged learning project, not a finished product.

### Active

- **ML prediction latency metric is not trustworthy.** In `ml_eviction_service.py`, the `ml_prediction_duration_seconds` histogram only wraps the counter increment, not the actual prediction handler body:
  ```python
  with ml_prediction_duration.time():
      ml_predictions_total.inc()
  ```
  As a result it reports near-zero latency (~0.02ms) regardless of real model inference + I/O time. This needs to wrap the full `/predict` handler before latency-driven cooldown tuning is meaningful.

- **`ml_skipped_cooldown_total` / `ml_circuit_open_total` counters are created but never registered with Prometheus.** They increment correctly in-process but won't appear on `/metrics`, since neither `NewMLWrapper` nor `NewMLWrapperWithConfig` calls `prometheus.Register` on them (registration currently only happens for the metrics on `Node` and `Cluster`, plus a manual loop in `main_cluster.go` that covers the ML policy metrics for policies returned by `createPolicyVariants` — worth double-checking this path actually reaches every constructed `MLPolicy` instance).

- **Weak ML model performance.** Current metrics: class-1 (evict) recall ≈ 0.15, F1 ≈ 0.22, with `freq` dominating feature importance at ≈ 0.44. The model is largely reinventing LFU rather than learning something LFU/LRU can't already capture. Root cause: no workload code currently calls `SetWithTTL`, so `ttl_remaining` has exactly 0.0 importance — one of five features is dead weight. Broader workload diversity (real TTL usage, more varied access distributions) is needed before the ML layer can meaningfully differentiate itself from the base policies it wraps.

- **`cache.go` uses package-level mutable variables (`evictKey`, `ok`) inside `evictToMakeSpace`,** rather than function-local variables. Since each node runs its own `*Cache` instance and `evictToMakeSpace` can execute concurrently across nodes, this is shared mutable package state being written from multiple goroutines without synchronization — a data race across node eviction loops, not just a style issue.

- **`metrics.go`'s `init()` still uses `prometheus.MustRegister`,** which panics on double-registration, unlike the safe-registration pattern (`prometheus.Register` + `AlreadyRegisteredError` check) used consistently elsewhere in `node.go`, `cluster.go`, and `main_cluster.go`.

### Resolved (kept here for history / portfolio narrative)

- `RLock`/`RUnlock` lock-leak in `cluster.go`'s `getNodeForKey`.
- Missing `keyOverrides` map causing silent cache misses for keys migrated via hot-key rebalancing.
- `MigrateHotKey` not returning the migrated key, breaking downstream override tracking.
- `ChooseEvictionDuringRecovery` not wired into WAL replay, meaning recovery could previously depend on a live ML service.
- `LFU.OnDelete` leaking empty frequency buckets over time.
- Double-eviction logic in the proactive ML trigger path.
- Malformed `set` WAL entries with empty values being replayed instead of rejected.
- Two divergent `CacheState` builder functions producing different feature schemas for training vs. serving — now unified through `buildCacheStateUnsafe`.
- `logAccess` call sites hardcoding `ttl_remaining=0.0` regardless of actual TTL.
- Feature name mismatch between training (`last_access_gap`) and the live `/predict` endpoint.
- Model filename mismatch (`eviction_model_v2.pkl` vs. `eviction_model.pkl`) silently causing model load failures.
- Missing `import time as _time` at module level in the ML service — every real prediction crashed with a `NameError`, which explained near-zero `ml_predictions_total` despite heavy eviction volume; predictions were silently falling back without visibility.
- CSV access-log flushing changed from synchronous per-call `Flush()` to a background 1-second ticker, removing a write-path bottleneck.

---

## Key Design Decisions & Lessons Learned

- **Silent failures are the recurring theme of this project.** The `NameError` crash on `_time`, the model filename mismatch, and the missing `keyOverrides` map all failed *silently* — the system kept running, just wrong. This is why the observability layer (Prometheus metrics on every path, not just the happy path) matters as much as the core logic.
- **Feature schema consistency between training and serving is fragile** and needs a single source of truth. Delegating both the async ML worker and the on-demand prediction path to one `buildCacheStateUnsafe()` function closed a class of bugs where training and serving could silently diverge.
- **A model with high `freq` importance and low class-1 recall is a signal**, not just a metric to shrug off — it means the ML layer isn't adding value beyond the policy it wraps. Workload design (does the access pattern actually exercise features like TTL?) matters as much as model architecture.
- **Prometheus metric placement must be validated**, not assumed — a `with metric.time():` block wrapping the wrong scope produces numbers that look plausible (small, stable) while being meaningless. Always check what's *inside* the context manager, not just that one exists.
- **Fallback-by-default is the right posture for an ML-augmented system.** Every ML failure mode (timeout, bad response, model not loaded, circuit open) degrades to the wrapped base policy rather than failing the request — the cache stays correct even when the ML service is completely down.

---

## Roadmap

- [ ] Fix `ml_prediction_duration_seconds` to wrap the full `/predict` handler body and validate real end-to-end latency
- [ ] Register `MetricCooldownSkipped` / `MetricCircuitOpenSkipped` for every constructed `MLPolicy` instance
- [ ] Fix the `evictKey`/`ok` package-global data race in `cache.go`
- [ ] Re-tune the 100ms ML cooldown once latency data is trustworthy
- [ ] Add workload diversity (TTL-bearing keys, varied access distributions) so `ttl_remaining` becomes a meaningful feature and the ML model can differentiate itself from LFU
- [ ] Re-train and re-evaluate the model once workload diversity improves class-1 recall
- [ ] Align `metrics.go` registration with the safe-registration pattern used elsewhere

---

## Tech Stack

**Go** (cache cluster, WAL, eviction policies, cluster coordination) · **Python / FastAPI** (ML microservice) · **scikit-learn** (RandomForestClassifier) · **BadgerDB** (embedded KV snapshot store) · **Prometheus** (metrics) · **Grafana** (dashboards) · **Docker Compose** (monitoring stack)
