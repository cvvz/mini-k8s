package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"mini-k8s/pkg/api/resource"
	autoscalingv2 "mini-k8s/pkg/apis/autoscaling/v2"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/scheme"
	"mini-k8s/pkg/util/ptr"
)

const autoscaleUsage = "usage: kk autoscale deployment|replicaset|statefulset NAME --max N [--min N] [--cpu-percent N | --cpu N%|QUANTITY] [--name NAME]"

// hpaTargets is kubectl's TARGETS column: "cpu: 201%/50%", "memory:
// <unknown>/100Mi", at most two metrics and a count of the rest.
func hpaTargets(h *autoscalingv2.HorizontalPodAutoscaler) string {
	specs, statuses := h.Spec.Metrics, h.Status.CurrentMetrics
	if len(specs) == 0 {
		return "<none>"
	}
	var list []string
	for i, spec := range specs {
		if len(list) == 2 {
			return strings.Join(list, ", ") + fmt.Sprintf(" + %d more...", len(specs)-2)
		}
		name, target, current := "", autoscalingv2.MetricTarget{}, (*autoscalingv2.MetricValueStatus)(nil)
		switch {
		case spec.Resource != nil:
			name, target = spec.Resource.Name, spec.Resource.Target
			if i < len(statuses) && statuses[i].Resource != nil {
				current = &statuses[i].Resource.Current
			}
		case spec.ContainerResource != nil:
			name, target = spec.ContainerResource.Name, spec.ContainerResource.Target
			if i < len(statuses) && statuses[i].ContainerResource != nil {
				current = &statuses[i].ContainerResource.Current
			}
		default:
			list = append(list, "<unknown>")
			continue
		}
		cur := "<unknown>"
		if target.AverageValue != nil {
			if current != nil && current.AverageValue != nil {
				cur = current.AverageValue.String()
			}
			list = append(list, fmt.Sprintf("%s: %s/%s", name, cur, target.AverageValue.String()))
			continue
		}
		if current != nil && current.AverageUtilization != nil {
			cur = fmt.Sprintf("%d%%", *current.AverageUtilization)
		}
		want := "<auto>"
		if target.AverageUtilization != nil {
			want = fmt.Sprintf("%d%%", *target.AverageUtilization)
		}
		list = append(list, fmt.Sprintf("%s: %s/%s", name, cur, want))
	}
	return strings.Join(list, ", ")
}

// describeHPA writes the autoscaler-specific lines of `kk describe hpa`.
func describeHPA(w io.Writer, h *autoscalingv2.HorizontalPodAutoscaler) {
	ref := h.Spec.ScaleTargetRef
	fmt.Fprintf(w, "Reference:\t%s/%s\n", ref.Kind, ref.Name)
	fmt.Fprintf(w, "Metrics:\t( current / target )\n")
	for i, spec := range h.Spec.Metrics {
		var name, label string
		var target autoscalingv2.MetricTarget
		var current *autoscalingv2.MetricValueStatus
		switch {
		case spec.Resource != nil:
			name, target, label = spec.Resource.Name, spec.Resource.Target, fmt.Sprintf("resource %s on pods", spec.Resource.Name)
			if i < len(h.Status.CurrentMetrics) && h.Status.CurrentMetrics[i].Resource != nil {
				current = &h.Status.CurrentMetrics[i].Resource.Current
			}
		case spec.ContainerResource != nil:
			c := spec.ContainerResource
			name, target, label = c.Name, c.Target, fmt.Sprintf("resource %s of container %q on pods", c.Name, c.Container)
			if i < len(h.Status.CurrentMetrics) && h.Status.CurrentMetrics[i].ContainerResource != nil {
				current = &h.Status.CurrentMetrics[i].ContainerResource.Current
			}
		default:
			continue
		}
		_ = name
		if target.AverageValue != nil {
			cur := "<unknown>"
			if current != nil && current.AverageValue != nil {
				cur = current.AverageValue.String()
			}
			fmt.Fprintf(w, "  %s:\t%s / %s\n", label, cur, target.AverageValue.String())
			continue
		}
		cur := "<unknown>"
		if current != nil && current.AverageUtilization != nil {
			cur = fmt.Sprintf("%d%%", *current.AverageUtilization)
			if current.AverageValue != nil {
				cur += fmt.Sprintf(" (%s)", current.AverageValue.String())
			}
		}
		fmt.Fprintf(w, "  %s  (as a percentage of request):\t%s / %s%%\n", label, cur, int32Ptr(target.AverageUtilization))
	}
	fmt.Fprintf(w, "Min replicas:\t%s\n", int32Ptr(h.Spec.MinReplicas))
	fmt.Fprintf(w, "Max replicas:\t%d\n", h.Spec.MaxReplicas)
	if b := h.Spec.Behavior; b != nil {
		fmt.Fprintln(w, "Behavior:")
		for _, r := range []struct {
			name  string
			rules *autoscalingv2.HPAScalingRules
		}{{"Scale Up", b.ScaleUp}, {"Scale Down", b.ScaleDown}} {
			if r.rules == nil {
				continue
			}
			fmt.Fprintf(w, "  %s:\n", r.name)
			fmt.Fprintf(w, "    Stabilization Window: %s seconds\n", int32Ptr(r.rules.StabilizationWindowSeconds))
			fmt.Fprintf(w, "    Select Policy: %s\n", ptr.Deref(r.rules.SelectPolicy, ""))
			fmt.Fprintln(w, "    Policies:")
			for _, p := range r.rules.Policies {
				fmt.Fprintf(w, "      - Type: %s\tValue: %d\tPeriod: %d seconds\n", p.Type, p.Value, p.PeriodSeconds)
			}
		}
	}
	fmt.Fprintf(w, "%s pods:\t%d current / %d desired\n", ref.Kind, h.Status.CurrentReplicas, h.Status.DesiredReplicas)
	if len(h.Status.Conditions) > 0 {
		fmt.Fprintln(w, "Conditions:")
		fmt.Fprintln(w, "  Type\tStatus\tReason\tMessage")
		fmt.Fprintln(w, "  ----\t------\t------\t-------")
		for _, c := range h.Status.Conditions {
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", c.Type, c.Status, c.Reason, c.Message)
		}
	}
}

// autoscale implements `kk autoscale`, like kubectl autoscale: an
// autoscaling/v2 HorizontalPodAutoscaler for a Deployment, ReplicaSet or
// StatefulSet.
func (a *app) autoscale(ctx context.Context, p args) error {
	pos := p.positional
	if len(pos) == 1 && strings.Contains(pos[0], "/") {
		typ, name, _ := strings.Cut(pos[0], "/")
		pos = []string{typ, name}
	}
	if len(pos) != 2 {
		return errors.New(autoscaleUsage)
	}
	res, err := lookupResource(pos[0])
	if err != nil {
		return err
	}
	if res.Resource != scheme.Deployments.Resource && res.Resource != scheme.ReplicaSets.Resource && res.Resource != scheme.StatefulSets.Resource {
		return fmt.Errorf("cannot autoscale a %s", res.Kind)
	}
	maxReplicas, err := strconv.Atoi(p.value("max", "-1"))
	if err != nil {
		return fmt.Errorf("invalid --max %q", p.value("max", ""))
	}
	minReplicas, err := strconv.Atoi(p.value("min", "-1"))
	if err != nil {
		return fmt.Errorf("invalid --min %q", p.value("min", ""))
	}
	if maxReplicas < 1 {
		return fmt.Errorf("--max=MAXPODS is required and must be at least 1, max: %d", maxReplicas)
	}
	if maxReplicas < minReplicas {
		return fmt.Errorf("--max=MAXPODS must be larger or equal to --min=MINPODS, max: %d, min: %d", maxReplicas, minReplicas)
	}
	name := p.value("name", pos[1])
	// The target must exist, as kubectl checks.
	if _, err := a.getObject(ctx, res, a.namespace, pos[1]); err != nil {
		return err
	}
	h := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.namespace},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MaxReplicas: int32(maxReplicas),
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: res.GroupVersion(), Kind: res.Kind, Name: pos[1]}}}
	if minReplicas > 0 {
		h.Spec.MinReplicas = ptr.To(int32(minReplicas))
	}
	target, err := cpuTarget(p.value("cpu-percent", ""), p.value("cpu", ""))
	if err != nil {
		return err
	}
	if target != nil {
		h.Spec.Metrics = []autoscalingv2.MetricSpec{{Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{Name: "cpu", Target: *target}}}
	}
	if _, err := client.HorizontalPodAutoscalers(a.client, a.namespace).Create(ctx, h); err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.out, "horizontalpodautoscaler.autoscaling/%s autoscaled\n", name)
	return err
}

// cpuTarget reads --cpu-percent N or --cpu N% (utilization) or --cpu
// QUANTITY (average usage); none leaves the server's default (80%).
func cpuTarget(percent, cpu string) (*autoscalingv2.MetricTarget, error) {
	switch {
	case percent != "" && cpu != "":
		return nil, errors.New("--cpu-percent and --cpu are mutually exclusive")
	case percent != "":
		cpu = percent + "%"
	case cpu == "":
		return nil, nil
	}
	if n, ok := strings.CutSuffix(cpu, "%"); ok {
		v, err := strconv.Atoi(n)
		if err != nil || v < 1 {
			return nil, fmt.Errorf("invalid CPU target %q", cpu)
		}
		return &autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: ptr.To(int32(v))}, nil
	}
	q, err := resource.ParseQuantity(cpu)
	if err != nil || q.IsZero() {
		return nil, fmt.Errorf("invalid CPU target %q", cpu)
	}
	return &autoscalingv2.MetricTarget{Type: autoscalingv2.AverageValueMetricType, AverageValue: &q}, nil
}
