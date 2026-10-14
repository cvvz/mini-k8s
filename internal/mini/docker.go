package mini

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const dockerAPIVersion = "/v1.41"

var dockerIdentifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// Docker manages only containers labelled for one cluster and node.
type Docker struct {
	client                 *http.Client
	baseURL                string
	cluster, node, network string
	mu                     sync.Mutex
	restarts               map[string]dockerRetry
	pulls                  map[string]dockerRetry
	now                    func() time.Time
}

type dockerRetry struct {
	attempts int
	next     time.Time
	err      error
}

type dockerContainer struct {
	ID     string            `json:"Id"`
	Labels map[string]string `json:"Labels"`
}

type dockerInspection struct {
	ID     string `json:"Id"`
	Config struct {
		Labels map[string]string `json:"Labels"`
		TTY    bool              `json:"Tty"`
	} `json:"Config"`
	State struct {
		Status    string    `json:"Status"`
		Running   bool      `json:"Running"`
		ExitCode  int       `json:"ExitCode"`
		Error     string    `json:"Error"`
		StartedAt time.Time `json:"StartedAt"`
	} `json:"State"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

type dockerAPIError struct {
	code    int
	message string
}

func (e *dockerAPIError) Error() string {
	return fmt.Sprintf("Docker HTTP %d: %s", e.code, e.message)
}

// NewDocker uses the local Engine Unix socket; an empty network selects mini-k8s.
func NewDocker(socket, cluster, node, network string) *Docker {
	socket = strings.TrimPrefix(socket, "unix://")
	if network == "" {
		network = "mini-k8s"
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
	return &Docker{
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("Docker API redirects are not allowed")
			},
		},
		baseURL: "http://docker", cluster: cluster, node: node, network: network,
		restarts: make(map[string]dockerRetry), pulls: make(map[string]dockerRetry), now: time.Now,
	}
}

func (d *Docker) configured() error {
	if d.cluster == "" || d.node == "" {
		return errors.New("Docker requires nonempty cluster and node ownership labels")
	}
	if d.network == "" || d.network == "host" || d.network == "none" || d.network == "bridge" || d.network == "default" || strings.HasPrefix(d.network, "container:") {
		return errors.New("Docker requires a dedicated container network")
	}
	return nil
}

func (d *Docker) owned(labels map[string]string) bool {
	return labels["mini.cluster"] == d.cluster && labels["mini.node"] == d.node && labels["mini.pod"] != ""
}

func (d *Docker) request(ctx context.Context, method, path string, query url.Values, input any) (*http.Response, error) {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return nil, fmt.Errorf("encode Docker request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	endpoint := d.baseURL + dockerAPIVersion + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Docker %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if err != nil {
			return nil, fmt.Errorf("read Docker HTTP %d error: %w", resp.StatusCode, err)
		}
		var problem struct{ Message string }
		message := strings.TrimSpace(string(data))
		if json.Unmarshal(data, &problem) == nil && problem.Message != "" {
			message = problem.Message
		}
		return nil, &dockerAPIError{code: resp.StatusCode, message: message}
	}
	return resp, nil
}

func (d *Docker) call(ctx context.Context, method, path string, query url.Values, input, output any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := d.request(ctx, method, path, query, input)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if output != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(output); err != nil {
			return fmt.Errorf("decode Docker %s: %w", path, err)
		}
		return nil
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return fmt.Errorf("read Docker %s: %w", path, err)
	}
	return nil
}

func dockerHTTPStatus(err error, code int) bool {
	var apiErr *dockerAPIError
	return errors.As(err, &apiErr) && apiErr.code == code
}

func (d *Docker) list(ctx context.Context, pod string) ([]dockerContainer, error) {
	labels := []string{"mini.cluster=" + d.cluster, "mini.node=" + d.node}
	if pod != "" {
		labels = append(labels, "mini.pod="+pod)
	}
	filters, err := json.Marshal(map[string][]string{"label": labels})
	if err != nil {
		return nil, err
	}
	var containers []dockerContainer
	err = d.call(ctx, http.MethodGet, "/containers/json", url.Values{
		"all": {"1"}, "filters": {string(filters)},
	}, nil, &containers)
	return containers, err
}

func (d *Docker) inspectOwned(ctx context.Context, id, pod string) (dockerInspection, error) {
	var result dockerInspection
	if !dockerIdentifier.MatchString(id) {
		return result, errors.New("invalid Docker container ID")
	}
	err := d.call(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, nil, &result)
	if err != nil {
		return result, err
	}
	if !d.owned(result.Config.Labels) || (pod != "" && result.Config.Labels["mini.pod"] != pod) {
		return result, errors.New("container is not owned by this cluster/node/pod")
	}
	if !dockerIdentifier.MatchString(result.ID) {
		return result, errors.New("Docker inspect returned an invalid container ID")
	}
	return result, nil
}

func (d *Docker) postpone(previous dockerRetry, err error) dockerRetry {
	// Saturating the exponent also bounds retries after very long crash loops.
	if previous.attempts < 6 {
		previous.attempts++
	}
	delay := time.Second << (previous.attempts - 1)
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	previous.next, previous.err = d.now().Add(delay), err
	return previous
}

func (d *Docker) ensureImage(ctx context.Context, image string) error {
	var imageInfo struct {
		ID string `json:"Id"`
	}
	err := d.call(ctx, http.MethodGet, "/images/"+url.PathEscape(image)+"/json", nil, nil, &imageInfo)
	if err == nil {
		if imageInfo.ID == "" {
			return errors.New("Docker image inspect returned an empty image ID")
		}
		delete(d.pulls, image)
		return nil
	}
	if !dockerHTTPStatus(err, http.StatusNotFound) {
		return err
	}
	retry := d.pulls[image]
	if d.now().Before(retry.next) {
		return fmt.Errorf("ImagePullBackOff until %s: %w", retry.next.Format(time.RFC3339Nano), retry.err)
	}
	if err := d.pull(ctx, image); err != nil {
		d.pulls[image] = d.postpone(retry, err)
		return err
	}
	delete(d.pulls, image)
	return nil
}

func (d *Docker) pull(ctx context.Context, image string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	resp, err := d.request(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {image}}, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	decoder := json.NewDecoder(resp.Body)
	seen := false
	for {
		var progress struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := decoder.Decode(&progress); err != nil {
			if err == io.EOF && seen {
				return nil
			}
			return fmt.Errorf("decode Docker image pull stream: %w", err)
		}
		seen = true
		if progress.ErrorDetail.Message != "" {
			return fmt.Errorf("Docker image pull: %s", progress.ErrorDetail.Message)
		}
		if progress.Error != "" {
			return fmt.Errorf("Docker image pull: %s", progress.Error)
		}
	}
}

// Ensure reconciles one assigned Pod. Restart and image-pull backoffs never sleep
// in the caller's reconciliation loop; a later Ensure call retries when due.
func (d *Docker) Ensure(ctx context.Context, p Resource) (Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	status := Status{NodeName: p.Status.NodeName, Restarts: p.Status.Restarts, Phase: "Pending"}
	fail := func(reason string, err error) (Status, error) {
		status.Phase, status.PodIP = "Failed", ""
		status.Reason = reason + ": " + err.Error()
		return status, fmt.Errorf("%s: %w", reason, err)
	}
	if err := d.configured(); err != nil {
		return fail("InvalidConfiguration", err)
	}
	if p.Kind != "Pod" || p.Status.NodeName != d.node || !dockerIdentifier.MatchString(p.Metadata.UID) {
		return fail("InvalidPod", errors.New("expected a Pod with a valid UID assigned to this node"))
	}
	var spec PodSpec
	if err := Strict(p.Spec, &spec); err != nil {
		return fail("InvalidPod", err)
	}
	if err := validatePod(spec); err != nil {
		return fail("InvalidPod", err)
	}
	containers, err := d.list(ctx, p.Metadata.UID)
	if err != nil {
		return fail("ContainerListFailed", err)
	}
	id := ""
	for _, container := range containers {
		if !d.owned(container.Labels) || container.Labels["mini.pod"] != p.Metadata.UID {
			continue
		}
		if id != "" {
			return fail("DuplicateContainers", errors.New("multiple owned containers for the same Pod UID"))
		}
		if !dockerIdentifier.MatchString(container.ID) {
			return fail("InvalidContainer", errors.New("Docker list returned an invalid container ID"))
		}
		id = container.ID
	}
	var inspection dockerInspection
	if id == "" {
		container := spec.Containers[0]
		if err := d.ensureImage(ctx, container.Image); err != nil {
			return fail("ImageUnavailable", err)
		}
		payload := struct {
			Image      string            `json:"Image"`
			Cmd        []string          `json:"Cmd,omitempty"`
			Entrypoint []string          `json:"Entrypoint,omitempty"`
			Env        []string          `json:"Env,omitempty"`
			Labels     map[string]string `json:"Labels"`
			HostConfig struct {
				NetworkMode string `json:"NetworkMode"`
			} `json:"HostConfig"`
		}{
			Image: container.Image, Cmd: container.Args, Entrypoint: container.Command,
			Labels: map[string]string{"mini.cluster": d.cluster, "mini.node": d.node, "mini.pod": p.Metadata.UID},
		}
		payload.HostConfig.NetworkMode = d.network
		for _, env := range container.Env {
			payload.Env = append(payload.Env, env.Name+"="+env.Value)
		}
		var created struct {
			ID string `json:"Id"`
		}
		if err := d.call(ctx, http.MethodPost, "/containers/create", url.Values{"name": {"mini-" + p.Metadata.UID}}, payload, &created); err != nil {
			return fail("ContainerCreateFailed", err)
		}
		if !dockerIdentifier.MatchString(created.ID) {
			return fail("ContainerCreateFailed", errors.New("Docker returned an invalid container ID"))
		}
		id = created.ID
		inspection.State.Status = "created"
	} else {
		inspection, err = d.inspectOwned(ctx, id, p.Metadata.UID)
		if err != nil {
			return fail("ContainerInspectFailed", err)
		}
		id = inspection.ID
	}
	status.ContainerID = id
	switch inspection.State.Status {
	case "running":
		if !inspection.State.Running {
			return fail("InvalidContainerState", errors.New("Docker reports running state without Running flag"))
		}
		if !inspection.State.StartedAt.IsZero() && d.now().Sub(inspection.State.StartedAt) >= time.Minute {
			delete(d.restarts, p.Metadata.UID)
		}
	case "created", "exited":
		retry := d.restarts[p.Metadata.UID]
		if d.now().Before(retry.next) {
			status.Reason = fmt.Sprintf("CrashLoopBackOff until %s (exit code %d)", retry.next.Format(time.RFC3339Nano), inspection.State.ExitCode)
			return status, nil
		}
		err := d.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil)
		// Docker uses 304 when another reconciler already started the container.
		if err != nil && !dockerHTTPStatus(err, http.StatusNotModified) {
			d.restarts[p.Metadata.UID] = d.postpone(retry, err)
			return fail("ContainerStartFailed", err)
		}
		d.restarts[p.Metadata.UID] = d.postpone(retry, nil)
		if inspection.State.Status == "exited" && err == nil {
			status.Restarts++
		}
		inspection, err = d.inspectOwned(ctx, id, p.Metadata.UID)
		if err != nil {
			return fail("ContainerInspectFailed", err)
		}
	case "restarting":
		status.Reason = "ContainerRestarting"
		return status, nil
	default:
		return fail("ContainerNotRunnable", fmt.Errorf("state %q: %s", inspection.State.Status, inspection.State.Error))
	}
	if inspection.State.Status != "running" || !inspection.State.Running {
		status.Reason = fmt.Sprintf("ContainerNotRunning: state %s, exit code %d: %s", inspection.State.Status, inspection.State.ExitCode, inspection.State.Error)
		return status, nil
	}
	network, ok := inspection.NetworkSettings.Networks[d.network]
	if !ok || network.IPAddress == "" {
		return fail("ContainerNetworkUnavailable", fmt.Errorf("container has no IP on network %q", d.network))
	}
	status.Phase, status.Reason, status.PodIP = "Running", "ContainerRunning", network.IPAddress
	return status, nil
}

// Cleanup requires the caller's current server list, not a local Pod cache.
// It removes stopped and running orphans only within this node's ownership.
func (d *Docker) Cleanup(ctx context.Context, active []Resource) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.configured(); err != nil {
		return err
	}
	keep := make(map[string]bool)
	for _, pod := range active {
		if pod.Kind == "Pod" && pod.Status.NodeName == d.node && pod.Metadata.UID != "" {
			keep[pod.Metadata.UID] = true
		}
	}
	containers, err := d.list(ctx, "")
	if err != nil {
		return fmt.Errorf("list Docker cleanup candidates: %w", err)
	}
	var failures []error
	for _, container := range containers {
		if !d.owned(container.Labels) || keep[container.Labels["mini.pod"]] {
			continue
		}
		inspected, err := d.inspectOwned(ctx, container.ID, container.Labels["mini.pod"])
		if dockerHTTPStatus(err, http.StatusNotFound) {
			continue // It disappeared after the list; there is nothing to remove.
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("inspect cleanup candidate %s: %w", container.ID, err))
			continue
		}
		err = d.call(ctx, http.MethodDelete, "/containers/"+url.PathEscape(inspected.ID), url.Values{"force": {"1"}}, nil, nil)
		if err != nil && !dockerHTTPStatus(err, http.StatusNotFound) {
			failures = append(failures, fmt.Errorf("remove orphan %s: %w", container.ID, err))
		} else {
			delete(d.restarts, container.Labels["mini.pod"])
		}
	}
	return errors.Join(failures...)
}

// Logs verifies ownership before streaming non-TTY stdout and stderr to w.
func (d *Docker) Logs(ctx context.Context, containerID string, w io.Writer) error {
	if err := d.configured(); err != nil {
		return err
	}
	if w == nil {
		return errors.New("Docker logs requires an output writer")
	}
	inspection, err := d.inspectOwned(ctx, containerID, "")
	if err != nil {
		return fmt.Errorf("inspect logs container: %w", err)
	}
	if inspection.Config.TTY {
		return errors.New("Docker logs requires a non-TTY container")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	resp, err := d.request(ctx, http.MethodGet, "/containers/"+url.PathEscape(inspection.ID)+"/logs",
		url.Values{"stdout": {"1"}, "stderr": {"1"}, "follow": {"0"}}, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var header [8]byte
	for {
		n, err := io.ReadFull(resp.Body, header[:])
		if err == io.EOF && n == 0 {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read Docker log header: %w", err)
		}
		if header[0] != 1 && header[0] != 2 {
			return fmt.Errorf("invalid Docker log stream %d", header[0])
		}
		if header[1] != 0 || header[2] != 0 || header[3] != 0 {
			return errors.New("invalid Docker log header reserved bytes")
		}
		length := int64(binary.BigEndian.Uint32(header[4:]))
		if _, err := io.CopyN(w, resp.Body, length); err != nil {
			return fmt.Errorf("copy Docker log payload: %w", err)
		}
	}
}
