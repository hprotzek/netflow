# netflow

An eBPF agent for Kubernetes that reports who talks to whom over TCP, how much, and how healthy the connection is. It runs as a DaemonSet and exposes Prometheus metrics that are shaped for drawing a service graph and for joining with Istio's metrics.

Built for GKE Standard with Dataplane V2 and Istio sidecars. Design details are in [docs/SPEC.md](docs/SPEC.md).

**Status:** the probes are verified on a local kernel. The agent has not yet run on GKE; see the open questions in the spec.

## What you get

For each edge (source workload → destination workload and port):

- bytes in both directions
- connections opened, currently open, and failed connects
- TCP retransmits and round-trip time
- connection lifetime

It sees all TCP traffic on the node, including pods without a sidecar, system namespaces and traffic leaving the cluster. It does not see HTTP requests, UDP or DNS.

## Metrics

All metrics carry the same labels, named like Istio's:

`source_workload_namespace`, `source_workload`, `destination_workload_namespace`, `destination_workload`, `destination_port`

| Metric | Type | Meaning |
|---|---|---|
| `netflow_tcp_sent_bytes_total` | counter | Payload bytes from source to destination |
| `netflow_tcp_received_bytes_total` | counter | Payload bytes from destination back to source |
| `netflow_tcp_retransmits_total` | counter | Segments retransmitted by the observed end |
| `netflow_tcp_connections_opened_total` | counter | Established connections |
| `netflow_tcp_connect_failures_total` | counter | Connects that were refused or timed out |
| `netflow_tcp_connections_open` | gauge | Connections open at the last sample |
| `netflow_tcp_rtt_seconds` | histogram | Smoothed round-trip time, sampled per open connection |
| `netflow_tcp_connection_duration_seconds` | histogram | Lifetime of closed connections |

Workloads that are not pods appear as `external` (outside the cluster), `node` (the host or a host-network pod), or a Service name. Their namespace label is empty for `external` and `node`.

## Build

Requires Docker. The eBPF probe is compiled inside the image build.

```bash
docker build -t REGISTRY/netflow:0.1.0 .
```

```bash
docker push REGISTRY/netflow:0.1.0
```

Build on the architecture of your nodes, or use `docker buildx build --platform linux/amd64,linux/arm64`.

## Deploy

Set the image in [deploy/netflow.yaml](deploy/netflow.yaml), then:

```bash
kubectl apply -f deploy/netflow.yaml
```

```bash
kubectl -n netflow rollout status daemonset/netflow
```

This creates the `netflow` namespace, a service account with read access to pods and services, and the DaemonSet. The pod runs with the `BPF` and `PERFMON` capabilities only.

Check one agent:

```bash
kubectl -n netflow port-forward daemonset/netflow 9090
```

```bash
curl -s localhost:9090/metrics | grep '^netflow_'
```

### Scraping

Each pod serves `/metrics` on port 9090 and carries the `prometheus.io/scrape` and `prometheus.io/port` annotations. If your OpenTelemetry collector does not use annotation discovery, add a scrape job for pods with the label `app=netflow`.

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | `:9090` | Address for `/metrics` |
| `-interval` | `15s` | How often open connections are sampled. Keep it at or below the scrape interval. |
| `-no-k8s` | off | Skip the Kubernetes lookup; every address becomes `external`. For testing probes on a dev machine. |

## Queries

These are starting points; they have not been run against real cluster data yet.

Throughput per edge, summed over nodes and ports:

```promql
sum by (source_workload_namespace, source_workload, destination_workload_namespace, destination_workload) (
  rate(netflow_tcp_sent_bytes_total[5m])
)
```

Retransmits per megabyte sent, as a loss indicator:

```promql
sum by (source_workload, destination_workload) (rate(netflow_tcp_retransmits_total[5m]))
/
sum by (source_workload, destination_workload) (rate(netflow_tcp_sent_bytes_total[5m])) * 1e6
```

99th percentile round-trip time per edge:

```promql
histogram_quantile(0.99, sum by (le, source_workload, destination_workload) (rate(netflow_tcp_rtt_seconds_bucket[5m])))
```

Istio request rate next to network round-trip time on the same edge:

```promql
sum by (source_workload, destination_workload) (rate(istio_requests_total{reporter="source"}[5m]))
and on (source_workload, destination_workload)
histogram_quantile(0.5, sum by (le, source_workload, destination_workload) (rate(netflow_tcp_rtt_seconds_bucket[5m])))
```

Edges the agent sees that Istio does not report:

```promql
sum by (source_workload, destination_workload) (rate(netflow_tcp_sent_bytes_total[5m]))
unless on (source_workload, destination_workload)
sum by (source_workload, destination_workload) (rate(istio_tcp_sent_bytes_total[5m]) or rate(istio_requests_total[5m]))
```

For a graph, Grafana's Node Graph panel takes one query for nodes and one for edges; the first query above is the edge list.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Pod fails with `load:` or `attach` errors | Kernel older than 5.11 or without BTF, or a capability was removed from the manifest |
| Pod fails with `kubernetes:` | RBAC missing, or the API server is unreachable from the pod |
| Everything is `external` | The agent runs with `-no-k8s`, or the pod informer has no permissions |
| A meshed workload shows no outbound traffic | Its traffic uses ports excluded from Istio redirection (see limits in the spec) |
| Edges point at Service names | Dataplane V2 leaves the Service IP on the socket (see limits in the spec) |

## Overhead

There are no hooks on the packet path: one tracepoint fires on TCP state changes, and one pass over the open sockets runs per interval. To measure on a node:

```bash
sysctl kernel.bpf_stats_enabled=1
```

```bash
bpftool prog show
```

`run_time_ns / run_cnt` is the average cost per call. Switch the sysctl off again afterwards.

## Development

See [AGENTS.md](AGENTS.md) for the build loop, the smoke test and the invariants.
