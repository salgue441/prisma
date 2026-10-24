// Package telemetry holds Prisma's Prometheus metrics and access logging.
package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics uses route and cluster names as labels, never raw paths, consumers
// or client IPs, to keep cardinality bounded.
type Metrics struct {
	Registry *prometheus.Registry

	Requests         *prometheus.CounterVec
	Duration         *prometheus.HistogramVec
	UpstreamRequests *prometheus.CounterVec
	UpstreamDuration *prometheus.HistogramVec
	Retries          *prometheus.CounterVec
	RateLimit        *prometheus.CounterVec
	AuthFailures     *prometheus.CounterVec
	MaskingFailures  *prometheus.CounterVec
	EndpointHealthy  *prometheus.GaugeVec
	CircuitOpen      *prometheus.GaugeVec
	ConfigReloads    *prometheus.CounterVec
	ConfigInfo       *prometheus.GaugeVec
}

func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	buckets := []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}
	m := &Metrics{
		Registry: reg,
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "prisma_requests_total", Help: "Requests handled by the gateway.",
		}, []string{"listener", "route", "protocol", "code", "grpc_status"}),
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "prisma_request_duration_seconds", Help: "End-to-end request duration.", Buckets: buckets,
		}, []string{"route", "protocol"}),
		UpstreamRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "prisma_upstream_requests_total", Help: "Upstream attempts by outcome.",
		}, []string{"cluster", "endpoint", "outcome"}),
		UpstreamDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "prisma_upstream_duration_seconds", Help: "Time to upstream response headers.", Buckets: buckets,
		}, []string{"cluster"}),
		Retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "prisma_upstream_retries_total", Help: "Upstream retry attempts.",
		}, []string{"cluster"}),
		RateLimit: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "prisma_ratelimit_decisions_total", Help: "Rate limit decisions.",
		}, []string{"route", "limit", "result"}),
		AuthFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "prisma_auth_failures_total", Help: "Authentication and authorization failures.",
		}, []string{"route", "reason"}),
		MaskingFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "prisma_masking_failures_total", Help: "Responses that could not be masked.",
		}, []string{"route", "action"}),
		EndpointHealthy: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "prisma_endpoint_healthy", Help: "1 if the endpoint passes active health checks.",
		}, []string{"cluster", "endpoint"}),
		CircuitOpen: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "prisma_circuit_open", Help: "1 while the cluster circuit breaker is open.",
		}, []string{"cluster"}),
		ConfigReloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "prisma_config_reloads_total", Help: "Configuration reload attempts.",
		}, []string{"result"}),
		ConfigInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "prisma_config_info", Help: "Active configuration version.",
		}, []string{"version"}),
	}
	reg.MustRegister(m.Requests, m.Duration, m.UpstreamRequests, m.UpstreamDuration, m.Retries,
		m.RateLimit, m.AuthFailures, m.MaskingFailures, m.EndpointHealthy, m.CircuitOpen,
		m.ConfigReloads, m.ConfigInfo)
	return m
}
