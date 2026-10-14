package mini

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func Kind(s string) (string, error) {
	switch strings.ToLower(s) {
	case "pod", "pods", "po":
		return "Pod", nil
	case "deployment", "deployments", "deploy":
		return "Deployment", nil
	case "node", "nodes", "no":
		return "Node", nil
	case "service", "services", "svc":
		return "Service", nil
	}
	return "", fmt.Errorf("unsupported resource %q", s)
}
func authenticate(w http.ResponseWriter, r *http.Request, token string) bool {
	if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
		http.Error(w, "unauthorized", 401)
		return false
	}
	return true
}
func reply(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		code := 500
		if errors.Is(err, ErrNotFound) {
			code = 404
		}
		if errors.Is(err, ErrConflict) {
			code = 409
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

// serviceSingletonError explains the one-Service limit instead of surfacing the
// version conflict produced by the shared singleton storage key.
func serviceSingletonError(existing, requested string) error {
	if existing == "" || existing == requested {
		return nil
	}
	return fmt.Errorf("%w: only one Service is supported; Service %q already exists (requested %q)", ErrConflict, existing, requested)
}
func APIHandler(store *Store, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authenticate(w, r, token) {
			return
		}
		if r.URL.Path == "/healthz" {
			_, err := store.List(r.Context(), "Node")
			reply(w, map[string]string{"status": "ok"}, err)
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 2 || len(parts) > 4 || parts[0] != "api" {
			http.NotFound(w, r)
			return
		}
		kind, err := Kind(parts[1])
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		name := ""
		if len(parts) >= 3 {
			name = parts[2]
			if !validName.MatchString(name) {
				http.Error(w, "invalid name", 400)
				return
			}
		}
		if r.Method == "GET" {
			if name == "" {
				items, err := store.List(r.Context(), kind)
				reply(w, items, err)
				return
			}
			obj, err := store.Get(r.Context(), kind, name)
			if err != nil {
				reply(w, nil, err)
				return
			}
			if len(parts) == 4 {
				if parts[3] != "logs" || kind != "Pod" {
					http.NotFound(w, r)
					return
				}
				if obj.Status.NodeName == "" {
					http.Error(w, "Pod is not scheduled", 409)
					return
				}
				url := "http://" + obj.Status.NodeName + ":8080/logs/" + name
				req, err := http.NewRequestWithContext(r.Context(), "GET", url, nil)
				if err != nil {
					reply(w, nil, err)
					return
				}
				req.Header.Set("Authorization", "Bearer "+token)
				resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
				if err != nil {
					reply(w, nil, err)
					return
				}
				defer resp.Body.Close()
				w.WriteHeader(resp.StatusCode)
				_, _ = io.Copy(w, resp.Body)
				return
			}
			reply(w, obj, nil)
			return
		}
		if r.Method == "DELETE" && name != "" && len(parts) == 3 {
			old, err := store.Get(r.Context(), kind, name)
			if err == nil {
				if rv := r.URL.Query().Get("version"); rv == "" || rv != fmt.Sprint(old.Metadata.ResourceVersion) {
					err = ErrConflict
				} else {
					_, err = store.Write(r.Context(), old, old.Metadata.ResourceVersion, true)
				}
			}
			reply(w, map[string]string{"deleted": name}, err)
			return
		}
		if r.Method != "POST" && r.Method != "PUT" {
			http.Error(w, "method not allowed", 405)
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		obj, err := Decode(data)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if obj.Kind != kind || (name != "" && name != obj.Metadata.Name) {
			http.Error(w, "resource URL and body disagree", 400)
			return
		}
		if r.Method == "POST" && name == "" {
			if kind == "Service" {
				existing, err := store.List(r.Context(), kind)
				if err != nil {
					reply(w, nil, err)
					return
				}
				if len(existing) > 0 {
					if err := serviceSingletonError(existing[0].Metadata.Name, obj.Metadata.Name); err != nil {
						reply(w, nil, err)
						return
					}
				}
			}
			obj.Metadata.UID = UID()
			obj.Metadata.ResourceVersion = 0
			obj.Status = Status{}
			if kind == "Pod" {
				obj.Status.Phase = "Pending"
			}
			created, err := store.Write(r.Context(), obj, 0, false)
			reply(w, created, err)
			return
		}
		if r.Method != "PUT" || name == "" {
			http.Error(w, "invalid operation", 400)
			return
		}
		old, err := store.Get(r.Context(), kind, name)
		if err != nil {
			reply(w, nil, err)
			return
		}
		if old.Metadata.ResourceVersion != obj.Metadata.ResourceVersion || old.Metadata.UID != obj.Metadata.UID {
			reply(w, nil, ErrConflict)
			return
		}
		if len(parts) == 4 {
			if parts[3] != "status" {
				http.NotFound(w, r)
				return
			}
			if old.Status.NodeName != "" && old.Status.NodeName != obj.Status.NodeName {
				http.Error(w, "Pod assignment is immutable", 400)
				return
			}
			old.Status = obj.Status
		} else {
			if err := CheckUpdate(old, obj); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			old.Spec = obj.Spec
		}
		updated, err := store.Write(r.Context(), old, old.Metadata.ResourceVersion, false)
		reply(w, updated, err)
	})
}

type Client struct {
	URL, Token string
	HTTP       *http.Client
}

func NewClient(url, token string) *Client {
	return &Client{strings.TrimRight(url, "/"), token, &http.Client{Timeout: 20 * time.Second}}
}
func (c *Client) request(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == 404 {
			return fmt.Errorf("%w: %s", ErrNotFound, b)
		}
		if resp.StatusCode == 409 {
			return fmt.Errorf("%w: %s", ErrConflict, b)
		}
		return fmt.Errorf("%s: %s", resp.Status, b)
	}
	if out == nil {
		return nil
	}
	if writer, ok := out.(io.Writer); ok {
		_, err = io.Copy(writer, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
func (c *Client) List(ctx context.Context, kind string) ([]Resource, error) {
	var out []Resource
	err := c.request(ctx, "GET", "/api/"+kind, nil, &out)
	return out, err
}
func (c *Client) Get(ctx context.Context, kind, name string) (Resource, error) {
	var out Resource
	err := c.request(ctx, "GET", "/api/"+kind+"/"+name, nil, &out)
	return out, err
}
func (c *Client) Create(ctx context.Context, r Resource) (Resource, error) {
	var out Resource
	err := c.request(ctx, "POST", "/api/"+r.Kind, r, &out)
	return out, err
}
func (c *Client) Update(ctx context.Context, r Resource, status bool) error {
	path := "/api/" + r.Kind + "/" + r.Metadata.Name
	if status {
		path += "/status"
	}
	return c.request(ctx, "PUT", path, r, nil)
}
func (c *Client) Delete(ctx context.Context, r Resource) error {
	return c.request(ctx, "DELETE", fmt.Sprintf("/api/%s/%s?version=%d", r.Kind, r.Metadata.Name, r.Metadata.ResourceVersion), nil, nil)
}
func (c *Client) Logs(ctx context.Context, name string, w io.Writer) error {
	return c.request(ctx, "GET", "/api/Pod/"+name+"/logs", nil, w)
}
func (c *Client) Health(ctx context.Context) error {
	return c.request(ctx, "GET", "/healthz", nil, nil)
}
