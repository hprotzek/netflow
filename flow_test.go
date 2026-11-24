package main

import (
	"net/netip"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestClassify(t *testing.T) {
	ips := map[string]endpoint{
		"10.0.0.1": {pod, "shop", "cart", true},
		"10.0.0.2": {pod, "shop", "db", false},
		"10.1.0.9": {service, "shop", "db-svc", false},
		"10.2.0.5": {node, "", "node", false},
	}
	lookup := func(ip netip.Addr) endpoint {
		if e, ok := ips[ip.String()]; ok {
			return e
		}
		return endpoint{workload: "external"}
	}
	a := netip.MustParseAddr
	tests := []struct {
		name string
		f    flow
		want edge
		ok   bool
	}{
		{"envoy outbound", flow{local: a("10.0.0.1"), remote: a("10.0.0.2"), rport: 5432, uid: 1337},
			edge{"shop", "cart", "shop", "db", "5432"}, true},
		{"app leg of sidecar pod", flow{local: a("10.0.0.1"), remote: a("10.1.0.9"), rport: 5432, uid: 1000}, edge{}, false},
		{"pod without sidecar", flow{local: a("10.0.0.2"), remote: a("8.8.8.8"), rport: 443, uid: 1000},
			edge{"shop", "db", "", "external", "443"}, true},
		{"server end, in-cluster client", flow{local: a("10.0.0.2"), remote: a("10.0.0.1"), lport: 5432, server: true}, edge{}, false},
		{"server end, kubelet probe", flow{local: a("10.0.0.2"), remote: a("10.2.0.5"), lport: 8080, server: true}, edge{}, false},
		{"server end, external client", flow{local: a("10.0.0.2"), remote: a("1.2.3.4"), lport: 5432, server: true},
			edge{"", "external", "shop", "db", "5432"}, true},
		{"envoy to app over loopback", flow{local: a("127.0.0.6"), remote: a("10.0.0.1"), rport: 8080, uid: 1337}, edge{}, false},
	}
	for _, tt := range tests {
		got, ok := classify(tt.f, lookup)
		if ok != tt.ok || got != tt.want {
			t.Errorf("%s: got %v %v, want %v %v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

func TestWorkloadName(t *testing.T) {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:            "cart-6d4f9c7b8-x2x9z",
		Labels:          map[string]string{"pod-template-hash": "6d4f9c7b8"},
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "cart-6d4f9c7b8"}},
	}}
	if got := workloadName(p); got != "cart" {
		t.Errorf("deployment pod: got %q", got)
	}
	p = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db-0",
		OwnerReferences: []metav1.OwnerReference{{Kind: "StatefulSet", Name: "db"}}}}
	if got := workloadName(p); got != "db" {
		t.Errorf("statefulset pod: got %q", got)
	}
	if got := workloadName(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bare"}}); got != "bare" {
		t.Errorf("bare pod: got %q", got)
	}
}
