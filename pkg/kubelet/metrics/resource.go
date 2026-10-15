// Package metrics serves the kubelet's resource metrics endpoint,
// /metrics/resource, like k8s.io/kubernetes/pkg/kubelet/metrics/collectors
// (resource_metrics.go): the cumulative CPU time and the memory working set
// of the node, its Pods and their containers, in the Prometheus text
// format metrics-server scrapes.
package metrics

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"mini-k8s/pkg/kubelet/kuberuntime"
)

// NodeStats is the usage of the whole node.
type NodeStats struct {
	CPUSeconds       float64
	MemoryWorkingSet uint64
	Timestamp        time.Time
}

// ReadNodeStats reads the node's usage from its cgroup (v2) directory: the
// node container's own cgroup, which holds every process of the node.
func ReadNodeStats(cgroupDir string, now time.Time) (NodeStats, error) {
	usec, err := readKey(filepath.Join(cgroupDir, "cpu.stat"), "usage_usec")
	if err != nil {
		return NodeStats{}, err
	}
	data, err := os.ReadFile(filepath.Join(cgroupDir, "memory.current"))
	if err != nil {
		return NodeStats{}, err
	}
	current, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return NodeStats{}, err
	}
	inactive, err := readKey(filepath.Join(cgroupDir, "memory.stat"), "inactive_file")
	if err != nil {
		return NodeStats{}, err
	}
	ws := current
	if inactive < ws {
		ws -= inactive
	}
	return NodeStats{CPUSeconds: float64(usec) / 1e6, MemoryWorkingSet: ws, Timestamp: now}, nil
}

// readKey returns the value of a "key value" line of a cgroup file.
func readKey(path, key string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), " "); ok && k == key {
			return strconv.ParseUint(v, 10, 64)
		}
	}
	return 0, fmt.Errorf("%s has no %s", path, key)
}

type family struct{ name, help, kind string }

var (
	containerCPU   = family{"container_cpu_usage_seconds_total", "[STABLE] Cumulative cpu time consumed by the container in core-seconds", "counter"}
	containerMem   = family{"container_memory_working_set_bytes", "[STABLE] Current working set of the container in bytes", "gauge"}
	containerStart = family{"container_start_time_seconds", "[STABLE] Start time of the container since unix epoch in seconds", "gauge"}
	nodeCPU        = family{"node_cpu_usage_seconds_total", "[STABLE] Cumulative cpu time consumed by the node in core-seconds", "counter"}
	nodeMem        = family{"node_memory_working_set_bytes", "[STABLE] Current working set of the node in bytes", "gauge"}
	podCPU         = family{"pod_cpu_usage_seconds_total", "[STABLE] Cumulative cpu time consumed by the pod in core-seconds", "counter"}
	podMem         = family{"pod_memory_working_set_bytes", "[STABLE] Current working set of the pod in bytes", "gauge"}
	scrapeError    = family{"resource_scrape_error", "[STABLE] 1 if there was an error while getting container metrics, 0 otherwise", "gauge"}
)

type sample struct {
	labels string
	value  float64
	at     time.Time
}

// Write renders the resource metrics. A nil node or a container error
// leaves those families out and sets resource_scrape_error.
func Write(w io.Writer, node *NodeStats, containers []kuberuntime.ContainerStats, containerErr error) error {
	sort.Slice(containers, func(i, j int) bool {
		a, b := containers[i], containers[j]
		return a.Namespace+"/"+a.Pod+"/"+a.Container < b.Namespace+"/"+b.Pod+"/"+b.Container
	})
	bw := bufio.NewWriter(w)
	emit := func(f family, samples []sample) {
		if len(samples) == 0 {
			return
		}
		fmt.Fprintf(bw, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.kind)
		for _, s := range samples {
			fmt.Fprintf(bw, "%s%s %s", f.name, s.labels, strconv.FormatFloat(s.value, 'g', -1, 64))
			if !s.at.IsZero() {
				fmt.Fprintf(bw, " %d", s.at.UnixMilli())
			}
			bw.WriteByte('\n')
		}
	}
	var cpu, mem, start []sample
	type podKey struct{ ns, name string }
	podCPUs, podMems, podAt := map[podKey]float64{}, map[podKey]float64{}, map[podKey]time.Time{}
	var pods []podKey
	for _, c := range containers {
		labels := fmt.Sprintf(`{container=%q,namespace=%q,pod=%q}`, c.Container, c.Namespace, c.Pod)
		cpu = append(cpu, sample{labels, c.CPUSeconds, c.Timestamp})
		mem = append(mem, sample{labels, float64(c.MemoryWorkingSet), c.Timestamp})
		start = append(start, sample{labels, float64(c.StartTime.UnixNano()) / 1e9, c.Timestamp})
		k := podKey{c.Namespace, c.Pod}
		if _, ok := podAt[k]; !ok {
			pods = append(pods, k)
		}
		podCPUs[k] += c.CPUSeconds
		podMems[k] += float64(c.MemoryWorkingSet)
		if c.Timestamp.After(podAt[k]) {
			podAt[k] = c.Timestamp
		}
	}
	emit(containerCPU, cpu)
	emit(containerMem, mem)
	emit(containerStart, start)
	if node != nil {
		emit(nodeCPU, []sample{{"", node.CPUSeconds, node.Timestamp}})
		emit(nodeMem, []sample{{"", float64(node.MemoryWorkingSet), node.Timestamp}})
	}
	var pcpu, pmem []sample
	for _, k := range pods {
		labels := fmt.Sprintf(`{namespace=%q,pod=%q}`, k.ns, k.name)
		pcpu = append(pcpu, sample{labels, podCPUs[k], podAt[k]})
		pmem = append(pmem, sample{labels, podMems[k], podAt[k]})
	}
	emit(podCPU, pcpu)
	emit(podMem, pmem)
	failed := 0.0
	if containerErr != nil || node == nil {
		failed = 1
	}
	emit(scrapeError, []sample{{"", failed, time.Time{}}})
	return bw.Flush()
}
