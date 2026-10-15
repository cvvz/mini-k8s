package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"mini-k8s/pkg/api/resource"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	metricsv1beta1 "mini-k8s/pkg/apis/metrics/v1beta1"
	"mini-k8s/pkg/client"
)

const topUsage = "usage: kk top nodes|pods [NAME] [-l SELECTOR] [--containers] [--sort-by cpu|memory]"

// errMetricsAPI is kubectl's message when metrics-server does not answer.
var errMetricsAPI = errors.New("Metrics API not available")

// metricsGet reads the metrics API, turning an unavailable API into
// kubectl's error.
func (a *app) metricsGet(ctx context.Context, path string, q url.Values, into any) error {
	err := a.client.Do(ctx, http.MethodGet, "/apis/"+metricsv1beta1.GroupVersion+path, q, "", nil, into)
	if client.Reason(err) == metav1.ReasonServiceUnavail {
		return errMetricsAPI
	}
	return err
}

// cpuMilli and memMi format usage like kubectl top: "330m", "2907Mi".
func cpuMilli(q resource.Quantity) string { return fmt.Sprintf("%dm", q.MilliValue()) }
func memMi(q resource.Quantity) string    { return fmt.Sprintf("%dMi", q.Value()/(1024*1024)) }

// top implements `kk top nodes` and `kk top pods`.
func (a *app) top(ctx context.Context, p args) error {
	if len(p.positional) < 1 || len(p.positional) > 2 {
		return errors.New(topUsage)
	}
	name := ""
	if len(p.positional) == 2 {
		name = p.positional[1]
	}
	sortBy := p.value("sort-by", "")
	if sortBy != "" && sortBy != "cpu" && sortBy != "memory" {
		return fmt.Errorf("--sort-by accepts only cpu or memory")
	}
	q := url.Values{}
	if sel := p.value("selector", ""); sel != "" {
		q.Set("labelSelector", sel)
	}
	switch p.positional[0] {
	case "node", "nodes", "no":
		return a.topNodes(ctx, name, q, sortBy)
	case "pod", "pods", "po":
		return a.topPods(ctx, name, q, sortBy, p.boolean("containers", false))
	}
	return errors.New(topUsage)
}

func (a *app) topNodes(ctx context.Context, name string, q url.Values, sortBy string) error {
	var items []metricsv1beta1.NodeMetrics
	if name != "" {
		var m metricsv1beta1.NodeMetrics
		if err := a.metricsGet(ctx, "/nodes/"+name, nil, &m); err != nil {
			return err
		}
		items = append(items, m)
	} else {
		var list metricsv1beta1.NodeMetricsList
		if err := a.metricsGet(ctx, "/nodes", q, &list); err != nil {
			return err
		}
		items = list.Items
	}
	if len(items) == 0 {
		return errors.New("metrics not available yet")
	}
	nodes, err := client.Nodes(a.client).List(ctx, client.ListOptions{LabelSelector: q.Get("labelSelector")})
	if err != nil {
		return err
	}
	allocatable := map[string]corev1.ResourceList{}
	for _, n := range nodes.Items {
		allocatable[n.Name] = n.Status.Allocatable
	}
	sortMetrics(items, sortBy, func(m metricsv1beta1.NodeMetrics) corev1.ResourceList { return m.Usage },
		func(m metricsv1beta1.NodeMetrics) string { return m.Name })
	tw := newTabWriter(a.out)
	fmt.Fprintln(tw, "NAME\tCPU(cores)\tCPU(%)\tMEMORY(bytes)\tMEMORY(%)")
	percent := func(used, total resource.Quantity) string {
		if total.MilliValue() == 0 {
			return "<unknown>"
		}
		return fmt.Sprintf("%d%%", used.MilliValue()*100/total.MilliValue())
	}
	for _, m := range items {
		cpu, mem := m.Usage[corev1.ResourceCPU], m.Usage[corev1.ResourceMemory]
		alloc := allocatable[m.Name]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.Name, cpuMilli(cpu), percent(cpu, alloc[corev1.ResourceCPU]), memMi(mem),
			percent(mem, alloc[corev1.ResourceMemory]))
	}
	return tw.Flush()
}

func (a *app) topPods(ctx context.Context, name string, q url.Values, sortBy string, containers bool) error {
	var items []metricsv1beta1.PodMetrics
	switch {
	case name != "":
		var m metricsv1beta1.PodMetrics
		err := a.metricsGet(ctx, "/namespaces/"+a.namespace+"/pods/"+name, nil, &m)
		if client.IsNotFound(err) {
			// The Pod may exist without metrics yet.
			if _, perr := client.Pods(a.client, a.namespace).Get(ctx, name); perr != nil {
				return perr
			}
			return fmt.Errorf("Metrics not available for pod %s/%s", a.namespace, name)
		}
		if err != nil {
			return err
		}
		items = append(items, m)
	case a.allNamespaces:
		var list metricsv1beta1.PodMetricsList
		if err := a.metricsGet(ctx, "/pods", q, &list); err != nil {
			return err
		}
		items = list.Items
	default:
		var list metricsv1beta1.PodMetricsList
		if err := a.metricsGet(ctx, "/namespaces/"+a.namespace+"/pods", q, &list); err != nil {
			return err
		}
		items = list.Items
	}
	if len(items) == 0 {
		if a.allNamespaces {
			_, err := fmt.Fprintln(a.errOut, "No resources found")
			return err
		}
		_, err := fmt.Fprintf(a.errOut, "No resources found in %s namespace.\n", a.namespace)
		return err
	}
	total := func(m metricsv1beta1.PodMetrics) corev1.ResourceList {
		var cpu, mem int64
		for _, c := range m.Containers {
			cpu += c.Usage[corev1.ResourceCPU].MilliValue()
			mem += c.Usage[corev1.ResourceMemory].MilliValue()
		}
		return corev1.ResourceList{corev1.ResourceCPU: resource.NewMilliQuantity(cpu), corev1.ResourceMemory: resource.NewMilliQuantity(mem)}
	}
	sortMetrics(items, sortBy, total, func(m metricsv1beta1.PodMetrics) string { return m.Namespace + "/" + m.Name })
	tw := newTabWriter(a.out)
	var header []string
	if a.allNamespaces {
		header = append(header, "NAMESPACE")
	}
	if containers {
		header = append(header, "POD", "NAME")
	} else {
		header = append(header, "NAME")
	}
	fmt.Fprintln(tw, strings.Join(append(header, "CPU(cores)", "MEMORY(bytes)"), "\t"))
	for _, m := range items {
		var prefix []string
		if a.allNamespaces {
			prefix = append(prefix, m.Namespace)
		}
		if !containers {
			usage := total(m)
			fmt.Fprintln(tw, strings.Join(append(prefix, m.Name, cpuMilli(usage[corev1.ResourceCPU]), memMi(usage[corev1.ResourceMemory])), "\t"))
			continue
		}
		for _, c := range m.Containers {
			fmt.Fprintln(tw, strings.Join(append(append([]string{}, prefix...), m.Name, c.Name, cpuMilli(c.Usage[corev1.ResourceCPU]),
				memMi(c.Usage[corev1.ResourceMemory])), "\t"))
		}
	}
	return tw.Flush()
}

// sortMetrics orders by name, or by descending CPU or memory.
func sortMetrics[T any](items []T, by string, usage func(T) corev1.ResourceList, name func(T) string) {
	sort.SliceStable(items, func(i, j int) bool {
		switch by {
		case "cpu", "memory":
			key := corev1.ResourceCPU
			if by == "memory" {
				key = corev1.ResourceMemory
			}
			if c := usage(items[i])[key].Cmp(usage(items[j])[key]); c != 0 {
				return c > 0
			}
		}
		return name(items[i]) < name(items[j])
	})
}
