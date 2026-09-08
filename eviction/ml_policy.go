package eviction

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// MLPolicy wraps a base eviction policy and tries ML prediction first
type MLPolicy struct {
	base           EvictionPolicy
	mlEndpoint     string
	httpClient     *http.Client
	enabled        bool
	lastCall       time.Time                   // Add cooldown protection
	onMLEviction   func(key string, isML bool) // Add this callback
	onFallback     func(key string)
	stateProvider  func() CacheState
	consecutiveErr int
	circuitOpenTil time.Time

	MetricCooldownSkipped    prometheus.Counter
	MetricCircuitOpenSkipped prometheus.Counter
}

func (m *MLPolicy) SetStateProvider(f func() CacheState) {
	m.stateProvider = f
}

type CacheState struct {
	Keys     []string               `json:"keys"`
	Features map[string]interface{} `json:"features"`
}

// --------------------
// Constructors
// --------------------
func NewMLWrapper(base EvictionPolicy) *MLPolicy {
	return &MLPolicy{
		base:       base,
		mlEndpoint: "http://localhost:8000/predict",
		httpClient: &http.Client{Timeout: 2 * time.Second},
		enabled:    true,
		MetricCooldownSkipped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ml_skipped_cooldown_total",
			Help: "Times ML prediction was skipped due to cooldown",
		}),
		MetricCircuitOpenSkipped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ml_circuit_open_total",
			Help: "Times ML prediction was skipped due to open circuit breaker",
		}),
	}
}

func NewMLWrapperWithConfig(base EvictionPolicy, endpoint string, timeout time.Duration, enabled bool, policyLabel string) *MLPolicy {
	return &MLPolicy{
		base:       base,
		mlEndpoint: endpoint,
		httpClient: &http.Client{Timeout: timeout},
		enabled:    enabled,
		MetricCooldownSkipped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: fmt.Sprintf("ml_skipped_cooldown_total_%s", policyLabel),
			Help: "Times ML prediction was skipped due to cooldown",
		}),
		MetricCircuitOpenSkipped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: fmt.Sprintf("ml_skipped_open_total_%s", policyLabel),
			Help: "Times ML prediction was skipped due to open circuit breaker",
		}),
	}
}

// --------------------
// Policy Hooks
// --------------------
func (m *MLPolicy) OnGet(key string)    { m.base.OnGet(key) }
func (m *MLPolicy) OnSet(key string)    { m.base.OnSet(key) }
func (m *MLPolicy) OnDelete(key string) { m.base.OnDelete(key) }

// --------------------
// ML Prediction - DEBUG VERSION
// --------------------

func (m *MLPolicy) SetEvictionCallbacks(onML func(string, bool), onFallback func(string)) {
	m.onMLEviction = onML
	m.onFallback = onFallback
}

// Update GetMLPrediction to call callbacks

// Update ChooseEviction to call fallback callback

func (m *MLPolicy) GetMLPrediction(state CacheState) (string, bool) {
	if !m.enabled || len(state.Keys) == 0 {
		fmt.Printf("[MLPolicy] ML disabled or no keys: enabled=%t, keys=%d\n", m.enabled, len(state.Keys))
		return "", false
	}

	if time.Now().Before(m.circuitOpenTil) {
		m.MetricCircuitOpenSkipped.Inc()
		return "", false
	}

	// Add cooldown protection to prevent rapid successive calls
	now := time.Now()
	if now.Sub(m.lastCall) < 100*time.Millisecond {
		fmt.Printf("[MLPolicy] Cooldown active, skipping ML prediction\n")
		m.MetricCooldownSkipped.Inc()
		return "", false
	}
	m.lastCall = now

	fmt.Printf("[MLPolicy] Sending ML request with %d keys\n", len(state.Keys))

	// Log a sample of features for debugging
	if len(state.Keys) > 0 {
		sampleKey := state.Keys[0]
		if features, ok := state.Features[sampleKey]; ok {
			fmt.Printf("[MLPolicy] Sample features for %s: %+v\n", sampleKey, features)
		}
	}

	body, err := json.Marshal(state)
	if err != nil {
		fmt.Printf("[MLPolicy] JSON marshal error: %v\n", err)
		return "", false
	}

	// DEBUG: Log request details
	fmt.Printf("[MLPolicy DEBUG] Making request to: %s\n", m.mlEndpoint)
	fmt.Printf("[MLPolicy DEBUG] Request body: %s\n", string(body))

	ctx, cancel := context.WithTimeout(context.Background(), m.httpClient.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", m.mlEndpoint, bytes.NewBuffer(body))
	if err != nil {
		fmt.Printf("[MLPolicy] Request creation error: %v\n", err)
		return "", false
	}
	req.Header.Set("Content-Type", "application/json")

	fmt.Printf("[MLPolicy DEBUG] About to make HTTP request...\n")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		fmt.Printf("[MLPolicy] HTTP request error: %v\n", err)
		m.recordFailure()
		return "", false
	}
	defer resp.Body.Close()

	fmt.Printf("[MLPolicy DEBUG] Received response with status: %d\n", resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("[MLPolicy] HTTP error status: %d\n", resp.StatusCode)
		m.recordFailure()
		return "", false
	}

	var result struct {
		EvictKey string `json:"evict_key"`
		Fallback bool   `json:"fallback,omitempty"`
		Error    string `json:"error,omitempty"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Printf("[MLPolicy] Response decode error: %v\n", err)
		return "", false
	}

	fmt.Printf("[MLPolicy DEBUG] Decoded response: evict_key=%s, fallback=%t, error=%s\n",
		result.EvictKey, result.Fallback, result.Error)

	if result.Error != "" {
		fmt.Printf("[MLPolicy] ML service error: %s\n", result.Error)
		return "", false
	}

	if result.EvictKey != "" {
		fmt.Printf("[MLPolicy] ML prediction: %s (fallback: %t)\n", result.EvictKey, result.Fallback)
		m.consecutiveErr = 0
		// ✅ CALL ML EVICTION CALLBACK
		if m.onMLEviction != nil {
			m.onMLEviction(result.EvictKey, true) // Track as ML eviction
		}

		return result.EvictKey, true
	}

	fmt.Printf("[MLPolicy DEBUG] No evict_key in response, falling back\n")
	return "", false
}

func (m *MLPolicy) recordFailure() {
	m.consecutiveErr++
	if m.consecutiveErr >= 3 {
		m.circuitOpenTil = time.Now().Add(10 * time.Second)
		m.consecutiveErr = 0
		fmt.Printf("[MLPolicy] Circuit opened for 10s after repeated failures\n")
	}
}

// --------------------
// Fallback
// --------------------
func (m *MLPolicy) ChooseEviction() (string, bool) {
	fmt.Printf("[MLPolicy.ChooseEviction] enabled=%t stateProvider=%v\n", m.enabled, m.stateProvider != nil)
	if m.enabled && m.stateProvider != nil {
		if key, ok := m.GetMLPrediction(m.stateProvider()); ok {
			return key, true
		}
	}
	key, ok := m.base.ChooseEviction()
	if ok && m.onFallback != nil {
		fmt.Printf("Evicting key: %s (via %s fallback)\n", key, m.getBasePolicyName())
		m.onFallback(key)

	}
	return key, ok
}

func (m *MLPolicy) ChooseEvictionDuringRecovery() (string, bool) {
	// Skip ML during recovery
	return m.base.ChooseEviction()
}

// --------------------
// Utilities
// --------------------
func (m *MLPolicy) getBasePolicyName() string {
	switch m.base.(type) {
	case *LRU:
		return "LRU"
	case *LFU:
		return "LFU"
	default:
		return "base policy"
	}
}

func (m *MLPolicy) SetEnabled(enabled bool) { m.enabled = enabled }
func (m *MLPolicy) IsEnabled() bool         { return m.enabled }
func (m *MLPolicy) BasePolicy() EvictionPolicy {
	return m.base
}
