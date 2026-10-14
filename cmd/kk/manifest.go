package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"mini-k8s/pkg/client"
	"mini-k8s/pkg/scheme"
)

// document is one resource read from a manifest.
type document struct {
	source    string
	res       scheme.ResourceInfo
	namespace string
	name      string
	body      map[string]any
}

func (d document) ref() string { return qualifiedName(d.res) + "/" + d.name }

// serverManaged lists metadata fields users may not set in manifests.
var serverManaged = []string{"uid", "resourceVersion", "generation", "creationTimestamp", "deletionTimestamp", "deletionGracePeriodSeconds"}

// readDocuments loads every document from files, directories (*.yaml,
// *.yml, *.json) and "-" (stdin), validating its shape.
func (a *app) readDocuments(paths []string) ([]document, error) {
	var docs []document
	for _, p := range paths {
		files := []string{p}
		if p != "-" {
			info, err := os.Stat(p)
			if err != nil {
				return nil, err
			}
			if info.IsDir() {
				files = nil
				entries, err := os.ReadDir(p)
				if err != nil {
					return nil, err
				}
				for _, e := range entries {
					ext := strings.ToLower(filepath.Ext(e.Name()))
					if !e.IsDir() && (ext == ".yaml" || ext == ".yml" || ext == ".json") {
						files = append(files, filepath.Join(p, e.Name()))
					}
				}
				sort.Strings(files)
			}
		}
		for _, f := range files {
			var r io.Reader = a.in
			if f != "-" {
				file, err := os.Open(f)
				if err != nil {
					return nil, err
				}
				defer file.Close()
				r = file
			}
			parsed, err := a.parseDocuments(f, r)
			if err != nil {
				return nil, err
			}
			docs = append(docs, parsed...)
		}
	}
	if len(docs) == 0 {
		return nil, errors.New("no objects passed to apply")
	}
	return docs, nil
}

func (a *app) parseDocuments(source string, r io.Reader) ([]document, error) {
	decoder := yaml.NewDecoder(r)
	var docs []document
	for i := 1; ; i++ {
		var raw any
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return docs, nil
			}
			return nil, fmt.Errorf("%s: document %d: %w", source, i, err)
		}
		if raw == nil {
			continue
		}
		value, err := jsonValue(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", source, i, err)
		}
		body, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: document %d: a resource must be a mapping", source, i)
		}
		d, err := a.document(body)
		if err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", source, i, err)
		}
		d.source = source
		docs = append(docs, d)
	}
}

func (a *app) document(body map[string]any) (document, error) {
	apiVersion, _ := body["apiVersion"].(string)
	kind, _ := body["kind"].(string)
	if apiVersion == "" || kind == "" {
		return document{}, errors.New("apiVersion and kind are required")
	}
	res, ok := scheme.ForKind(apiVersion, kind)
	if !ok {
		return document{}, fmt.Errorf("no matches for kind %q in version %q", kind, apiVersion)
	}
	if res.Resource == scheme.Nodes.Resource {
		return document{}, errors.New("nodes are registered by their kubelets and cannot be applied")
	}
	if _, ok := body["status"]; ok {
		return document{}, errors.New("status is managed by the cluster and cannot be set in a manifest")
	}
	meta, _ := body["metadata"].(map[string]any)
	if meta == nil {
		return document{}, errors.New("metadata is required")
	}
	for _, f := range serverManaged {
		if _, ok := meta[f]; ok {
			return document{}, fmt.Errorf("metadata.%s is server-managed and cannot be set in a manifest", f)
		}
	}
	name, _ := meta["name"].(string)
	if name == "" {
		return document{}, errors.New("metadata.name is required")
	}
	d := document{res: res, name: name, body: body}
	if res.Namespaced {
		ns, _ := meta["namespace"].(string)
		switch {
		case ns == "":
			ns = a.namespace
		case a.explicitNS && ns != a.namespace:
			return document{}, fmt.Errorf("the namespace from the provided object %q does not match the namespace %q", ns, a.namespace)
		}
		meta["namespace"] = ns
		d.namespace = ns
	}
	return d, nil
}

// jsonValue converts YAML values into JSON-compatible values.
func jsonValue(value any) (any, error) {
	switch v := value.(type) {
	case map[any]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			k, ok := key.(string)
			if !ok {
				return nil, errors.New("mapping keys must be strings")
			}
			converted, err := jsonValue(item)
			if err != nil {
				return nil, err
			}
			out[k] = converted
		}
		return out, nil
	case map[string]any:
		for k, item := range v {
			converted, err := jsonValue(item)
			if err != nil {
				return nil, err
			}
			v[k] = converted
		}
	case []any:
		for i, item := range v {
			converted, err := jsonValue(item)
			if err != nil {
				return nil, err
			}
			v[i] = converted
		}
	}
	return value, nil
}

// applyFiles creates or updates every document. All documents are validated
// by a server-side dry run first, so a bad document changes nothing.
func (a *app) applyFiles(ctx context.Context, p args, createOnly bool) error {
	files := p.values["filename"]
	if len(files) == 0 {
		return errors.New("error: must specify one of -f")
	}
	docs, err := a.readDocuments(files)
	if err != nil {
		return err
	}
	pendingNS := map[string]bool{}
	for _, d := range docs {
		// Objects in a namespace created earlier in this batch cannot be
		// dry-run yet; they are validated when applied.
		if d.res.Namespaced && pendingNS[d.namespace] {
			continue
		}
		action, err := a.applyOne(ctx, d, createOnly, true)
		if err != nil {
			return fmt.Errorf("%s (%s): %w; no objects were changed", d.ref(), d.source, err)
		}
		if d.res.Resource == scheme.Namespaces.Resource && action == "created" {
			pendingNS[d.name] = true
		}
	}
	for _, d := range docs {
		action, err := a.applyOne(ctx, d, createOnly, false)
		if err != nil {
			return fmt.Errorf("%s: %w", d.ref(), err)
		}
		fmt.Fprintf(a.out, "%s %s\n", d.ref(), action)
	}
	return nil
}

// applyOne creates the object or merge-patches the existing one.
func (a *app) applyOne(ctx context.Context, d document, createOnly, dryRun bool) (string, error) {
	query := url.Values{}
	if dryRun {
		query.Set("dryRun", "All")
	}
	current, err := a.getObject(ctx, d.res, d.namespace, d.name)
	if client.IsNotFound(err) {
		if err := a.client.Do(ctx, http.MethodPost, d.res.Path(d.namespace, ""), query, "", d.body, nil); err != nil {
			return "", err
		}
		return "created", nil
	}
	if err != nil {
		return "", err
	}
	if createOnly {
		return "", fmt.Errorf("%s %q already exists", d.res.GroupResource(), d.name)
	}
	patch, err := json.Marshal(d.body)
	if err != nil {
		return "", err
	}
	updated := d.res.New()
	if err := a.client.Do(ctx, http.MethodPatch, d.res.Path(d.namespace, d.name), query, "application/merge-patch+json", patch, updated); err != nil {
		return "", err
	}
	if updated.GetObjectMeta().ResourceVersion == current.GetObjectMeta().ResourceVersion {
		return "unchanged", nil
	}
	return "configured", nil
}
