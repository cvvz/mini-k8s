package dockershim

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
	"time"
)

// dockerAPIVersion pins the Engine API so behaviour does not drift with the daemon.
const dockerAPIVersion = "/v1.41"

var dockerIdentifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// dockerClient is a minimal Docker Engine API client over a Unix socket.
type dockerClient struct {
	http    *http.Client
	baseURL string
}

func newDockerClient(socket string) *dockerClient {
	socket = strings.TrimPrefix(socket, "unix://")
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &dockerClient{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", socket)
				},
				IdleConnTimeout: 90 * time.Second,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("Docker API redirects are not allowed")
			},
		},
		baseURL: "http://docker",
	}
}

type dockerAPIError struct {
	code    int
	message string
}

func (e *dockerAPIError) Error() string { return fmt.Sprintf("Docker HTTP %d: %s", e.code, e.message) }

func dockerStatus(err error, code int) bool {
	var apiErr *dockerAPIError
	return errors.As(err, &apiErr) && apiErr.code == code
}

func (d *dockerClient) request(ctx context.Context, method, path string, query url.Values, input any) (*http.Response, error) {
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
	resp, err := d.http.Do(req)
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

// call performs a bounded request and decodes JSON into output when non-nil.
func (d *dockerClient) call(ctx context.Context, method, path string, query url.Values, input, output any) error {
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
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

type dockerContainer struct {
	ID     string            `json:"Id"`
	Labels map[string]string `json:"Labels"`
	State  string            `json:"State"`
}

type dockerInspection struct {
	ID     string `json:"Id"`
	Image  string `json:"Image"`
	Config struct {
		Labels map[string]string `json:"Labels"`
		TTY    bool              `json:"Tty"`
	} `json:"Config"`
	State struct {
		Status     string    `json:"Status"`
		Running    bool      `json:"Running"`
		OOMKilled  bool      `json:"OOMKilled"`
		ExitCode   int       `json:"ExitCode"`
		Error      string    `json:"Error"`
		StartedAt  time.Time `json:"StartedAt"`
		FinishedAt time.Time `json:"FinishedAt"`
	} `json:"State"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// list returns containers (including stopped ones) carrying all labels.
func (d *dockerClient) list(ctx context.Context, labels ...string) ([]dockerContainer, error) {
	filters, err := json.Marshal(map[string][]string{"label": labels})
	if err != nil {
		return nil, err
	}
	var containers []dockerContainer
	err = d.call(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"1"}, "filters": {string(filters)}}, nil, &containers)
	return containers, err
}

func (d *dockerClient) inspect(ctx context.Context, id string) (dockerInspection, error) {
	var result dockerInspection
	if !dockerIdentifier.MatchString(id) {
		return result, errors.New("invalid Docker container ID")
	}
	err := d.call(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, nil, &result)
	return result, err
}

func (d *dockerClient) imageExists(ctx context.Context, image string) (bool, error) {
	var info struct {
		ID string `json:"Id"`
	}
	err := d.call(ctx, http.MethodGet, "/images/"+url.PathEscape(image)+"/json", nil, nil, &info)
	if dockerStatus(err, http.StatusNotFound) {
		return false, nil
	}
	if err == nil && info.ID == "" {
		return false, errors.New("Docker image inspect returned an empty image ID")
	}
	return err == nil, err
}

// pull fetches an image, surfacing errors reported inside the progress stream.
func (d *dockerClient) pull(ctx context.Context, image string) error {
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

func (d *dockerClient) create(ctx context.Context, name string, config any) (string, error) {
	var created struct {
		ID string `json:"Id"`
	}
	if err := d.call(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, config, &created); err != nil {
		return "", err
	}
	if !dockerIdentifier.MatchString(created.ID) {
		return "", errors.New("Docker returned an invalid container ID")
	}
	return created.ID, nil
}

func (d *dockerClient) start(ctx context.Context, id string) error {
	err := d.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil)
	// 304 means another reconciler already started it.
	if dockerStatus(err, http.StatusNotModified) {
		return nil
	}
	return err
}

// stop sends SIGTERM and, after grace seconds, SIGKILL.
func (d *dockerClient) stop(ctx context.Context, id string, grace int64) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(grace)*time.Second+30*time.Second)
	defer cancel()
	resp, err := d.request(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop", url.Values{"t": {fmt.Sprint(grace)}}, nil)
	if dockerStatus(err, http.StatusNotModified) || dockerStatus(err, http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (d *dockerClient) remove(ctx context.Context, id string) error {
	err := d.call(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), url.Values{"force": {"1"}}, nil, nil)
	if dockerStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

func (d *dockerClient) version(ctx context.Context) (string, error) {
	var v struct{ Version string }
	err := d.call(ctx, http.MethodGet, "/version", nil, nil, &v)
	return v.Version, err
}

// logs streams demultiplexed stdout/stderr of a non-TTY container to w,
// flushing after every frame when w supports it.
func (d *dockerClient) logs(ctx context.Context, id string, follow bool, tail int, w io.Writer) error {
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "follow": {"0"}}
	if follow {
		q.Set("follow", "1")
	}
	if tail > 0 {
		q.Set("tail", fmt.Sprint(tail))
	}
	resp, err := d.request(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs", q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	flusher, _ := w.(http.Flusher)
	var header [8]byte
	for {
		n, err := io.ReadFull(resp.Body, header[:])
		if err == io.EOF && n == 0 {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read Docker log header: %w", err)
		}
		if header[0] != 1 && header[0] != 2 {
			return fmt.Errorf("invalid Docker log stream %d", header[0])
		}
		if header[1] != 0 || header[2] != 0 || header[3] != 0 {
			return errors.New("invalid Docker log header reserved bytes")
		}
		if _, err := io.CopyN(w, resp.Body, int64(binary.BigEndian.Uint32(header[4:]))); err != nil {
			return fmt.Errorf("copy Docker log payload: %w", err)
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}
