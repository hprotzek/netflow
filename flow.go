package main

import (
	"net/netip"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type kind int

const (
	external kind = iota
	pod
	service
	node
)

// endpoint is what an IP address resolves to in the cluster.
type endpoint struct {
	kind      kind
	namespace string
	workload  string
	sidecar   bool // pod runs an istio-proxy container
}

// flow is one TCP socket as seen on this node.
type flow struct {
	local, remote netip.Addr
	lport, rport  uint16
	uid           uint32
	server        bool // the socket was accepted, not connected
}

// edge is one arrow in the service graph.
type edge struct {
	srcNS, src, dstNS, dst, port string
}

func (e edge) values() []string { return []string{e.srcNS, e.src, e.dstNS, e.dst, e.port} }

const istioProxyUID = 1337

// classify returns the graph edge a socket belongs to. ok is false when the
// socket must not be counted because another socket already carries the same
// traffic: every connection has two ends, and a sidecar adds two more legs.
func classify(f flow, lookup func(netip.Addr) endpoint) (e edge, ok bool) {
	if f.local.IsLoopback() || f.remote.IsLoopback() {
		return e, false // sidecar <-> app inside the pod
	}
	l, r := lookup(f.local), lookup(f.remote)
	if f.server {
		if r.kind != external {
			return e, false // in-cluster clients are counted by the agent on their node
		}
		return edge{r.namespace, r.workload, l.namespace, l.workload, strconv.Itoa(int(f.lport))}, true
	}
	// shortcut: assumes all outbound traffic of a sidecar pod goes through Envoy;
	// ports excluded from redirection (traffic.sidecar.istio.io/excludeOutbound*)
	// are dropped here. Upgrade by reading those pod annotations.
	if l.sidecar && f.uid != istioProxyUID {
		return e, false // app -> Envoy leg, redirected by iptables
	}
	return edge{l.namespace, l.workload, r.namespace, r.workload, strconv.Itoa(int(f.rport))}, true
}

// Label names match Istio's standard metrics so both join in PromQL.
var labels = []string{
	"source_workload_namespace", "source_workload",
	"destination_workload_namespace", "destination_workload", "destination_port",
}

var (
	sentBytes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "netflow_tcp_sent_bytes_total", Help: "TCP payload bytes from source to destination."}, labels)
	receivedBytes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "netflow_tcp_received_bytes_total", Help: "TCP payload bytes from destination back to source."}, labels)
	retransmits = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "netflow_tcp_retransmits_total", Help: "Segments retransmitted by the observed end of the connection."}, labels)
	opened = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "netflow_tcp_connections_opened_total", Help: "Established TCP connections."}, labels)
	failed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "netflow_tcp_connect_failures_total", Help: "Connection attempts that never got established."}, labels)
	openConns = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "netflow_tcp_connections_open", Help: "Currently open TCP connections."}, labels)
	rtt = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "netflow_tcp_rtt_seconds", Help: "Smoothed round-trip time, sampled per open connection.",
		Buckets: []float64{.0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1, .25, 1}}, labels)
	duration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "netflow_tcp_connection_duration_seconds", Help: "Lifetime of closed connections.",
		Buckets: []float64{.01, .1, 1, 10, 60, 600, 3600}}, labels)
)
