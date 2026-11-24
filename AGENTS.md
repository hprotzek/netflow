# netflow

eBPF agent that turns TCP sockets on a Kubernetes node into service-graph metrics. Design and counting rules: [docs/SPEC.md](docs/SPEC.md). Read it before changing which sockets are counted, the metric names, or the labels.

## Build and test loop

The probe compiles only on Linux, and the generated `bpf_bpfel.go` / `.o` files are build output, kept out of the repo. Everything goes through Docker:

```bash
docker build -t netflow:dev .
```

That runs `go generate`, `go vet`, `go test` and the build. On macOS, plain `go test ./...` covers only the kernel-independent logic (`flow.go`, `k8s.go`); `main.go` is Linux-only.

A change to `bpf/flow.c` or `main.go` is done when the smoke test shows the expected series. Run it with exactly the capabilities the manifest grants, so a missing capability fails here and not on the cluster:

```bash
docker run -d --name netflow-test --cap-drop ALL --cap-add BPF --cap-add PERFMON -p 19090:9090 netflow:dev -no-k8s -interval 3s
```

```bash
curl -s localhost:19090/metrics | grep '^netflow_'
```

Generate traffic from a second container; probes are kernel-global, so the agent sees it. `-no-k8s` labels every address `external`. Remove the container afterwards.

## Invariants

- **One place decides what is counted:** `classify` in `flow.go`. Every rule there has a row in `TestClassify`; add the row with the rule.
- **`struct rec` in `bpf/flow.c` has explicit padding and matches the `kind*` / `role*` constants in `main.go`.** Change both together; the verifier rejects records with uninitialised padding.
- **Label names are Istio's** (`source_workload`, `destination_workload`, ...). The Grafana joins depend on them.
- **Hooks stay at socket level** (tracepoints, iterators). The tc and XDP hooks belong to GKE's managed Cilium (`anetd`).
- **Per-socket values that the kernel clears at close are saved at open** in `struct conn` (the local port is the known case).
- **Kernel structs are declared minimally in `flow.c`** with `preserve_access_index`; add a field there rather than pulling in `vmlinux.h`.
- **A known limit in code is marked** `shortcut: <limit>, <when to upgrade>`. Grep for `shortcut:` to list them.
