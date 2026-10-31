package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type Metrics struct {
	Registry *prometheus.Registry

	HTTPRequests *prometheus.CounterVec   // route, method, code
	HTTPDuration *prometheus.HistogramVec // route, method
	RateLimited  *prometheus.CounterVec   // limiter: "request" | "create"

	VMCreates        *prometheus.CounterVec // result: "ok" | "quota" | "error"
	VMCreateDuration prometheus.Histogram
	VMRemovals       *prometheus.CounterVec // reason (from dueReason)
}

func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	f := promauto.With(reg)
	return &Metrics{
		Registry: reg,
		HTTPRequests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "playground_http_requests_total"}, []string{"route", "method", "code"}),
		HTTPDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "playground_http_request_duration_seconds", Buckets: prometheus.DefBuckets},
			[]string{"route", "method"}),
		RateLimited: f.NewCounterVec(prometheus.CounterOpts{
			Name: "playground_rate_limited_total"}, []string{"limiter"}),
		VMCreates: f.NewCounterVec(prometheus.CounterOpts{
			Name: "playground_vm_creates_total"}, []string{"result"}),
		VMCreateDuration: f.NewHistogram(prometheus.HistogramOpts{
			Name:    "playground_vm_create_duration_seconds",
			Buckets: []float64{.1, .25, .5, 1, 2, 5, 10, 30}}),
		VMRemovals: f.NewCounterVec(prometheus.CounterOpts{
			Name: "playground_vm_removals_total"}, []string{"reason"}),
	}
}
