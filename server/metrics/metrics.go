// Package metrics exposes Prometheus metrics for quicvoiced.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	Connections = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "quicvoice_connections",
		Help: "Open QUIC connections.",
	})
	Rooms = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "quicvoice_rooms",
		Help: "Rooms with at least one participant.",
	})
	Participants = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "quicvoice_participants",
		Help: "Participants across all rooms.",
	})
	DatagramsIn = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "quicvoice_datagrams_in_total",
		Help: "Datagrams received, by kind.",
	}, []string{"kind"})
	DatagramsOut = promauto.NewCounter(prometheus.CounterOpts{
		Name: "quicvoice_datagrams_out_total",
		Help: "Audio datagrams forwarded to receivers.",
	})
	Dropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "quicvoice_datagrams_dropped_total",
		Help: "Datagrams dropped, by reason.",
	}, []string{"reason"})
	AuthFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "quicvoice_auth_failures_total",
		Help: "Rejected Hello messages, by reason.",
	}, []string{"reason"})
)
