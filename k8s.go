package main

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// workloadName derives the Deployment/StatefulSet/DaemonSet name the way Istio does.
func workloadName(p *corev1.Pod) string {
	if len(p.OwnerReferences) == 0 {
		return p.Name
	}
	o := p.OwnerReferences[0]
	if h := p.Labels["pod-template-hash"]; o.Kind == "ReplicaSet" && h != "" {
		return strings.TrimSuffix(o.Name, "-"+h)
	}
	return o.Name
}

func hasSidecar(p *corev1.Pod) bool {
	for _, cs := range [][]corev1.Container{p.Spec.Containers, p.Spec.InitContainers} {
		for _, c := range cs {
			if c.Name == "istio-proxy" {
				return true
			}
		}
	}
	return false
}

// slim keeps only what lookup needs; every node caches all pods of the cluster.
func slim(obj any) (any, error) {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return obj, nil
	}
	names := func(cs []corev1.Container) []corev1.Container {
		out := make([]corev1.Container, len(cs))
		for i := range cs {
			out[i].Name = cs[i].Name
		}
		return out
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace,
			Labels: p.Labels, OwnerReferences: p.OwnerReferences},
		Spec: corev1.PodSpec{HostNetwork: p.Spec.HostNetwork,
			Containers: names(p.Spec.Containers), InitContainers: names(p.Spec.InitContainers)},
		Status: corev1.PodStatus{Phase: p.Status.Phase, PodIPs: p.Status.PodIPs, HostIP: p.Status.HostIP},
	}, nil
}

// newLookup watches pods and services and returns the IP resolver.
func newLookup(ctx context.Context) (func(netip.Addr) endpoint, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	f := informers.NewSharedInformerFactory(client, 0)
	pods := f.Core().V1().Pods().Informer()
	svcs := f.Core().V1().Services().Informer()
	if err := pods.SetTransform(slim); err != nil {
		return nil, err
	}
	err = pods.AddIndexers(cache.Indexers{
		"ip": func(obj any) ([]string, error) {
			p := obj.(*corev1.Pod)
			// hostNetwork pods share the node IP; finished pods may have had theirs reused
			if p.Spec.HostNetwork || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				return nil, nil
			}
			var ips []string
			for _, ip := range p.Status.PodIPs {
				ips = append(ips, ip.IP)
			}
			return ips, nil
		},
		"hostip": func(obj any) ([]string, error) {
			if ip := obj.(*corev1.Pod).Status.HostIP; ip != "" {
				return []string{ip}, nil
			}
			return nil, nil
		},
	})
	if err != nil {
		return nil, err
	}
	err = svcs.AddIndexers(cache.Indexers{"ip": func(obj any) ([]string, error) {
		return obj.(*corev1.Service).Spec.ClusterIPs, nil
	}})
	if err != nil {
		return nil, err
	}
	f.Start(ctx.Done())
	for t, ok := range f.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return nil, fmt.Errorf("cache sync failed for %v", t)
		}
	}

	return func(ip netip.Addr) endpoint {
		s := ip.String()
		if objs, _ := pods.GetIndexer().ByIndex("ip", s); len(objs) > 0 {
			p := objs[0].(*corev1.Pod)
			return endpoint{pod, p.Namespace, workloadName(p), hasSidecar(p)}
		}
		// shortcut: a Service IP is shown as a node named after the Service, not
		// resolved to its backing workload. Only hit if Dataplane V2 leaves the
		// virtual IP on the socket; upgrade via EndpointSlices if these show up.
		if objs, _ := svcs.GetIndexer().ByIndex("ip", s); len(objs) > 0 {
			sv := objs[0].(*corev1.Service)
			return endpoint{kind: service, namespace: sv.Namespace, workload: sv.Name}
		}
		if objs, _ := pods.GetIndexer().ByIndex("hostip", s); len(objs) > 0 {
			return endpoint{kind: node, workload: "node"}
		}
		return endpoint{kind: external, workload: "external"}
	}, nil
}
