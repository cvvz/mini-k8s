package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"

	appsv1 "mini-k8s/pkg/apis/apps/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	networkingv1 "mini-k8s/pkg/apis/networking/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/scheme"
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
	value  func(obj metav1.Object, now time.Time) string
}

func age(t metav1.Time, now time.Time) string {
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
	return column{header: "NAME", value: func(o metav1.Object, _ time.Time) string { return o.GetObjectMeta().Name }}
}

func ageColumn() column {
	return column{header: "AGE", value: func(o metav1.Object, now time.Time) string { return age(o.GetObjectMeta().CreationTimestamp, now) }}
}

// podSummary computes kubectl's READY, STATUS and RESTARTS columns for a Pod
// (printPod in Kubernetes): the first unfinished init container decides the
// status while the Pod initializes; afterwards the containers do, and a
// "Completed" container next to running or failed ones reads Running,
// NotReady or the failure.
func podSummary(p *corev1.Pod) (ready, status string, restarts int32) {
	reason := p.Status.Phase
	if p.Status.Reason != "" {
		reason = p.Status.Reason
	}
	initializing := false
	for i, cs := range p.Status.InitContainerStatuses {
		restarts += cs.RestartCount
		switch {
		case cs.State.Terminated != nil && cs.State.Terminated.ExitCode == 0:
			continue
		case cs.State.Terminated != nil && cs.State.Terminated.Reason == "":
			reason = fmt.Sprintf("Init:ExitCode:%d", cs.State.Terminated.ExitCode)
		case cs.State.Terminated != nil:
			reason = "Init:" + cs.State.Terminated.Reason
		case cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && cs.State.Waiting.Reason != "PodInitializing":
			reason = "Init:" + cs.State.Waiting.Reason
		default:
			reason = fmt.Sprintf("Init:%d/%d", i, len(p.Spec.InitContainers))
		}
		initializing = true
		break
	}
	readyCount := 0
	if c := p.Status.Condition(corev1.PodInitialized); !initializing || (c != nil && c.Status == corev1.ConditionTrue) {
		restarts = 0
		running := false
		errorReason := ""
		for i := len(p.Status.ContainerStatuses) - 1; i >= 0; i-- {
			cs := p.Status.ContainerStatuses[i]
			restarts += cs.RestartCount
			switch {
			case cs.State.Waiting != nil && cs.State.Waiting.Reason != "":
				reason = cs.State.Waiting.Reason
			case cs.State.Terminated != nil:
				reason = cs.State.Terminated.Reason
				if reason == "" {
					reason = fmt.Sprintf("ExitCode:%d", cs.State.Terminated.ExitCode)
				}
				if cs.State.Terminated.ExitCode != 0 {
					errorReason = reason
				}
			case cs.Ready && cs.State.Running != nil:
				running = true
				readyCount++
			}
		}
		if reason == "Completed" {
			readyCond := p.Status.Condition(corev1.PodReady)
			switch {
			case running && readyCond != nil && readyCond.Status == corev1.ConditionTrue:
				reason = corev1.PodRunning
			case errorReason != "":
				reason = errorReason
			case running:
				reason = "NotReady"
			}
		}
	}
	if reason == "" {
		reason = corev1.PodPending
	}
	if p.DeletionTimestamp != nil && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
		reason = "Terminating"
	}
	return fmt.Sprintf("%d/%d", readyCount, len(p.Spec.Containers)), reason, restarts
}

// PodStatusReason summarises a Pod the way kubectl's STATUS column does.
func PodStatusReason(p *corev1.Pod) string {
	_, status, _ := podSummary(p)
	return status
}

func podReadyCount(p *corev1.Pod) string {
	ready, _, _ := podSummary(p)
	return ready
}

func podRestarts(p *corev1.Pod) string {
	_, _, restarts := podSummary(p)
	return fmt.Sprint(restarts)
}

// nodeRoles lists the node-role.kubernetes.io/<role> labels, like kubectl.
func nodeRoles(n *corev1.Node) string {
	var roles []string
	for k := range n.Labels {
		if role, ok := strings.CutPrefix(k, corev1.LabelNodeRolePrefix); ok && role != "" {
			roles = append(roles, role)
		}
	}
	sort.Strings(roles)
	return orNone(strings.Join(roles, ","))
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

func nodeStatus(n *corev1.Node, now time.Time) string {
	s := "NotReady"
	if corev1.IsNodeReady(n, now) {
		s = "Ready"
	}
	if n.Spec.Unschedulable {
		s += ",SchedulingDisabled"
	}
	return s
}

// servicePorts is kubectl's PORT(S) column: 80/TCP, or 80:30080/TCP with a
// node port.
func servicePorts(s *corev1.Service) string {
	if len(s.Spec.Ports) == 0 {
		return "<none>"
	}
	var parts []string
	for _, p := range s.Spec.Ports {
		if p.NodePort != 0 {
			parts = append(parts, fmt.Sprintf("%d:%d/%s", p.Port, p.NodePort, p.Protocol))
		} else {
			parts = append(parts, fmt.Sprintf("%d/%s", p.Port, p.Protocol))
		}
	}
	return strings.Join(parts, ",")
}

// formatEndpoints is kubectl's ENDPOINTS column: the first three ready
// ip:port pairs, port by port, then "+ N more...".
func formatEndpoints(e *corev1.Endpoints) string {
	if len(e.Subsets) == 0 {
		return "<none>"
	}
	const limit = 3
	var list []string
	count := 0
	for _, ss := range e.Subsets {
		for _, port := range ss.Ports {
			count += len(ss.Addresses)
			for _, a := range ss.Addresses {
				if len(list) < limit {
					list = append(list, net.JoinHostPort(a.IP, strconv.Itoa(int(port.Port))))
				}
			}
		}
	}
	if count > limit {
		return fmt.Sprintf("%s + %d more...", strings.Join(list, ","), count-limit)
	}
	return strings.Join(list, ",")
}

// ingressHosts is kubectl's HOSTS column: the rule hosts, "*" without any.
func ingressHosts(ing *networkingv1.Ingress) string {
	var hosts []string
	for _, r := range ing.Spec.Rules {
		if r.Host != "" {
			hosts = append(hosts, r.Host)
		}
	}
	switch {
	case len(hosts) == 0:
		return "*"
	case len(hosts) > 3:
		return fmt.Sprintf("%s + %d more...", strings.Join(hosts[:3], ","), len(hosts)-3)
	}
	return strings.Join(hosts, ",")
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

func columnsFor(res scheme.ResourceInfo) []column {
	switch res.Resource {
	case scheme.Pods.Resource:
		return []column{nameColumn(),
			{header: "READY", value: func(o metav1.Object, _ time.Time) string { return podReadyCount(o.(*corev1.Pod)) }},
			{header: "STATUS", value: func(o metav1.Object, _ time.Time) string { return PodStatusReason(o.(*corev1.Pod)) }},
			{header: "RESTARTS", value: func(o metav1.Object, _ time.Time) string { return podRestarts(o.(*corev1.Pod)) }},
			ageColumn(),
			{header: "IP", wide: true, value: func(o metav1.Object, _ time.Time) string { return orNone(o.(*corev1.Pod).Status.PodIP) }},
			{header: "NODE", wide: true, value: func(o metav1.Object, _ time.Time) string { return orNone(o.(*corev1.Pod).Spec.NodeName) }},
		}
	case scheme.Deployments.Resource:
		return []column{nameColumn(),
			{header: "READY", value: func(o metav1.Object, _ time.Time) string {
				d := o.(*appsv1.Deployment)
				return fmt.Sprintf("%d/%d", d.Status.ReadyReplicas, *d.Spec.Replicas)
			}},
			{header: "UP-TO-DATE", value: func(o metav1.Object, _ time.Time) string {
				return fmt.Sprint(o.(*appsv1.Deployment).Status.UpdatedReplicas)
			}},
			{header: "AVAILABLE", value: func(o metav1.Object, _ time.Time) string {
				return fmt.Sprint(o.(*appsv1.Deployment).Status.AvailableReplicas)
			}},
			ageColumn(),
			{header: "CONTAINERS", wide: true, value: func(o metav1.Object, _ time.Time) string {
				var names []string
				for _, c := range o.(*appsv1.Deployment).Spec.Template.Spec.Containers {
					names = append(names, c.Name)
				}
				return strings.Join(names, ",")
			}},
			{header: "IMAGES", wide: true, value: func(o metav1.Object, _ time.Time) string {
				var images []string
				for _, c := range o.(*appsv1.Deployment).Spec.Template.Spec.Containers {
					images = append(images, c.Image)
				}
				return strings.Join(images, ",")
			}},
			{header: "SELECTOR", wide: true, value: func(o metav1.Object, _ time.Time) string {
				sel, _ := o.(*appsv1.Deployment).Spec.Selector.AsSelector()
				return sel.String()
			}},
		}
	case scheme.ReplicaSets.Resource:
		return replicaSetColumns()
	case scheme.Services.Resource:
		svc := func(o metav1.Object) *corev1.Service { return o.(*corev1.Service) }
		return []column{nameColumn(),
			{header: "TYPE", value: func(o metav1.Object, _ time.Time) string { return svc(o).Spec.Type }},
			{header: "CLUSTER-IP", value: func(o metav1.Object, _ time.Time) string { return orNone(svc(o).Spec.ClusterIP) }},
			{header: "EXTERNAL-IP", value: func(metav1.Object, time.Time) string { return "<none>" }},
			{header: "PORT(S)", value: func(o metav1.Object, _ time.Time) string { return servicePorts(svc(o)) }},
			ageColumn(),
			{header: "SELECTOR", wide: true, value: func(o metav1.Object, _ time.Time) string { return orNone(labelsString(svc(o).Spec.Selector)) }},
		}
	case scheme.Endpoints.Resource:
		return []column{nameColumn(),
			{header: "ENDPOINTS", value: func(o metav1.Object, _ time.Time) string { return formatEndpoints(o.(*corev1.Endpoints)) }},
			ageColumn(),
		}
	case scheme.Ingresses.Resource:
		ing := func(o metav1.Object) *networkingv1.Ingress { return o.(*networkingv1.Ingress) }
		return []column{nameColumn(),
			{header: "CLASS", value: func(o metav1.Object, _ time.Time) string {
				if c := ing(o).Spec.IngressClassName; c != nil {
					return *c
				}
				return "<none>"
			}},
			{header: "HOSTS", value: func(o metav1.Object, _ time.Time) string { return ingressHosts(ing(o)) }},
			{header: "ADDRESS", value: func(o metav1.Object, _ time.Time) string {
				var addrs []string
				for _, a := range ing(o).Status.LoadBalancer.Ingress {
					addrs = append(addrs, a.IP+a.Hostname)
				}
				return strings.Join(addrs, ",")
			}},
			{header: "PORTS", value: func(metav1.Object, time.Time) string { return "80" }},
			ageColumn(),
		}
	case scheme.Nodes.Resource:
		return []column{nameColumn(),
			{header: "STATUS", value: func(o metav1.Object, now time.Time) string { return nodeStatus(o.(*corev1.Node), now) }},
			{header: "ROLES", value: func(o metav1.Object, _ time.Time) string { return nodeRoles(o.(*corev1.Node)) }},
			ageColumn(),
			{header: "VERSION", value: func(o metav1.Object, _ time.Time) string { return o.(*corev1.Node).Status.NodeInfo.KubeletVersion }},
			{header: "INTERNAL-IP", wide: true, value: func(o metav1.Object, _ time.Time) string { return orNone(o.(*corev1.Node).InternalIP()) }},
			{header: "CONTAINER-RUNTIME", wide: true, value: func(o metav1.Object, _ time.Time) string {
				return o.(*corev1.Node).Status.NodeInfo.ContainerRuntimeVersion
			}},
		}
	case scheme.Namespaces.Resource:
		return []column{nameColumn(),
			{header: "STATUS", value: func(o metav1.Object, _ time.Time) string { return o.(*corev1.Namespace).Status.Phase }},
			ageColumn(),
		}
	case scheme.Events.Resource:
		return []column{
			{header: "LAST SEEN", value: func(o metav1.Object, now time.Time) string { return age(o.(*corev1.Event).LastTimestamp, now) }},
			{header: "TYPE", value: func(o metav1.Object, _ time.Time) string { return o.(*corev1.Event).Type }},
			{header: "REASON", value: func(o metav1.Object, _ time.Time) string { return o.(*corev1.Event).Reason }},
			{header: "OBJECT", value: func(o metav1.Object, _ time.Time) string {
				ref := o.(*corev1.Event).InvolvedObject
				return strings.ToLower(ref.Kind) + "/" + ref.Name
			}},
			{header: "COUNT", wide: true, value: func(o metav1.Object, _ time.Time) string { return fmt.Sprint(o.(*corev1.Event).Count) }},
			{header: "MESSAGE", value: func(o metav1.Object, _ time.Time) string { return o.(*corev1.Event).Message }},
		}
	}
	return []column{nameColumn(), ageColumn()}
}

func printTable(w io.Writer, res scheme.ResourceInfo, objs []metav1.Object, o tableOptions) error {
	if res.Resource == scheme.Events.Resource {
		sort.SliceStable(objs, func(i, j int) bool {
			return objs[i].(*corev1.Event).LastTimestamp.Before(objs[j].(*corev1.Event).LastTimestamp.Time)
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

func (a *app) describeObject(ctx context.Context, res scheme.ResourceInfo, obj metav1.Object) error {
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
	if ref := metav1.ControllerOf(m); ref != nil {
		fmt.Fprintf(tw, "Controlled By:\t%s/%s\n", ref.Kind, ref.Name)
	}
	if len(m.Finalizers) > 0 {
		fmt.Fprintf(tw, "Finalizers:\t%s\n", strings.Join(m.Finalizers, ","))
	}
	if m.DeletionTimestamp != nil {
		fmt.Fprintf(tw, "Terminating:\tsince %s (grace %ds)\n", m.DeletionTimestamp.Format(time.RFC3339), *m.DeletionGracePeriodSeconds)
	}
	switch o := obj.(type) {
	case *corev1.Pod:
		fmt.Fprintf(tw, "Node:\t%s\n", orNone(o.Spec.NodeName))
		fmt.Fprintf(tw, "Status:\t%s\n", PodStatusReason(o))
		fmt.Fprintf(tw, "IP:\t%s\n", orNone(o.Status.PodIP))
	case *appsv1.Deployment:
		fmt.Fprintf(tw, "Replicas:\t%d desired | %d updated | %d total | %d available | %d unavailable\n",
			*o.Spec.Replicas, o.Status.UpdatedReplicas, o.Status.Replicas, o.Status.AvailableReplicas, o.Status.UnavailableReplicas)
	case *corev1.Service:
		fmt.Fprintf(tw, "Type:\t%s\n", o.Spec.Type)
		fmt.Fprintf(tw, "IP:\t%s\n", orNone(o.Spec.ClusterIP))
		fmt.Fprintf(tw, "Port(s):\t%s\n", servicePorts(o))
	case *corev1.Node:
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
	if res.Resource == scheme.Events.Resource {
		return nil
	}
	ns := m.Namespace
	if ns == "" {
		ns = corev1.NamespaceDefault
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
