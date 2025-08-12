package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sumanth-kadarla/ml-cache/pkg/store"
	"github.com/sumanth-kadarla/ml-cache/pkg/wal"
)

var (
	cacheHits = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cache_hits_total",
		Help: "Total number of cache hits",
	})
	cacheMisses = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cache_misses_total",
		Help: "Total number of cache misses",
	})
	requestLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "cache_request_latency_seconds",
		Help:    "Latency of cache GET requests",
		Buckets: prometheus.DefBuckets,
	})
)

func init() {
	prometheus.MustRegister(cacheHits, cacheMisses, requestLatency)
}

func main() {
	// Init WAL
	w, err := wal.NewWAL("./data/wal.log")
	if err != nil {
		log.Fatalf("wal init: %v", err)
	}
	defer w.Close()

	// Init in-memory store
	st := store.NewInMemStore(1024, w)

	mux := http.NewServeMux()

	// Prometheus metrics endpoint
	mux.Handle("/metrics", promhttp.Handler())

	// GET endpoint
	mux.HandleFunc("/v1/get", func(wr http.ResponseWriter, r *http.Request) {
		start := time.Now()
		k := r.URL.Query().Get("key")
		if k == "" {
			wr.WriteHeader(http.StatusBadRequest)
			wr.Write([]byte("missing key"))
			return
		}
		v, ok := st.Get(k)
		if !ok {
			cacheMisses.Inc()
			wr.WriteHeader(http.StatusNotFound)
			wr.Write([]byte(""))
			return
		}
		cacheHits.Inc()
		requestLatency.Observe(time.Since(start).Seconds())
		wr.Write([]byte(v))
	})

	// SET endpoint
	mux.HandleFunc("/v1/set", func(wr http.ResponseWriter, r *http.Request) {
		var req struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			wr.WriteHeader(http.StatusBadRequest)
			return
		}
		st.Set(req.Key, req.Value)
		wr.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr: "0.0.0.0:8080",
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	// Start server
	go func() {
		log.Println("server listening on :8080")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	// Graceful shutdown
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
	log.Println("shutting down")
	srv.Close()
}
