// Package client is the Go client for the mini-k8s API: a REST client that
// turns Status responses into typed errors, generic per-resource clients and
// watch streams.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/scheme"
)

// Client talks to one API server with a bearer token.
type Client struct {
	base   string
	token  string
	http   *http.Client
	stream *http.Client
}

// New returns a client for base (e.g. http://127.0.0.1:18000).
func New(base, token string) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"), token: token,
		http:   &http.Client{Timeout: 30 * time.Second},
		stream: &http.Client{},
	}
}

// APIError is a non-2xx response carrying a Status.
type APIError struct{ Status metav1.Status }

func (e *APIError) Error() string {
	if e.Status.Message != "" {
		return e.Status.Message
	}
	return fmt.Sprintf("server returned %d %s", e.Status.Code, e.Status.Reason)
}

// Reason returns the StatusReason of an API error, or "".
func Reason(err error) metav1.StatusReason {
	var e *APIError
	if errors.As(err, &e) {
		return e.Status.Reason
	}
	return ""
}

// IsNotFound reports a 404 NotFound error.
func IsNotFound(err error) bool { return Reason(err) == metav1.ReasonNotFound }

// IsConflict reports a resourceVersion or precondition conflict.
func IsConflict(err error) bool { return Reason(err) == metav1.ReasonConflict }

// IsAlreadyExists reports a create of an existing name.
func IsAlreadyExists(err error) bool { return Reason(err) == metav1.ReasonAlreadyExists }

// IsInvalid reports a validation failure.
func IsInvalid(err error) bool { return Reason(err) == metav1.ReasonInvalid }

// IsForbidden reports a request refused by admission.
func IsForbidden(err error) bool { return Reason(err) == metav1.ReasonForbidden }

// IsExpired reports a watch resourceVersion that is too old.
func IsExpired(err error) bool { return Reason(err) == metav1.ReasonExpired }

// Request performs a request and returns the response for 2xx statuses.
// Callers must close the body.
func (c *Client) Request(ctx context.Context, method, path string, query url.Values, contentType string, body []byte, streaming bool) (*http.Response, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	hc := c.http
	if streaming {
		hc = c.stream
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	apiErr := &APIError{}
	if json.Unmarshal(data, &apiErr.Status) != nil || apiErr.Status.Kind != "Status" {
		apiErr.Status = metav1.Status{Code: int32(resp.StatusCode), Message: strings.TrimSpace(string(data))}
	}
	if apiErr.Status.Code == 0 {
		apiErr.Status.Code = int32(resp.StatusCode)
	}
	return nil, apiErr
}

// Do sends in (marshalled unless nil or already []byte) and decodes the
// response into out when out is not nil.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, contentType string, in, out any) error {
	var body []byte
	switch v := in.(type) {
	case nil:
	case []byte:
		body = v
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		body = data
	}
	if body != nil && contentType == "" {
		contentType = "application/json"
	}
	resp, err := c.Request(ctx, method, path, query, contentType, body, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	if raw, ok := out.(*[]byte); ok {
		*raw, err = io.ReadAll(resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Health returns nil when /healthz reports ok.
func (c *Client) Health(ctx context.Context) error {
	return c.Do(ctx, http.MethodGet, "/healthz", nil, "", nil, nil)
}

// ListOptions narrows list and watch requests.
type ListOptions struct {
	LabelSelector   string
	FieldSelector   string
	ResourceVersion string
	TimeoutSeconds  int
}

// Query renders the options as URL parameters.
func (o ListOptions) Query() url.Values {
	q := url.Values{}
	if o.LabelSelector != "" {
		q.Set("labelSelector", o.LabelSelector)
	}
	if o.FieldSelector != "" {
		q.Set("fieldSelector", o.FieldSelector)
	}
	if o.ResourceVersion != "" {
		q.Set("resourceVersion", o.ResourceVersion)
	}
	if o.TimeoutSeconds > 0 {
		q.Set("timeoutSeconds", fmt.Sprint(o.TimeoutSeconds))
	}
	return q
}

// LogOptions selects a container log stream.
type LogOptions struct {
	Container string
	Follow    bool
	TailLines int
	Previous  bool
}

// PodLogs opens the log stream of a Pod container.
func (c *Client) PodLogs(ctx context.Context, namespace, name string, o LogOptions) (io.ReadCloser, error) {
	q := url.Values{}
	if o.Container != "" {
		q.Set("container", o.Container)
	}
	if o.Follow {
		q.Set("follow", "true")
	}
	if o.TailLines > 0 {
		q.Set("tailLines", fmt.Sprint(o.TailLines))
	}
	if o.Previous {
		q.Set("previous", "true")
	}
	resp, err := c.Request(ctx, http.MethodGet, scheme.Pods.Path(namespace, name, "log"), q, "", nil, true)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// ExecOptions selects the container and the command of an exec request.
type ExecOptions struct {
	Container string
	Command   []string
	// Timeout bounds the command's run time on the node (0: the kubelet's default).
	Timeout time.Duration
}

// PodExec runs a command to completion in a Pod container.
func (c *Client) PodExec(ctx context.Context, namespace, name string, o ExecOptions) (*corev1.PodExecResult, error) {
	q := url.Values{"command": o.Command}
	if o.Container != "" {
		q.Set("container", o.Container)
	}
	if o.Timeout > 0 {
		q.Set("timeout", fmt.Sprint(int(o.Timeout/time.Second)))
	}
	// The command may run longer than the regular request timeout.
	resp, err := c.Request(ctx, http.MethodPost, scheme.Pods.Path(namespace, name, "exec"), q, "", nil, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var res corev1.PodExecResult
	return &res, json.NewDecoder(resp.Body).Decode(&res)
}

// BindPod assigns a Pod to a node through the binding subresource.
func (c *Client) BindPod(ctx context.Context, namespace, name, node string) error {
	b := corev1.Binding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Binding"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Target:     corev1.ObjectReference{Kind: "Node", Name: node},
	}
	return c.Do(ctx, http.MethodPost, scheme.Pods.Path(namespace, name, "binding"), nil, "", b, nil)
}
