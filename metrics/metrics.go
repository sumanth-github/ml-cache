package metrics

import "github.com/prometheus/client_golang/prometheus"

var CacheEvictions = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "cache_evictions_total",
	Help: "Total number of evicted keys",
})

func init() {
	prometheus.MustRegister(CacheEvictions)
}
