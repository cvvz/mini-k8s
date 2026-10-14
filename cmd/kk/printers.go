package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"

	"mini-k8s/internal/api"
	"mini-k8s/internal/client"
)

func newTabWriter(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 8, 3, ' ', 0) }

type tableOptions struct {
	wide          bool
	withNamespace bool
	header        bool
	now           time.Time
	// eventType, when set, prefixes watch rows like kubectl --output-watch-events.
	eventType string
}

type column struct {
	header string
	wide   bool
	value  func(obj api.Object, now time.Time) string
}

func age(t api.Time, now time.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	return humanDuration(now.Sub(t.Time))
}

// humanDuration mirrors kubectl's compact ages: 45s, 5m3s, 17m, 3h4m, 2d.
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch s := int(d.Seconds()); {
	case s < 120:
		return fmt.Sprintf("%ds", s)
	case s < 600:
		return fmt.Sprintf("%dm%ds", s/60, s%60)
	case d < 3*time.Hour:
		return fmt.Sprintf("%dm", s/60)
	case d < 8*time.Hour:
		return fmt.Sprintf("%dh%dm", s/3600, (s%3600)/60)
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", s/3600)
	default:
		return fmt.Sprintf("%dd", s/86400)
	}
}

func nameColumn() column {
	return column{header: "NAME", value: func(o api.Object, _ time.Time) string { return o.GetObjectMeta().Name }}
}

func ageColumn() column {
	return column{header: "AGE", value: func(o api.Object, now time.Time) string { return age(o.GetObjectMeta().CreationTimestamp, now) }}
}

// PodStatusReason summarises a Pod the way kubectl's STATUS column does.
func PodStatusReason(p *api.Pod) string {
	if p.DeletionTimestamp != nil {
		return "Terminating"
	}
	reason := p.Status.Phase
	if p.Status.Reason != "" {
		reason = p.Status.Reason
	}
	for _, cs := range p.Status.ContainerStatuses {
		switch {
		case cs.State.Waiting != nil && cs.State.Waiting.Reason != "":
			reason = cs.State.Waiting.Reason
		case cs.State.Terminated != nil && cs.State.Terminated.Reason != "":
			reason = cs.State.Terminated.Reason
		}
	}
	if reason == "" {
		reason = api.PodPending
	}
	return reason
}

func podReadyCount(p *api.Pod) string {
	ready := 0
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
	}
	return fmt.Sprintf("%d/%d", ready, len(p.Spec.Containers))
}

func podRestarts(p *api.Pod) string {
	n := int32(0)
	for _, cs := range p.Status.ContainerStatuses {
		n += cs.RestartCount
	}
	return fmt.Sprint(n)
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

func nodeStatus(n *api.Node, now time.Time) string {
	s := "NotReady"
	if api.IsNodeReady(n, now) {
		s = "Ready"
	}
	if n.Spec.Unschedulable {
		s += ",SchedulingDisabled"
	}
	return s
}

func servicePorts(s *api.Service) string {
	var parts []string
	for _, p := range s.Spec.Ports {
		parts = append(parts, fmt.Sprintf("%d->%d/%s", p.Port, p.TargetPort, p.Protocol))
	}
	return strings.Join(parts, ",")
}

func labelsString(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, ",")
}

func columnsFor(res api.ResourceInfo) []column {
	switch res.Resource {
	case api.Pods.Resource:
		return []column{nameColumn(),
			{header: "READY", value: func(o api.Object, _ time.Time) string { return podReadyCount(o.(*api.Pod)) }},
			{header: "STATUS", value: func(o api.Object, _ time.Time) string { return PodStatusReason(o.(*api.Pod)) }},
			{header: "RESTARTS", value: func(o api.Object, _ time.Time) string { return podRestarts(o.(*api.Pod)) }},
			ageColumn(),
			{header: "IP", wide: true, value: func(o api.Object, _ time.Time) string { return orNone(o.(*api.Pod).Status.PodIP) }},
			{header: "NODE", wide: true, value: func(o api.Object, _ time.Time) string { return orNone(o.(*api.Pod).Spec.NodeName) }},
		}
	case api.Deployments.Resource:
		return []column{nameColumn(),
			{header: "READY", value: func(o api.Object, _ time.Time) string {
				d := o.(*api.Deployment)
				return fmt.Sprintf("%d/%d", d.Status.ReadyReplicas, *d.Spec.Replicas)
			}},
			{header: "UP-TO-DATE", value: func(o api.Object, _ time.Time) string { return fmt.Sprint(o.(*api.Deployment).Status.UpdatedReplicas) }},
			{header: "AVAILABLE", value: func(o api.Object, _ time.Time) string {
				return fmt.Sprint(o.(*api.Deployment).Status.AvailableReplicas)
			}},
			ageColumn(),
			{header: "CONTAINERS", wide: true, value: func(o api.Object, _ time.Time) string {
				var names []string
				for _, c := range o.(*api.Deployment).Spec.Template.Spec.Containers {
					names = append(names, c.Name)
				}
				return strings.Join(names, ",")
			}},
			{header: "IMAGES", wide: true, value: func(o api.Object, _ time.Time) string {
				var images []string
				for _, c := range o.(*api.Deployment).Spec.Template.Spec.Containers {
					images = append(images, c.Image)
				}
				return strings.Join(images, ",")
			}},
			{header: "SELECTOR", wide: true, value: func(o api.Object, _ time.Time) string {
				sel, _ := o.(*api.Deployment).Spec.Selector.AsSelector()
				return sel.String()
			}},
		}
	case api.Services.Resource:
		return []column{nameColumn(),
			{header: "SELECTOR", value: func(o api.Object, _ time.Time) string { return labelsString(o.(*api.Service).Spec.Selector) }},
			{header: "PORT(S)", value: func(o api.Object, _ time.Time) string { return servicePorts(o.(*api.Service)) }},
			ageColumn(),
		}
	case api.Nodes.Resource:
		return []column{nameColumn(),
			{header: "STATUS", value: func(o api.Object, now time.Time) string { return nodeStatus(o.(*api.Node), now) }},
			{header: "ROLES", value: func(api.Object, time.Time) string { return "worker" }},
			ageColumn(),
			{header: "VERSION", value: func(o api.Object, _ time.Time) string { return o.(*api.Node).Status.NodeInfo.KubeletVersion }},
			{header: "INTERNAL-IP", wide: true, value: func(o api.Object, _ time.Time) string { return orNone(o.(*api.Node).InternalIP()) }},
			{header: "CONTAINER-RUNTIME", wide: true, value: func(o api.Object, _ time.Time) string {
				return o.(*api.Node).Status.NodeInfo.ContainerRuntimeVersion
			}},
		}
	case api.Namespaces.Resource:
		return []column{nameColumn(),
			{header: "STATUS", value: func(o api.Object, _ time.Time) string { return o.(*api.Namespace).Status.Phase }},
			ageColumn(),
		}
	case api.Events.Resource:
		return []column{
			{header: "LAST SEEN", value: func(o api.Object, now time.Time) string { return age(o.(*api.Event).LastTimestamp, now) }},
			{header: "TYPE", value: func(o api.Object, _ time.Time) string { return o.(*api.Event).Type }},
			{header: "REASON", value: func(o api.Object, _ time.Time) string { return o.(*api.Event).Reason }},
			{header: "OBJECT", value: func(o api.Object, _ time.Time) string {
				ref := o.(*api.Event).InvolvedObject
				return strings.ToLower(ref.Kind) + "/" + ref.Name
			}},
			{header: "COUNT", wide: true, value: func(o api.Object, _ time.Time) string { return fmt.Sprint(o.(*api.Event).Count) }},
			{header: "MESSAGE", value: func(o api.Object, _ time.Time) string { return o.(*api.Event).Message }},
		}
	}
	return []column{nameColumn(), ageColumn()}
}

func printTable(w io.Writer, res api.ResourceInfo, objs []api.Object, o tableOptions) error {
	if res.Resource == api.Events.Resource {
		sort.SliceStable(objs, func(i, j int) bool {
			return objs[i].(*api.Event).LastTimestamp.Before(objs[j].(*api.Event).LastTimestamp.Time)
		})
	}
	var cols []column
	for _, c := range columnsFor(res) {
		if !c.wide || o.wide {
			cols = append(cols, c)
		}
	}
	tw := newTabWriter(w)
	if o.header {
		var headers []string
		if o.eventType != "" {
			headers = append(headers, "EVENT")
		}
		if o.withNamespace {
			headers = append(headers, "NAMESPACE")
		}
		for _, c := range cols {
			headers = append(headers, c.header)
		}
		fmt.Fprintln(tw, strings.Join(headers, "\t"))
	}
	for _, obj := range objs {
		var cells []string
		if o.eventType != "" {
			cells = append(cells, o.eventType)
		}
		if o.withNamespace {
			cells = append(cells, obj.GetObjectMeta().Namespace)
		}
		for _, c := range cols {
			cells = append(cells, c.value(obj, o.now))
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

// writeStructured prints v as indented JSON or as YAML.
func writeStructured(w io.Writer, v any, format string) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if format == "json" {
		var generic any
		_ = json.Unmarshal(data, &generic)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "    ")
		return enc.Encode(generic)
	}
	var generic any
	if err := json.Unmarshal(data, &generic); err != nil {
		return err
	}
	out, err := marshalYAML(generic)
	if err != nil {
		return err
	}
	_, err = w.Write(out)
	return err
}

// marshalYAML renders v with two-space indentation, as kubectl does.
func marshalYAML(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func indentYAML(v any, prefix string) string {
	data, _ := json.Marshal(v)
	var generic any
	_ = json.Unmarshal(data, &generic)
	if m, ok := generic.(map[string]any); ok && len(m) == 0 {
		return prefix + "<none>\n"
	}
	out, _ := marshalYAML(generic)
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		b.WriteString(prefix + line + "\n")
	}
	return b.String()
}

func (a *app) describeObject(ctx context.Context, res api.ResourceInfo, obj api.Object) error {
	m := obj.GetObjectMeta()
	now := a.now()
	tw := newTabWriter(a.out)
	fmt.Fprintf(tw, "Name:\t%s\n", m.Name)
	if res.Namespaced {
		fmt.Fprintf(tw, "Namespace:\t%s\n", m.Namespace)
	}
	fmt.Fprintf(tw, "Labels:\t%s\n", orNone(labelsString(m.Labels)))
	fmt.Fprintf(tw, "Annotations:\t%s\n", orNone(labelsString(m.Annotations)))
	fmt.Fprintf(tw, "CreationTimestamp:\t%s (%s ago)\n", m.CreationTimestamp.Format(time.RFC3339), age(m.CreationTimestamp, now))
	if ref := api.ControllerOf(m); ref != nil {
		fmt.Fprintf(tw, "Controlled By:\t%s/%s\n", ref.Kind, ref.Name)
	}
	if len(m.Finalizers) > 0 {
		fmt.Fprintf(tw, "Finalizers:\t%s\n", strings.Join(m.Finalizers, ","))
	}
	if m.DeletionTimestamp != nil {
		fmt.Fprintf(tw, "Terminating:\tsince %s (grace %ds)\n", m.DeletionTimestamp.Format(time.RFC3339), *m.DeletionGracePeriodSeconds)
	}
	switch o := obj.(type) {
	case *api.Pod:
		fmt.Fprintf(tw, "Node:\t%s\n", orNone(o.Spec.NodeName))
		fmt.Fprintf(tw, "Status:\t%s\n", PodStatusReason(o))
		fmt.Fprintf(tw, "IP:\t%s\n", orNone(o.Status.PodIP))
	case *api.Deployment:
		fmt.Fprintf(tw, "Replicas:\t%d desired | %d updated | %d total | %d available | %d unavailable\n",
			*o.Spec.Replicas, o.Status.UpdatedReplicas, o.Status.Replicas, o.Status.AvailableReplicas, o.Status.UnavailableReplicas)
	case *api.Node:
		fmt.Fprintf(tw, "Status:\t%s\n", nodeStatus(o, now))
		fmt.Fprintf(tw, "InternalIP:\t%s\n", orNone(o.InternalIP()))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	data, _ := json.Marshal(obj)
	var parts map[string]any
	_ = json.Unmarshal(data, &parts)
	for _, section := range []string{"spec", "status", "involvedObject"} {
		if v, ok := parts[section]; ok {
			fmt.Fprintf(a.out, "%s%s:\n%s", strings.ToUpper(section[:1]), section[1:], indentYAML(v, "  "))
		}
	}
	if res.Resource == api.Events.Resource {
		return nil
	}
	ns := m.Namespace
	if ns == "" {
		ns = api.NamespaceDefault
	}
	events, err := client.Events(a.client, ns).List(ctx, client.ListOptions{FieldSelector: "involvedObject.uid=" + m.UID})
	if err != nil {
		return err
	}
	if len(events.Items) == 0 {
		_, err := fmt.Fprintln(a.out, "Events:\t<none>")
		return err
	}
	sort.Slice(events.Items, func(i, j int) bool { return events.Items[i].LastTimestamp.Before(events.Items[j].LastTimestamp.Time) })
	fmt.Fprintln(a.out, "Events:")
	tw = newTabWriter(a.out)
	fmt.Fprintln(tw, "  Type\tReason\tAge\tFrom\tMessage")
	for _, e := range events.Items {
		ageStr := age(e.LastTimestamp, now)
		if e.Count > 1 {
			ageStr += fmt.Sprintf(" (x%d)", e.Count)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", e.Type, e.Reason, ageStr, e.Source.Component, e.Message)
	}
	return tw.Flush()
}
