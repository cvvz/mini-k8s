package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/scheme"
	"mini-k8s/pkg/util/ptr"
	"mini-k8s/pkg/watch"
)

// Version is the kk client version.
const Version = "v2.0.0-mini"

type app struct {
	client        *client.Client
	out           io.Writer
	errOut        io.Writer
	in            io.Reader
	namespace     string
	explicitNS    bool
	allNamespaces bool
	now           func() time.Time
}

func newApp(c *client.Client, out io.Writer, in io.Reader, a args) *app {
	return &app{
		client: c, out: out, errOut: os.Stderr, in: in,
		namespace: a.value("namespace", corev1.NamespaceDefault), explicitNS: len(a.values["namespace"]) > 0,
		allNamespaces: a.boolean("all-namespaces", false), now: time.Now,
	}
}

func (a *app) run(ctx context.Context, command string, p args) error {
	switch command {
	case "get":
		return a.get(ctx, p)
	case "describe":
		return a.describe(ctx, p)
	case "apply":
		return a.applyFiles(ctx, p, false)
	case "create":
		if len(p.positional) >= 1 && (p.positional[0] == "namespace" || p.positional[0] == "ns") {
			return a.createNamespace(ctx, p)
		}
		if len(p.positional) >= 1 && (p.positional[0] == "configmap" || p.positional[0] == "cm" || p.positional[0] == "secret") {
			return a.createConfig(ctx, p)
		}
		if len(p.positional) >= 1 && p.positional[0] == "job" {
			return a.createJob(ctx, p)
		}
		return a.applyFiles(ctx, p, true)
	case "delete":
		return a.delete(ctx, p)
	case "logs":
		return a.logs(ctx, p)
	case "exec":
		return a.exec(ctx, p)
	case "scale":
		return a.scale(ctx, p)
	case "top":
		return a.top(ctx, p)
	case "rollout":
		return a.rollout(ctx, p)
	case "cordon", "uncordon":
		return a.cordon(ctx, p, command == "cordon")
	case "drain":
		return a.drain(ctx, p)
	case "taint":
		return a.taint(ctx, p)
	case "label":
		return a.label(ctx, p)
	case "api-resources":
		return a.apiResources(ctx)
	case "version":
		return a.version(ctx)
	}
	return fmt.Errorf("unknown command %q", command)
}

// target is a resource type with optional names.
type target struct {
	res   scheme.ResourceInfo
	names []string
}

func lookupResource(name string) (scheme.ResourceInfo, error) {
	res, ok := scheme.Lookup(name)
	if !ok {
		return res, fmt.Errorf("the server doesn't have a resource type %q", name)
	}
	return res, nil
}

// parseTargets accepts "TYPE[,TYPE] [NAME...]" or "TYPE/NAME...".
func parseTargets(pos []string) ([]target, error) {
	if len(pos) == 0 {
		return nil, errors.New("you must specify the type of resource; run kk api-resources for a list")
	}
	if strings.Contains(pos[0], "/") {
		var out []target
		for _, p := range pos {
			typ, name, ok := strings.Cut(p, "/")
			if !ok || name == "" {
				return nil, fmt.Errorf("there is no need to specify a resource type as a separate argument when passing arguments in resource/name form: %q", p)
			}
			res, err := lookupResource(typ)
			if err != nil {
				return nil, err
			}
			out = append(out, target{res: res, names: []string{name}})
		}
		return out, nil
	}
	types := strings.Split(pos[0], ",")
	if len(types) > 1 && len(pos) > 1 {
		return nil, errors.New("names cannot be combined with multiple resource types; use TYPE/NAME")
	}
	var out []target
	for _, typ := range types {
		res, err := lookupResource(typ)
		if err != nil {
			return nil, err
		}
		out = append(out, target{res: res, names: pos[1:]})
	}
	return out, nil
}

// ns returns the namespace for requests on res ("" = cluster-scoped or all).
func (a *app) ns(res scheme.ResourceInfo, forList bool) string {
	if !res.Namespaced || (forList && a.allNamespaces) {
		return ""
	}
	return a.namespace
}

func (a *app) getObject(ctx context.Context, res scheme.ResourceInfo, ns, name string) (metav1.Object, error) {
	obj := res.New()
	err := a.client.Do(ctx, http.MethodGet, res.Path(ns, name), nil, "", nil, obj)
	return obj, err
}

func (a *app) listObjects(ctx context.Context, res scheme.ResourceInfo, ns string, opts client.ListOptions) ([]metav1.Object, string, error) {
	var raw struct {
		Metadata metav1.ListMeta   `json:"metadata"`
		Items    []json.RawMessage `json:"items"`
	}
	if err := a.client.Do(ctx, http.MethodGet, res.Path(ns, ""), opts.Query(), "", nil, &raw); err != nil {
		return nil, "", err
	}
	objs := make([]metav1.Object, 0, len(raw.Items))
	for _, item := range raw.Items {
		obj := res.New()
		if err := json.Unmarshal(item, obj); err != nil {
			return nil, "", err
		}
		objs = append(objs, obj)
	}
	sort.SliceStable(objs, func(i, j int) bool {
		mi, mj := objs[i].GetObjectMeta(), objs[j].GetObjectMeta()
		if mi.Namespace != mj.Namespace {
			return mi.Namespace < mj.Namespace
		}
		return mi.Name < mj.Name
	})
	return objs, raw.Metadata.ResourceVersion, nil
}

func (a *app) get(ctx context.Context, p args) error {
	targets, err := parseTargets(p.positional)
	if err != nil {
		return err
	}
	output := p.value("output", "")
	switch output {
	case "", "wide", "json", "yaml", "name":
	default:
		return fmt.Errorf("unsupported output format %q (use wide, json, yaml or name)", output)
	}
	opts := client.ListOptions{LabelSelector: p.value("selector", ""), FieldSelector: p.value("field-selector", "")}
	if p.boolean("watch", false) {
		return a.getAndWatch(ctx, targets, opts, output)
	}
	var all []metav1.Object
	for i, t := range targets {
		var objs []metav1.Object
		if len(t.names) > 0 {
			for _, name := range t.names {
				obj, err := a.getObject(ctx, t.res, a.ns(t.res, false), name)
				if err != nil {
					return err
				}
				objs = append(objs, obj)
			}
		} else {
			if objs, _, err = a.listObjects(ctx, t.res, a.ns(t.res, true), opts); err != nil {
				return err
			}
		}
		if output == "json" || output == "yaml" {
			all = append(all, objs...)
			continue
		}
		if i > 0 {
			fmt.Fprintln(a.out)
		}
		if err := a.printObjects(t.res, objs, output); err != nil {
			return err
		}
	}
	if output == "json" || output == "yaml" {
		var v any = all
		if len(targets) == 1 && len(targets[0].names) == 1 {
			v = all[0]
		} else {
			v = map[string]any{"apiVersion": "v1", "kind": "List", "items": all}
		}
		return writeStructured(a.out, v, output)
	}
	return nil
}

func (a *app) printObjects(res scheme.ResourceInfo, objs []metav1.Object, output string) error {
	if output == "name" {
		for _, obj := range objs {
			fmt.Fprintln(a.out, qualifiedName(res)+"/"+obj.GetObjectMeta().Name)
		}
		return nil
	}
	if len(objs) == 0 {
		if res.Namespaced && !a.allNamespaces {
			_, err := fmt.Fprintf(a.out, "No resources found in %s namespace.\n", a.namespace)
			return err
		}
		_, err := fmt.Fprintln(a.out, "No resources found")
		return err
	}
	return printTable(a.out, res, objs, tableOptions{wide: output == "wide", withNamespace: a.allNamespaces && res.Namespaced, header: true, now: a.now()})
}

// getAndWatch prints the current state of one resource type (optionally one
// named object) and then streams its changes, like kubectl get -w.
func (a *app) getAndWatch(ctx context.Context, targets []target, opts client.ListOptions, output string) error {
	if len(targets) != 1 || len(targets[0].names) > 1 {
		return errors.New("watch needs a single resource type and at most one name")
	}
	t := targets[0]
	if len(t.names) == 1 {
		// Report NotFound like a plain get, then narrow the watch to the object.
		if _, err := a.getObject(ctx, t.res, a.ns(t.res, false), t.names[0]); err != nil {
			return err
		}
		byName := "metadata.name=" + t.names[0]
		if opts.FieldSelector != "" {
			byName = opts.FieldSelector + "," + byName
		}
		opts.FieldSelector = byName
	}
	objs, rv, err := a.listObjects(ctx, t.res, a.ns(t.res, len(t.names) == 0), opts)
	if err != nil {
		return err
	}
	if output == "json" || output == "yaml" {
		for _, obj := range objs {
			if err := writeWatchObject(a.out, obj, output); err != nil {
				return err
			}
		}
	} else if err := a.printObjects(t.res, objs, output); err != nil {
		return err
	}
	return a.watch(ctx, t.res, len(t.names) == 0, opts, rv, output)
}

func writeWatchObject(w io.Writer, obj metav1.Object, output string) error {
	if output == "yaml" {
		if _, err := fmt.Fprintln(w, "---"); err != nil {
			return err
		}
	}
	return writeStructured(w, obj, output)
}

func (a *app) watch(ctx context.Context, res scheme.ResourceInfo, forList bool, opts client.ListOptions, rv, output string) error {
	for {
		q := opts
		q.ResourceVersion = rv
		values := q.Query()
		values.Set("watch", "true")
		resp, err := a.client.Request(ctx, http.MethodGet, res.Path(a.ns(res, forList), ""), values, "", nil, true)
		if err != nil {
			return err
		}
		dec := json.NewDecoder(resp.Body)
		for {
			var ev metav1.WatchEvent
			if err := dec.Decode(&ev); err != nil {
				resp.Body.Close()
				if ctx.Err() != nil {
					return nil
				}
				break // server timeout: re-establish from the last resourceVersion
			}
			if ev.Type == watch.Error {
				resp.Body.Close()
				return fmt.Errorf("watch: %s", ev.Object)
			}
			obj := res.New()
			if err := json.Unmarshal(ev.Object, obj); err != nil {
				resp.Body.Close()
				return err
			}
			rv = obj.GetObjectMeta().ResourceVersion
			switch output {
			case "name":
				fmt.Fprintln(a.out, qualifiedName(res)+"/"+obj.GetObjectMeta().Name)
				continue
			case "json", "yaml":
				if err := writeWatchObject(a.out, obj, output); err != nil {
					resp.Body.Close()
					return err
				}
				continue
			}
			if err := printTable(a.out, res, []metav1.Object{obj}, tableOptions{wide: output == "wide", withNamespace: a.allNamespaces && res.Namespaced, now: a.now(), eventType: ev.Type}); err != nil {
				resp.Body.Close()
				return err
			}
		}
	}
}

// qualifiedName returns "pod" or "deployment.apps", as kubectl prints.
func qualifiedName(res scheme.ResourceInfo) string {
	if res.Group == "" {
		return res.Singular
	}
	return res.Singular + "." + res.Group
}

func (a *app) describe(ctx context.Context, p args) error {
	targets, err := parseTargets(p.positional)
	if err != nil {
		return err
	}
	first := true
	for _, t := range targets {
		var objs []metav1.Object
		if len(t.names) == 0 {
			if objs, _, err = a.listObjects(ctx, t.res, a.ns(t.res, true), client.ListOptions{}); err != nil {
				return err
			}
		}
		for _, name := range t.names {
			obj, err := a.getObject(ctx, t.res, a.ns(t.res, false), name)
			if err != nil {
				return err
			}
			objs = append(objs, obj)
		}
		for _, obj := range objs {
			if !first {
				fmt.Fprintln(a.out)
			}
			first = false
			if err := a.describeObject(ctx, t.res, obj); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *app) createNamespace(ctx context.Context, p args) error {
	if len(p.positional) != 2 {
		return errors.New("usage: kk create namespace NAME")
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.positional[1]}}
	if _, err := client.Namespaces(a.client).Create(ctx, ns); err != nil {
		return err
	}
	_, err := fmt.Fprintf(a.out, "namespace/%s created\n", ns.Name)
	return err
}

func (a *app) delete(ctx context.Context, p args) error {
	opts := &metav1.DeleteOptions{}
	if g := p.value("grace-period", ""); g != "" {
		n, err := strconv.ParseInt(g, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid --grace-period %q", g)
		}
		if n >= 0 {
			opts.GracePeriodSeconds = &n
		}
	}
	if p.boolean("force", false) {
		opts.GracePeriodSeconds = ptr.To[int64](0)
	}
	switch c := strings.ToLower(p.value("cascade", "background")); c {
	case "background", "true":
		policy := metav1.DeletePropagationBackground
		opts.PropagationPolicy = &policy
	case "foreground":
		policy := metav1.DeletePropagationForeground
		opts.PropagationPolicy = &policy
	case "orphan", "false":
		policy := metav1.DeletePropagationOrphan
		opts.PropagationPolicy = &policy
	default:
		return fmt.Errorf("invalid --cascade %q (use background, foreground or orphan)", c)
	}
	type victim struct {
		res       scheme.ResourceInfo
		namespace string
		name      string
		uid       string // of the deleted object, set after deletion
	}
	var victims []victim
	if files := p.values["filename"]; len(files) > 0 {
		docs, err := a.readDocuments(files)
		if err != nil {
			return err
		}
		for _, d := range docs {
			victims = append(victims, victim{d.res, d.namespace, d.name, ""})
		}
	} else {
		targets, err := parseTargets(p.positional)
		if err != nil {
			return err
		}
		selector := p.value("selector", "")
		for _, t := range targets {
			if len(t.names) == 0 {
				if selector == "" && !p.boolean("all", false) {
					return fmt.Errorf("resource(s) were provided, but no name was specified; use a name, -l SELECTOR or --all")
				}
				objs, _, err := a.listObjects(ctx, t.res, a.ns(t.res, false), client.ListOptions{LabelSelector: selector})
				if err != nil {
					return err
				}
				for _, obj := range objs {
					victims = append(victims, victim{t.res, obj.GetObjectMeta().Namespace, obj.GetObjectMeta().Name, ""})
				}
			}
			for _, name := range t.names {
				victims = append(victims, victim{t.res, a.ns(t.res, false), name, ""})
			}
		}
	}
	ignoreNotFound := p.boolean("ignore-not-found", false)
	var deleted []victim
	for _, v := range victims {
		if v.res.Resource == scheme.Nodes.Resource {
			return errors.New("nodes are registered by their kubelets and cannot be deleted with kk")
		}
		obj := v.res.New()
		err := a.client.Do(ctx, http.MethodDelete, v.res.Path(v.namespace, v.name), nil, "", opts, obj)
		if client.IsNotFound(err) && ignoreNotFound {
			continue
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(a.out, "%s %q deleted\n", qualifiedName(v.res), v.name)
		v.uid = obj.GetObjectMeta().UID
		deleted = append(deleted, v)
	}
	if !p.boolean("wait", true) {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for _, v := range deleted {
		for {
			// Done once the deleted object is gone, even if a new one with
			// the same name (a recreated mirror Pod) already exists.
			obj, err := a.getObject(waitCtx, v.res, v.namespace, v.name)
			if client.IsNotFound(err) || (err == nil && v.uid != "" && obj.GetObjectMeta().UID != v.uid) {
				break
			}
			if waitCtx.Err() != nil {
				return fmt.Errorf("timed out waiting for %s %q to be deleted", qualifiedName(v.res), v.name)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	return nil
}

func (a *app) logs(ctx context.Context, p args) error {
	if len(p.positional) != 1 {
		return errors.New("usage: kk logs POD [-c CONTAINER] [-f] [--tail N] [-p]")
	}
	name := strings.TrimPrefix(strings.TrimPrefix(p.positional[0], "pod/"), "pods/")
	tail := 0
	if t := p.value("tail", ""); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 0 {
			return fmt.Errorf("invalid --tail %q", t)
		}
		tail = n
	}
	container, err := a.defaultContainer(ctx, name, p.value("container", ""))
	if err != nil {
		return err
	}
	stream, err := a.client.PodLogs(ctx, a.namespace, name, client.LogOptions{
		Container: container, Follow: p.boolean("follow", false), TailLines: tail,
		Previous: p.boolean("previous", false)})
	if err != nil {
		return err
	}
	defer stream.Close()
	_, err = io.Copy(a.out, stream)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (a *app) scale(ctx context.Context, p args) error {
	pos := p.positional
	if len(pos) == 1 && strings.Contains(pos[0], "/") {
		typ, name, _ := strings.Cut(pos[0], "/")
		pos = []string{typ, name}
	}
	if len(pos) != 2 {
		return errors.New("usage: kk scale deployment|replicaset|statefulset NAME --replicas N")
	}
	res, err := lookupResource(pos[0])
	if err != nil {
		return err
	}
	if res.Resource != scheme.Deployments.Resource && res.Resource != scheme.ReplicaSets.Resource &&
		res.Resource != scheme.StatefulSets.Resource {
		return fmt.Errorf("scale is not supported for %s", res.Resource)
	}
	replicas, err := strconv.Atoi(p.value("replicas", ""))
	if err != nil || replicas < 0 {
		return errors.New("--replicas must be a non-negative integer")
	}
	patch := map[string]any{"spec": map[string]any{"replicas": replicas}}
	if err := a.client.Do(ctx, http.MethodPatch, res.Path(a.namespace, pos[1]), nil, "application/merge-patch+json", patch, nil); err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.out, "%s/%s scaled\n", qualifiedName(res), pos[1])
	return err
}

func (a *app) apiResources(ctx context.Context) error {
	var groups struct {
		Groups []struct {
			PreferredVersion struct {
				GroupVersion string `json:"groupVersion"`
			} `json:"preferredVersion"`
		} `json:"groups"`
	}
	if err := a.client.Do(ctx, http.MethodGet, "/apis", nil, "", nil, &groups); err != nil {
		return err
	}
	paths := []string{"/api/v1"}
	for _, g := range groups.Groups {
		paths = append(paths, "/apis/"+g.PreferredVersion.GroupVersion)
	}
	tw := newTabWriter(a.out)
	fmt.Fprintln(tw, "NAME\tSHORTNAMES\tAPIVERSION\tNAMESPACED\tKIND")
	var failed []string
	for _, path := range paths {
		var list struct {
			GroupVersion string `json:"groupVersion"`
			Resources    []struct {
				Name       string   `json:"name"`
				Namespaced bool     `json:"namespaced"`
				Kind       string   `json:"kind"`
				ShortNames []string `json:"shortNames"`
			} `json:"resources"`
		}
		if err := a.client.Do(ctx, http.MethodGet, path, nil, "", nil, &list); err != nil {
			// Like kubectl: an unavailable aggregated group is reported
			// after the others.
			failed = append(failed, strings.TrimPrefix(strings.TrimPrefix(path, "/apis/"), "/api/")+": "+errorMessage(err))
			continue
		}
		for _, r := range list.Resources {
			if strings.Contains(r.Name, "/") {
				continue
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\n", r.Name, strings.Join(r.ShortNames, ","), list.GroupVersion, r.Namespaced, r.Kind)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(failed) > 0 {
		return fmt.Errorf("unable to retrieve the complete list of server APIs: %s", strings.Join(failed, ", "))
	}
	return nil
}

// errorMessage is the message of an API error, or the error itself.
func errorMessage(err error) string {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && apiErr.Status.Message != "" {
		return apiErr.Status.Message
	}
	return err.Error()
}

func (a *app) version(ctx context.Context) error {
	fmt.Fprintf(a.out, "Client Version: %s\n", Version)
	var v struct {
		GitVersion string `json:"gitVersion"`
	}
	if err := a.client.Do(ctx, http.MethodGet, "/version", nil, "", nil, &v); err != nil {
		return err
	}
	_, err := fmt.Fprintf(a.out, "Server Version: %s\n", v.GitVersion)
	return err
}

// exitCodeError carries the exit code of a command run with kk exec.
type exitCodeError struct{ code int }

func (e exitCodeError) Error() string {
	return fmt.Sprintf("command terminated with exit code %d", e.code)
}

func (a *app) exec(ctx context.Context, p args) error {
	if len(p.positional) != 1 || len(p.rest) == 0 {
		return errors.New("usage: kk exec POD [-c CONTAINER] -- COMMAND [ARG...]")
	}
	name := strings.TrimPrefix(strings.TrimPrefix(p.positional[0], "pod/"), "pods/")
	container, err := a.defaultContainer(ctx, name, p.value("container", ""))
	if err != nil {
		return err
	}
	res, err := a.client.PodExec(ctx, a.namespace, name, client.ExecOptions{Container: container, Command: p.rest})
	if err != nil {
		return err
	}
	if _, err := a.out.Write(res.Stdout); err != nil {
		return err
	}
	if _, err := a.errOut.Write(res.Stderr); err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return exitCodeError{int(res.ExitCode)}
	}
	return nil
}

// defaultContainer returns the requested container or, like kubectl, the
// first container of the Pod, announcing the choice when there are several.
func (a *app) defaultContainer(ctx context.Context, pod, container string) (string, error) {
	if container != "" {
		return container, nil
	}
	p, err := client.Pods(a.client, a.namespace).Get(ctx, pod)
	if err != nil {
		return "", err
	}
	if len(p.Spec.Containers) > 1 {
		names := make([]string, len(p.Spec.Containers))
		for i, c := range p.Spec.Containers {
			names[i] = c.Name
		}
		fmt.Fprintf(a.errOut, "Defaulted container %q out of: %s\n", names[0], strings.Join(names, ", "))
	}
	return p.Spec.Containers[0].Name, nil
}
