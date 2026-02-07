package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	registry *prometheus.Registry

	requestsTotal *prometheus.CounterVec
	latencyMs     *prometheus.HistogramVec
}

func New() *Metrics {
	r := prometheus.NewRegistry()
	m := &Metrics{
		registry: r,
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "claude_gateway_requests_total",
			Help: "Total number of requests processed by the gateway.",
		}, []string{"facade", "provider", "status"}),
		latencyMs: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "claude_gateway_request_latency_ms",
			Help:    "Request latency in milliseconds.",
			Buckets: []float64{5, 10, 25, 50, 100, 250, 500, 1000, 2000, 5000, 10000, 30000},
		}, []string{"facade", "provider", "status"}),
	}
	r.MustRegister(m.requestsTotal, m.latencyMs)
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *Metrics) ObserveRequest(facade, provider string, status int, dur time.Duration) {
	s := strconv.Itoa(status)
	m.requestsTotal.WithLabelValues(facade, provider, s).Inc()
	m.latencyMs.WithLabelValues(facade, provider, s).Observe(float64(dur.Milliseconds()))
}
