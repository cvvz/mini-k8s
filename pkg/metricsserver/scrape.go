// Package metricsserver is a small metrics-server (like
// sigs.k8s.io/metrics-server): it scrapes the resource metrics of every
// kubelet, keeps the last two samples of each node and container, and
// serves their CPU rate and memory working set as the metrics.k8s.io API,
// which the API server proxies to it.
package metricsserver

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// point is one sample: cumulative CPU seconds and the memory working set.
type point struct {
	cpu   float64
	mem   uint64
	at    time.Time
	start time.Time
}

type podKey struct{ namespace, name string }

// batch is the result of scraping one or more kubelets.
type batch struct {
	nodes map[string]point
	pods  map[podKey]map[string]point // container name to sample
}

func newBatch() *batch { return &batch{nodes: map[string]point{}, pods: map[podKey]map[string]point{}} }

// parse reads a kubelet's /metrics/resource for the given node.
func parse(r io.Reader, node string, into *batch) error {
	containers := map[string]*point{}
	key := func(labels map[string]string) string {
		return labels["namespace"] + "/" + labels["pod"] + "/" + labels["container"]
	}
	get := func(labels map[string]string) *point {
		k := key(labels)
		if containers[k] == nil {
			containers[k] = &point{}
		}
		return containers[k]
	}
	var nodePoint point
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, value, at, err := parseSample(line)
		if err != nil {
			return err
		}
		switch name {
		case "node_cpu_usage_seconds_total":
			nodePoint.cpu, nodePoint.at = value, at
		case "node_memory_working_set_bytes":
			nodePoint.mem = uint64(value)
		case "container_cpu_usage_seconds_total":
			p := get(labels)
			p.cpu, p.at = value, at
		case "container_memory_working_set_bytes":
			get(labels).mem = uint64(value)
		case "container_start_time_seconds":
			get(labels).start = time.Unix(0, int64(value*1e9)).Truncate(time.Second)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if !nodePoint.at.IsZero() {
		into.nodes[node] = nodePoint
	}
	for k, p := range containers {
		parts := strings.SplitN(k, "/", 3)
		if p.at.IsZero() || len(parts) != 3 {
			continue
		}
		pk := podKey{parts[0], parts[1]}
		if into.pods[pk] == nil {
			into.pods[pk] = map[string]point{}
		}
		into.pods[pk][parts[2]] = *p
	}
	return nil
}

// parseSample splits `name{k="v",...} value timestamp_ms`.
func parseSample(line string) (name string, labels map[string]string, value float64, at time.Time, err error) {
	labels = map[string]string{}
	rest := line
	if i := strings.IndexByte(line, '{'); i >= 0 {
		j := strings.LastIndexByte(line, '}')
		if j < i {
			return "", nil, 0, time.Time{}, fmt.Errorf("bad sample %q", line)
		}
		name, rest = line[:i], strings.TrimSpace(line[j+1:])
		for _, pair := range splitLabels(line[i+1 : j]) {
			k, v, ok := strings.Cut(pair, "=")
			if !ok {
				return "", nil, 0, time.Time{}, fmt.Errorf("bad label %q", pair)
			}
			if uq, err := strconv.Unquote(v); err == nil {
				v = uq
			}
			labels[strings.TrimSpace(k)] = v
		}
	} else {
		name, rest, _ = strings.Cut(line, " ")
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", nil, 0, time.Time{}, fmt.Errorf("bad sample %q", line)
	}
	if value, err = strconv.ParseFloat(fields[0], 64); err != nil {
		return "", nil, 0, time.Time{}, err
	}
	if len(fields) > 1 {
		ms, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return "", nil, 0, time.Time{}, err
		}
		at = time.UnixMilli(ms)
	}
	return name, labels, value, at, nil
}

// splitLabels splits `a="x",b="y,z"` at the commas between labels.
func splitLabels(s string) []string {
	var out []string
	quoted, start := false, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			quoted = !quoted
		case ',':
			if !quoted {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	if strings.TrimSpace(s[start:]) != "" {
		out = append(out, s[start:])
	}
	return out
}

// scraper fetches /metrics/resource from kubelets.
type scraper struct {
	http        *http.Client
	token       string
	kubeletPort int
}

func (s *scraper) scrape(ctx context.Context, node, address string, into *batch) error {
	url := "http://" + net.JoinHostPort(address, strconv.Itoa(s.kubeletPort)) + "/metrics/resource"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return parse(resp.Body, node, into)
}
