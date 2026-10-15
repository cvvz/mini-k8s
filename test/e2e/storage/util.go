// Package storage holds specs of ConfigMaps, Secrets and the volumes that
// project them and the downward API into Pods.
package storage

import (
	"context"
	"maps"
	"strings"

	"mini-k8s/test/e2e/framework"
)

// execIn runs a command in a Pod of the spec namespace through kk exec.
func execIn(ctx context.Context, f *framework.Framework, pod string, args ...string) (string, error) {
	return framework.RunKK(ctx, append([]string{"exec", "-n", f.Namespace, pod, "--"}, args...)...)
}

// lines splits command output into lines.
func lines(out string) []string { return strings.Split(strings.TrimRight(out, "\n"), "\n") }

// sleeper is a container that idles, with extra fields (env, volumeMounts…).
func sleeper(extra map[string]any) map[string]any {
	c := map[string]any{"name": "c", "image": framework.DemoImage, "command": []any{"/mini"}, "args": []any{"sleep", "1h"}}
	maps.Copy(c, extra)
	return c
}

// newPod builds a Pod labelled app=<name> with one container and volumes.
func newPod(name string, container map[string]any, volumes ...any) framework.Object {
	spec := map[string]any{"containers": []any{container}}
	if len(volumes) > 0 {
		spec["volumes"] = volumes
	}
	return framework.Object{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "labels": map[string]any{"app": name}}, "spec": spec}
}

func app(name string) map[string]string { return map[string]string{"app": name} }

func configMap(name string, data map[string]any) framework.Object {
	return framework.Object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": name}, "data": data}
}

func secret(name string, stringData map[string]any) framework.Object {
	return framework.Object{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": name}, "stringData": stringData}
}

func keyRef(kind, name, key string) map[string]any {
	return map[string]any{kind: map[string]any{"name": name, "key": key}}
}

func mount(name, path string) map[string]any { return map[string]any{"name": name, "mountPath": path} }
