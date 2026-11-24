//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"net/netip"
	"os/signal"
	"syscall"
	"time"

	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

//go:generate go tool bpf2go -target bpfel -type rec bpf bpf/flow.c

// Mirror the enums in bpf/flow.c.
const (
	kindTick = iota
	kindOpen
	kindClose
	kindFailed
)
const roleServer = 1

func main() {
	addr := flag.String("listen", ":9090", "address for /metrics")
	interval := flag.Duration("interval", 15*time.Second, "how often open connections are sampled")
	noK8s := flag.Bool("no-k8s", false, "resolve nothing; for testing the probes outside a cluster")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	lookup := func(netip.Addr) endpoint { return endpoint{workload: "external"} }
	if !*noK8s {
		var err error
		if lookup, err = newLookup(ctx); err != nil {
			log.Fatalf("kubernetes: %v", err)
		}
	}

	_ = rlimit.RemoveMemlock() // only needed on kernels before 5.11
	var objs bpfObjects
	if err := loadBpfObjects(&objs, nil); err != nil {
		log.Fatalf("load: %v", err)
	}
	defer objs.Close()
	tp, err := link.AttachTracing(link.TracingOptions{Program: objs.SetState})
	if err != nil {
		log.Fatalf("attach tracepoint: %v", err)
	}
	defer tp.Close()
	iter, err := link.AttachIter(link.IterOptions{Program: objs.Dump, Map: objs.Conns})
	if err != nil {
		log.Fatalf("attach iterator: %v", err)
	}
	defer iter.Close()
	rd, err := ringbuf.NewReader(objs.Events)
	if err != nil {
		log.Fatalf("ringbuf: %v", err)
	}

	recs := make(chan bpfRec, 4096)
	go func() {
		for {
			raw, err := rd.Read()
			if errors.Is(err, ringbuf.ErrClosed) {
				return
			}
			if r, ok := decode(raw.RawSample); err == nil && ok {
				recs <- r
			}
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	go func() { log.Fatal(http.ListenAndServe(*addr, nil)) }()
	log.Printf("netflow running, metrics on %s", *addr)

	open := map[edge]float64{}
	tick := time.NewTicker(*interval)
	for {
		select {
		case <-ctx.Done():
			rd.Close()
			return
		case r := <-recs:
			handle(&r, lookup, open)
		case <-tick.C:
			for e := range open {
				open[e] = 0
			}
			f, err := iter.Open()
			if err != nil {
				log.Printf("iterator: %v", err)
				continue
			}
			data, err := io.ReadAll(f)
			f.Close()
			if err != nil {
				log.Printf("iterator read: %v", err)
			}
			for n := binary.Size(bpfRec{}); len(data) >= n; data = data[n:] {
				if r, ok := decode(data[:n]); ok {
					handle(&r, lookup, open)
				}
			}
			for e, n := range open {
				openConns.WithLabelValues(e.values()...).Set(n)
			}
		}
	}
}

func decode(b []byte) (r bpfRec, ok bool) {
	return r, binary.Read(bytes.NewReader(b), binary.NativeEndian, &r) == nil
}

func handle(r *bpfRec, lookup func(netip.Addr) endpoint, open map[edge]float64) {
	server := r.Role == roleServer
	e, ok := classify(flow{
		local:  netip.AddrFrom16(r.Saddr).Unmap(),
		remote: netip.AddrFrom16(r.Daddr).Unmap(),
		lport:  r.Sport, rport: r.Dport, uid: r.Uid, server: server,
	}, lookup)
	if !ok {
		return
	}
	l := e.values()
	sent, recv := r.Sent, r.Recv
	if server {
		sent, recv = recv, sent // metrics are source -> destination, the socket is the destination
	}
	sentBytes.WithLabelValues(l...).Add(float64(sent))
	receivedBytes.WithLabelValues(l...).Add(float64(recv))
	retransmits.WithLabelValues(l...).Add(float64(r.Retrans))

	switch r.Kind {
	case kindOpen:
		opened.WithLabelValues(l...).Inc()
		return
	case kindFailed:
		failed.WithLabelValues(l...).Inc()
		return
	case kindClose:
		duration.WithLabelValues(l...).Observe(time.Duration(r.DurNs).Seconds())
	case kindTick:
		open[e]++
	}
	if r.SrttUs > 0 {
		rtt.WithLabelValues(l...).Observe(float64(r.SrttUs) / 1e6)
	}
}
