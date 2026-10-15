package metrics

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mini-k8s/pkg/kubelet/kuberuntime"
)

func TestReadNodeStats(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"cpu.stat":       "usage_usec 29297446\nuser_usec 11914078\n",
		"memory.current": "169103360\n",
		"memory.stat":    "anon 1\nactive_file 28311552\ninactive_file 49750016\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Unix(1792044695, 0)
	st, err := ReadNodeStats(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if st.CPUSeconds != 29.297446 || st.MemoryWorkingSet != 169103360-49750016 || !st.Timestamp.Equal(now) {
		t.Fatalf("stats: %+v", st)
	}
	if _, err := ReadNodeStats(t.TempDir(), now); err == nil {
		t.Fatal("missing files are an error")
	}
}

func TestWrite(t *testing.T) {
	at := time.UnixMilli(1792044695747)
	node := &NodeStats{CPUSeconds: 357.35, MemoryWorkingSet: 1400000000, Timestamp: at}
	containers := []kuberuntime.ContainerStats{
		{Namespace: "ns", Pod: "web", Container: "sidecar", CPUSeconds: 0.5, MemoryWorkingSet: 1024, StartTime: time.Unix(1792000000, 0), Timestamp: at},
		{Namespace: "ns", Pod: "web", Container: "app", CPUSeconds: 3.851144, MemoryWorkingSet: 2048, StartTime: time.Unix(1792000000, 0), Timestamp: at},
	}
	var b bytes.Buffer
	if err := Write(&b, node, containers, nil); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"# HELP container_cpu_usage_seconds_total [STABLE] Cumulative cpu time consumed by the container in core-seconds\n" +
			"# TYPE container_cpu_usage_seconds_total counter\n" +
			`container_cpu_usage_seconds_total{container="app",namespace="ns",pod="web"} 3.851144 1792044695747` + "\n" +
			`container_cpu_usage_seconds_total{container="sidecar",namespace="ns",pod="web"} 0.5 1792044695747` + "\n",
		`container_memory_working_set_bytes{container="app",namespace="ns",pod="web"} 2048 1792044695747`,
		`container_start_time_seconds{container="app",namespace="ns",pod="web"} 1.792e+09 1792044695747`,
		"node_cpu_usage_seconds_total 357.35 1792044695747\n",
		"node_memory_working_set_bytes 1.4e+09 1792044695747\n",
		`pod_cpu_usage_seconds_total{namespace="ns",pod="web"} 4.351144 1792044695747`,
		`pod_memory_working_set_bytes{namespace="ns",pod="web"} 3072 1792044695747`,
		"resource_scrape_error 0\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	b.Reset()
	_ = Write(&b, nil, nil, errors.New("runtime down"))
	if out := b.String(); !strings.Contains(out, "resource_scrape_error 1\n") || strings.Contains(out, "node_cpu") {
		t.Fatalf("on errors:\n%s", out)
	}
}
