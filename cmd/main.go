package main

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var requests = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "rate_limit_request",
	},
	[]string{"request", "status"},
)

func main() {
	prometheus.MustRegister(requests)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Hello, World!"))
	})

	http.Handle("/metrics", promhttp.Handler())
	http.ListenAndServe(":8080", nil)
}
