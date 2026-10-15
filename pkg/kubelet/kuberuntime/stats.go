package kuberuntime

import (
	"context"
	"time"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// ContainerStats is the resource usage of a running container, as the CRI
// runtime measures it.
type ContainerStats struct {
	Namespace, Pod, Container string
	// CPUSeconds is the cumulative CPU time in core-seconds.
	CPUSeconds float64
	// MemoryWorkingSet is the memory in use, without inactive file pages.
	MemoryWorkingSet uint64
	StartTime        time.Time
	Timestamp        time.Time
}

// ContainerStats lists the usage of the running containers of Pods.
func (m *Manager) ContainerStats(ctx context.Context) ([]ContainerStats, error) {
	running, err := m.runtime.ListContainers(ctx, &runtimeapi.ListContainersRequest{Filter: &runtimeapi.ContainerFilter{
		State: &runtimeapi.ContainerStateValue{State: runtimeapi.ContainerState_CONTAINER_RUNNING}}})
	if err != nil {
		return nil, err
	}
	byID := map[string]*runtimeapi.Container{}
	for _, c := range running.Containers {
		byID[c.Id] = c
	}
	resp, err := m.runtime.ListContainerStats(ctx, &runtimeapi.ListContainerStatsRequest{})
	if err != nil {
		return nil, err
	}
	var out []ContainerStats
	for _, s := range resp.Stats {
		if s.Attributes == nil || s.Cpu == nil || s.Memory == nil {
			continue
		}
		c := byID[s.Attributes.Id]
		if c == nil || c.Labels[LabelPodName] == "" || s.Cpu.UsageCoreNanoSeconds == nil {
			continue
		}
		st := ContainerStats{Namespace: c.Labels[LabelPodNamespace], Pod: c.Labels[LabelPodName], Container: c.Labels[LabelContainerName],
			CPUSeconds: float64(s.Cpu.UsageCoreNanoSeconds.Value) / 1e9, StartTime: time.Unix(0, c.CreatedAt),
			Timestamp: time.Unix(0, s.Cpu.Timestamp)}
		if s.Memory.WorkingSetBytes != nil {
			st.MemoryWorkingSet = s.Memory.WorkingSetBytes.Value
		}
		if s.Cpu.Timestamp == 0 {
			st.Timestamp = time.Now()
		}
		out = append(out, st)
	}
	return out, nil
}
