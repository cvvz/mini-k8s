package kuberuntime

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	corev1 "mini-k8s/pkg/apis/core/v1"
)

// LogOptions selects which log to read and how.
type LogOptions struct {
	Follow    bool
	TailLines int
	Previous  bool
}

// logAttempts returns the attempts that have log files, highest first.
func (m *Manager) logAttempts(pod *corev1.Pod, container string) ([]int, error) {
	entries, err := os.ReadDir(filepath.Join(m.PodLogDir(pod), container))
	if err != nil {
		return nil, err
	}
	var attempts []int
	for _, e := range entries {
		if n, err := strconv.Atoi(strings.TrimSuffix(e.Name(), ".log")); err == nil && strings.HasSuffix(e.Name(), ".log") {
			attempts = append(attempts, n)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(attempts)))
	return attempts, nil
}

// Logs writes a container's log to w. containerd writes logs in the CRI
// format ("<time> <stream> <P|F> <text>") to one file per container
// instance; Previous selects the instance before the latest one.
func (m *Manager) Logs(ctx context.Context, pod *corev1.Pod, container string, o LogOptions, w io.Writer) error {
	attempts, err := m.logAttempts(pod, container)
	if err != nil || len(attempts) == 0 {
		return fmt.Errorf("no logs for container %q of pod %s/%s", container, pod.Namespace, pod.Name)
	}
	idx := 0
	if o.Previous {
		if len(attempts) < 2 {
			return fmt.Errorf("previous terminated container %q of pod %s/%s not found", container, pod.Namespace, pod.Name)
		}
		idx = 1
	}
	path := filepath.Join(m.PodLogDir(pod), container, fmt.Sprintf("%d.log", attempts[idx]))
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	var tail []string
	partial := "" // an unterminated last line, completed while following
	for {
		line, err := reader.ReadString('\n')
		if err == io.EOF {
			partial = line
			break
		}
		if err != nil {
			return err
		}
		text := parseCRILine(line)
		if o.TailLines > 0 {
			tail = append(tail, text)
			if len(tail) > o.TailLines {
				tail = tail[1:]
			}
			continue
		}
		if _, err := io.WriteString(w, text); err != nil {
			return err
		}
	}
	for _, text := range tail {
		if _, err := io.WriteString(w, text); err != nil {
			return err
		}
	}
	flush(w)
	if !o.Follow || o.Previous {
		return nil
	}
	return m.follow(ctx, pod, container, reader, partial, w)
}

// follow streams lines appended to the file behind reader until the instance
// stops running or ctx ends. bufio.Reader retries the file after an EOF, so
// new data is picked up on the next poll.
func (m *Manager) follow(ctx context.Context, pod *corev1.Pod, container string, reader *bufio.Reader, pending string, w io.Writer) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		// Check before draining, so lines written just before the container
		// exited are still delivered.
		alive := m.running(ctx, pod.UID, container)
		for {
			chunk, err := reader.ReadString('\n')
			pending += chunk
			if err != nil {
				break
			}
			if _, err := io.WriteString(w, parseCRILine(pending)); err != nil {
				return err
			}
			pending = ""
		}
		flush(w)
		if !alive {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (m *Manager) running(ctx context.Context, uid, name string) bool {
	instances, err := m.containers(ctx, uid, name)
	return err == nil && len(instances) > 0 && instances[0].State == runtimeapi.ContainerState_CONTAINER_RUNNING
}

// parseCRILine turns "2024-01-01T00:00:00.1Z stdout F hello\n" into "hello\n".
// Partial lines (tag P) are emitted without a newline.
func parseCRILine(line string) string {
	line = strings.TrimSuffix(line, "\n")
	parts := strings.SplitN(line, " ", 4)
	if len(parts) < 3 {
		return line + "\n"
	}
	text := ""
	if len(parts) == 4 {
		text = parts[3]
	}
	if parts[2] == "P" {
		return text
	}
	return text + "\n"
}

func flush(w io.Writer) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
