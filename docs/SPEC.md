# netflow specification

Status: v1 built and verified on a local kernel (2026-11-24). Not yet run on GKE.

## Purpose

Give every consumer a queryable service graph of the cluster with network-level numbers on each edge, including traffic that Istio does not see.

Target environment: GKE Standard 1.35, Dataplane V2 (managed Cilium), open-source Istio in sidecar mode, metrics stored in Mimir behind an OpenTelemetry collector.

## Goals

1. One edge per (source workload, destination workload, destination port), with bytes in both directions, connection counts, failed connects, retransmits, round-trip time and connection duration.
2. Cover meshed and non-meshed traffic, without counting any connection twice.
3. Join with Istio's metrics in PromQL on the same edge.
4. Data reachable over a standard API (PromQL), not from logs.
5. Stay well under 1% of a node's CPU and memory.

## Non-goals

- HTTP or gRPC request metrics. Istio provides them.
- Policy verdicts and drops. Hubble provides them.
- Per-connection records. Only aggregates are exported.
- UDP and DNS.
- Enforcement of any kind.

## Architecture

```
kernel                                   user space (one pod per node)
------                                   -----------------------------
tp_btf/inet_sock_set_state --ringbuf-->  decode -> classify -> Prometheus counters
  open / close / failed events                        ^
                                                      | IP -> workload
sk_storage map (state per socket)         pod + service informers
        ^
        | every 15 s
iter/bpf_sk_storage_map ----seq file-->   one record per open socket
```

### Kernel side (`bpf/flow.c`)

- **State:** an `SK_STORAGE` map holds, per established TCP socket, the start time, role, local port, and the counters as of the last report. The kernel frees the entry with the socket.
- **Tracepoint `inet_sock_set_state`:**
  - to `ESTABLISHED`: create state, emit `OPEN`. Role is server if the previous state was `SYN_RECV`, otherwise client.
  - to `CLOSE` with state: emit `CLOSE` with the final deltas and the duration, then delete the state.
  - `SYN_SENT` to `CLOSE` without state: emit `FAILED`.
- **Iterator over the storage map:** run by user space every interval; emits `TICK` with deltas for each open socket and updates the stored counters.
- **Counters** come from the kernel's own `tcp_sock` fields (`bytes_sent`, `bytes_received`, `total_retrans`, `srtt_us`). Nothing is hooked on the packet path.

Every record carries deltas since the previous record of the same socket, so user space only adds.

Why sample open sockets: Envoy pools connections and keeps them open for hours. Reporting only at close would hide most mesh traffic.

### User space

- `main.go`: loads and attaches the programs, reads the ring buffer, runs the iterator on a ticker, serves `/metrics`.
- `k8s.go`: cluster-wide pod and service informers, indexed by IP. Pods are trimmed to the fields the lookup needs.
- `flow.go`: the counting rules (`classify`) and the metric definitions.

## Address resolution

An IP resolves to the first match:

| Order | Match | Result |
|---|---|---|
| 1 | IP of a running pod that is not on the host network | pod: namespace and workload |
| 2 | Cluster IP of a Service | service: namespace and Service name |
| 3 | Host IP of any pod | `node` |
| 4 | anything else | `external` |

Workload name: the owner's name; for a ReplicaSet owner the `pod-template-hash` suffix is removed, giving the Deployment name. Pods without an owner use the pod name. This matches Istio's `source_workload` / `destination_workload`.

## Counting rules

One logical connection appears as several sockets: two ends, and with sidecars two more legs inside each pod. Exactly one socket is counted.

| Socket | Counted | Reason |
|---|---|---|
| Either address is loopback | no | sidecar-to-app leg inside the pod |
| Client socket, local pod has `istio-proxy`, user ID is not 1337 | no | app-to-Envoy leg, redirected by iptables |
| Any other client socket | yes, as local → remote | the real connection on the wire |
| Server socket, remote resolves to `external` | yes, as remote → local | no agent sees the client end |
| Any other server socket | no | counted by the agent on the client's node |

For a counted server socket the byte counters are swapped, so `sent` always means source to destination.

## Metrics contract

The metric names, types and labels are listed in the [README](../README.md#metrics). They are a contract with dashboards and other consumers: renaming one is a breaking change.

Each agent exports its own series. Consumers aggregate across nodes with `sum by (...)`.

## Runtime requirements

- Kernel 5.11 or newer with BTF (tracing programs with socket storage, storage-map iterators, ring buffer). GKE's Container-Optimized OS meets this.
- Capabilities `BPF` and `PERFMON`. No host network, host PID or privileged mode.
- RBAC: `list` and `watch` on pods and services, cluster-wide.

## Known limits

| Limit | Effect | Upgrade path |
|---|---|---|
| Ports excluded from Istio redirection | That traffic from sidecar pods is not counted | Read the `traffic.sidecar.istio.io/excludeOutbound*` annotations in `classify` |
| Service IP left on the socket | Edge points at the Service name, not the workload | Resolve through EndpointSlices |
| All outside addresses are `external` | One node for databases, SaaS and internet alike | Configurable CIDR-to-name list |
| Host-network pods | Shown as `node`, not as their workload | Attribute by cgroup instead of IP |
| Sockets open before the agent starts | Invisible until they close and reopen | Seed state from a TCP socket iterator at start |
| Agent restart | Bytes since the last tick are lost; counters restart at zero | Acceptable for rate queries |
| Destination port as a label | High series count if clients target many ports | Drop or bucket the label |
| Ambient mesh | Sidecar rules do not apply | Separate design |

## Open questions

To be answered on a real GKE node:

1. Does Dataplane V2 put the backend pod IP or the Service IP on the socket of a non-meshed pod?
2. Does Dataplane V2's socket-level load balancing bypass sidecar redirection, and if so, do the uid-1337 rule and Istio's own metrics still agree?
3. Does the collector discover the agent through the `prometheus.io/scrape` annotations?
4. Measured cost per node: `kernel.bpf_stats_enabled=1`, then `bpftool prog show`.
