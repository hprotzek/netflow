# TODO

Ordered by priority. Background for each item is in [docs/SPEC.md](docs/SPEC.md) under "Known limits" and "Open questions".

## 1. Validate on GKE

- [ ] Push the image to the registry and set it in `deploy/netflow.yaml`
- [ ] Deploy to a non-production cluster or a single node pool
- [ ] Check which destination address Dataplane V2 leaves on the socket (pod IP or Service IP)
- [ ] Compare edges for meshed workloads with Istio's own metrics (sidecar rule, user ID 1337)
- [ ] Cross-check edges against `hubble observe`
- [ ] Confirm the OTel collector scrapes the agent and series arrive in Mimir
- [ ] Measure cost per node with `kernel.bpf_stats_enabled=1` and `bpftool prog show`
- [ ] Measure agent memory with the real pod and Service cache
- [ ] Import the dashboard and check the queries against real data
- [ ] Update the spec's open questions and the README's overhead section with the results

## 2. Fix known gaps

- [ ] Resolve Service IPs to workloads through EndpointSlices (only if the GKE run shows Service names as destinations)
- [ ] Named external destinations from a configurable CIDR list
- [ ] Track connections that were open before the agent started
- [ ] Count traffic on ports excluded from Istio redirection

## 3. Make it operable

- [ ] Self-metrics: ring-buffer drops, unattributed records, lookup cache size
- [ ] Readiness probe
- [ ] Helm chart or Kustomize base
- [ ] CI: build for amd64 and arm64, run tests
- [ ] Check the series count in Mimir; decide whether `destination_port` stays a label
- [ ] Choose a license
- [ ] Fix the SSH remote or switch `origin` to HTTPS

## 4. Extend what it sees

- [ ] Source and destination zone labels, for cross-zone traffic cost
- [ ] DNS probe: queries per pod, and hostnames for external destinations
- [ ] Packet drops from the kernel's drop tracepoint
- [ ] UDP flows

## 5. Only if a consumer asks

- [ ] Per-connection records to Pub/Sub and BigQuery
- [ ] Live event stream of opens, closes and failed connects
