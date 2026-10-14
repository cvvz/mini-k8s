package apiserver_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mini-k8s/pkg/apiserver"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/storage/etcd3"
	etcdtesting "mini-k8s/pkg/storage/etcd3/testing"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	appsv1 "mini-k8s/pkg/apis/apps/v1"
	"mini-k8s/pkg/watch"
	"mini-k8s/pkg/scheme"
	"mini-k8s/pkg/util/ptr"
	"mini-k8s/pkg/util/deepcopy"
)

func newServer(t *testing.T) (*client.Client, string) {
	t.Helper()
	s := apiserver.New(apiserver.Config{Store: etcd3.New(etcdtesting.URL(t)), Token: "secret", Prefix: etcdtesting.Prefix(t)})
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s)
	t.Cleanup(hs.Close)
	return client.New(hs.URL, "secret"), hs.URL
}

func pod(name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
	}
}

func TestCreateGetUpdateConflict(t *testing.T) {
	c, _ := newServer(t)
	ctx := context.Background()
	pods := client.Pods(c, "default")
	created, err := pods.Create(ctx, pod("a", map[string]string{"app": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	if created.UID == "" || created.ResourceVersion == "" || created.CreationTimestamp.IsZero() ||
		created.Status.Phase != corev1.PodPending || *created.Spec.TerminationGracePeriodSeconds != 30 || created.Kind != "Pod" {
		t.Fatalf("server did not fill identity/defaults: %+v", created)
	}
	if _, err := pods.Create(ctx, pod("a", nil)); !client.IsAlreadyExists(err) {
		t.Fatalf("duplicate create: %v", err)
	}
	stale := *created
	created.Labels["app"] = "y"
	updated, err := pods.Update(ctx, created)
	if err != nil || updated.Labels["app"] != "y" || updated.ResourceVersion == created.ResourceVersion {
		t.Fatalf("label update: %+v %v", updated, err)
	}
	stale.Labels = map[string]string{"app": "z"}
	if _, err := pods.Update(ctx, &stale); !client.IsConflict(err) {
		t.Fatalf("stale update: %v", err)
	}
	changed := deepcopy.Copy(updated)
	changed.Spec.Containers[0].Image = "other"
	if _, err := pods.Update(ctx, changed); !client.IsInvalid(err) || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("pod spec change: %v", err)
	}
	if _, err := client.Pods(c, "missing").Create(ctx, pod("b", nil)); !client.IsNotFound(err) {
		t.Fatalf("create in missing namespace: %v", err)
	}
}

func TestStrictDecodingAndValidation(t *testing.T) {
	c, _ := newServer(t)
	ctx := context.Background()
	body := []byte(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p"},"spec":{"containers":[{"name":"c","image":"i"}],"bogus":1}}`)
	err := c.Do(ctx, http.MethodPost, scheme.Pods.Path("default", ""), nil, "", body, nil)
	if client.Reason(err) != metav1.ReasonBadRequest || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field: %v", err)
	}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d"}, Spec: appsv1.DeploymentSpec{
		Replicas: ptr.To[int32](-1), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "a"}},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "b"}}, Spec: pod("", nil).Spec},
	}}
	_, err = client.Deployments(c, "default").Create(ctx, d)
	if !client.IsInvalid(err) || !strings.Contains(err.Error(), "spec.replicas") || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("invalid deployment: %v", err)
	}
	var apiErr *client.APIError
	if !asAPIError(err, &apiErr) || len(apiErr.Status.Details.Causes) < 2 {
		t.Fatalf("missing causes: %+v", err)
	}
}

func asAPIError(err error, target **client.APIError) bool {
	e, ok := err.(*client.APIError)
	if ok {
		*target = e
	}
	return ok
}

func TestStatusSubresourceAndGeneration(t *testing.T) {
	c, _ := newServer(t)
	ctx := context.Background()
	deps := client.Deployments(c, "default")
	d, err := deps.Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web"}, Spec: appsv1.DeploymentSpec{
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}}, Spec: pod("", nil).Spec},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Generation != 1 || *d.Spec.Replicas != 1 {
		t.Fatalf("generation/defaults: %+v", d)
	}
	d.Status.ReadyReplicas = 5 // ignored by the main resource
	d.Spec.Replicas = ptr.To[int32](3)
	d, err = deps.Update(ctx, d)
	if err != nil || d.Generation != 2 || d.Status.ReadyReplicas != 0 {
		t.Fatalf("spec update: %+v %v", d, err)
	}
	d.Status.ReadyReplicas = 2
	d.Spec.Replicas = ptr.To[int32](9) // ignored by /status
	d, err = deps.UpdateStatus(ctx, d)
	if err != nil || d.Status.ReadyReplicas != 2 || *d.Spec.Replicas != 3 || d.Generation != 2 {
		t.Fatalf("status update: %+v %v", d, err)
	}
	rv := d.ResourceVersion
	same, err := deps.Update(ctx, d)
	if err != nil || same.ResourceVersion != rv {
		t.Fatalf("no-op update should not write: %s -> %s (%v)", rv, same.ResourceVersion, err)
	}
	patched, err := deps.Patch(ctx, "web", map[string]any{"spec": map[string]any{"replicas": 0}})
	if err != nil || *patched.Spec.Replicas != 0 || patched.Generation != 3 {
		t.Fatalf("merge patch: %+v %v", patched, err)
	}
	_, err = deps.Patch(ctx, "web", map[string]any{"metadata": map[string]any{"resourceVersion": rv}, "spec": map[string]any{"replicas": 4}})
	if !client.IsConflict(err) {
		t.Fatalf("patch with stale resourceVersion: %v", err)
	}
}

func TestGenerateNameAndSelectors(t *testing.T) {
	c, _ := newServer(t)
	ctx := context.Background()
	pods := client.Pods(c, "default")
	for i, app := range []string{"a", "a", "b"} {
		p := pod("", map[string]string{"app": app})
		p.GenerateName = "gen-"
		created, err := pods.Create(ctx, p)
		if err != nil || !strings.HasPrefix(created.Name, "gen-") || len(created.Name) != len("gen-")+5 {
			t.Fatalf("generateName %d: %+v %v", i, created, err)
		}
	}
	list, err := pods.List(ctx, client.ListOptions{LabelSelector: "app=a"})
	if err != nil || len(list.Items) != 2 || list.ListMeta.ResourceVersion == "" || list.Kind != "PodList" {
		t.Fatalf("label selector: %+v %v", list, err)
	}
	list, err = pods.List(ctx, client.ListOptions{FieldSelector: "spec.nodeName="})
	if err != nil || len(list.Items) != 3 {
		t.Fatalf("field selector: %d %v", len(list.Items), err)
	}
	if _, err := pods.List(ctx, client.ListOptions{FieldSelector: "spec.bogus=x"}); client.Reason(err) != metav1.ReasonBadRequest {
		t.Fatalf("unsupported field selector: %v", err)
	}
	all, err := client.Pods(c, "").List(ctx, client.ListOptions{})
	if err != nil || len(all.Items) != 3 {
		t.Fatalf("all namespaces: %v", err)
	}
}

func TestWatchTranslatesSelectorTransitions(t *testing.T) {
	c, _ := newServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pods := client.Pods(c, "default")
	if _, err := pods.Create(ctx, pod("existing", map[string]string{"app": "w"})); err != nil {
		t.Fatal(err)
	}
	w, err := pods.Watch(ctx, client.ListOptions{LabelSelector: "app=w"})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	next := func() client.Event[*corev1.Pod] {
		select {
		case ev := <-w.Events():
			return ev
		case <-ctx.Done():
			t.Fatal("timed out waiting for watch event")
		}
		return client.Event[*corev1.Pod]{}
	}
	if ev := next(); ev.Type != watch.Added || ev.Object.Name != "existing" {
		t.Fatalf("initial: %+v", ev)
	}
	p, _ := pods.Create(ctx, pod("new", map[string]string{"app": "w"}))
	if ev := next(); ev.Type != watch.Added || ev.Object.Name != "new" {
		t.Fatalf("added: %+v", ev)
	}
	p.Labels["app"] = "other"
	if _, err := pods.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	if ev := next(); ev.Type != watch.Deleted || ev.Object.Name != "new" {
		t.Fatalf("leaving the selection must be DELETED: %+v", ev)
	}
	if err := pods.Delete(ctx, "existing", nil); err != nil {
		t.Fatal(err)
	}
	if ev := next(); ev.Type != watch.Deleted || ev.Object.Name != "existing" || ev.Object.ResourceVersion == "" {
		t.Fatalf("deleted: %+v", ev)
	}
}

func TestPodGracefulDeletionAndFinalizers(t *testing.T) {
	c, _ := newServer(t)
	ctx := context.Background()
	pods := client.Pods(c, "default")
	if _, err := pods.Create(ctx, pod("unscheduled", nil)); err != nil {
		t.Fatal(err)
	}
	if err := pods.Delete(ctx, "unscheduled", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pods.Get(ctx, "unscheduled"); !client.IsNotFound(err) {
		t.Fatalf("unscheduled Pod should be deleted immediately: %v", err)
	}

	if _, err := pods.Create(ctx, pod("bound", nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.BindPod(ctx, "default", "bound", "worker-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.BindPod(ctx, "default", "bound", "worker-2"); !client.IsConflict(err) {
		t.Fatalf("rebinding: %v", err)
	}
	if err := pods.Delete(ctx, "bound", nil); err != nil {
		t.Fatal(err)
	}
	p, err := pods.Get(ctx, "bound")
	if err != nil || p.DeletionTimestamp == nil || *p.DeletionGracePeriodSeconds != 30 || p.Spec.NodeName != "worker-1" {
		t.Fatalf("bound Pod should be terminating: %+v %v", p, err)
	}
	wrongUID := "nope"
	if err := pods.Delete(ctx, "bound", &metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0), Preconditions: &metav1.Preconditions{UID: &wrongUID}}); !client.IsConflict(err) {
		t.Fatalf("uid precondition: %v", err)
	}
	if err := pods.Delete(ctx, "bound", &metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0), Preconditions: &metav1.Preconditions{UID: &p.UID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pods.Get(ctx, "bound"); !client.IsNotFound(err) {
		t.Fatalf("grace 0 should remove the Pod: %v", err)
	}

	f := pod("final", nil)
	f.Finalizers = []string{"example.com/block"}
	if _, err := pods.Create(ctx, f); err != nil {
		t.Fatal(err)
	}
	if err := pods.Delete(ctx, "final", nil); err != nil {
		t.Fatal(err)
	}
	f, err = pods.Get(ctx, "final")
	if err != nil || f.DeletionTimestamp == nil {
		t.Fatalf("finalizer must block deletion: %+v %v", f, err)
	}
	if _, err := pods.Patch(ctx, "final", map[string]any{"metadata": map[string]any{"finalizers": nil}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pods.Get(ctx, "final"); !client.IsNotFound(err) {
		t.Fatalf("removing the last finalizer should complete deletion: %v", err)
	}
}

func TestPropagationPolicyFinalizersAndNamespaceTermination(t *testing.T) {
	c, _ := newServer(t)
	ctx := context.Background()
	if _, err := client.Namespaces(c).Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team"}}); err != nil {
		t.Fatal(err)
	}
	pods := client.Pods(c, "team")
	if _, err := pods.Create(ctx, pod("p", nil)); err != nil {
		t.Fatal(err)
	}
	orphan := metav1.DeletePropagationOrphan
	if err := pods.Delete(ctx, "p", &metav1.DeleteOptions{PropagationPolicy: &orphan}); err != nil {
		t.Fatal(err)
	}
	p, err := pods.Get(ctx, "p")
	if err != nil || !p.HasFinalizer(metav1.FinalizerOrphanDependents) {
		t.Fatalf("orphan policy must add the orphan finalizer: %+v %v", p, err)
	}
	if err := client.Namespaces(c).Delete(ctx, "team", nil); err != nil {
		t.Fatal(err)
	}
	ns, err := client.Namespaces(c).Get(ctx, "team")
	if err != nil || ns.Status.Phase != corev1.NamespaceTerminating || !ns.HasFinalizer(corev1.FinalizerKubernetes) {
		t.Fatalf("namespace should be Terminating: %+v %v", ns, err)
	}
	if _, err := pods.Create(ctx, pod("late", nil)); !client.IsForbidden(err) {
		t.Fatalf("create in terminating namespace: %v", err)
	}
}

func TestAuthenticationAndDiscovery(t *testing.T) {
	c, url := newServer(t)
	resp, err := http.Get(url + "/api/v1/pods")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", resp.StatusCode)
	}
	var list struct {
		GroupVersion string `json:"groupVersion"`
		Resources    []struct {
			Name string `json:"name"`
		} `json:"resources"`
	}
	if err := c.Do(context.Background(), http.MethodGet, "/apis/apps/v1", nil, "", nil, &list); err != nil || list.GroupVersion != "apps/v1" {
		t.Fatalf("discovery: %+v %v", list, err)
	}
	if err := c.Do(context.Background(), http.MethodGet, "/api/v1/namespaces/default/widgets", nil, "", nil, nil); !client.IsNotFound(err) {
		t.Fatalf("unknown resource: %v", err)
	}
}

// TestUnconditionalWritesRetryLostRaces hammers a Deployment's status (as a
// controller would) while a client patches, updates and deletes it without
// resourceVersions; none of those writes may fail with a conflict.
func TestUnconditionalWritesRetryLostRaces(t *testing.T) {
	c, _ := newServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps := client.Deployments(c, "default")
	if _, err := deps.Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "busy"}, Spec: appsv1.DeploymentSpec{
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "busy"}},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "busy"}}, Spec: pod("", nil).Spec},
	}}); err != nil {
		t.Fatal(err)
	}
	go func() {
		for i := int32(0); ctx.Err() == nil; i++ {
			if d, err := deps.Get(ctx, "busy"); err == nil {
				d.Status.ReadyReplicas = i
				_, _ = deps.UpdateStatus(ctx, d)
			}
		}
	}()
	for i := 0; i < 20; i++ {
		if _, err := deps.Patch(ctx, "busy", map[string]any{"spec": map[string]any{"replicas": i}}); err != nil {
			t.Fatalf("patch %d: %v", i, err)
		}
		d, err := deps.Get(ctx, "busy")
		if err != nil {
			t.Fatal(err)
		}
		d.ResourceVersion = ""
		d.Labels = map[string]string{"round": fmt.Sprint(i)}
		if _, err := deps.Update(ctx, d); err != nil {
			t.Fatalf("unconditional update %d: %v", i, err)
		}
	}
	if err := deps.Delete(ctx, "busy", nil); err != nil {
		t.Fatalf("unconditional delete: %v", err)
	}
	stale := "1"
	if _, err := deps.Patch(ctx, "busy", map[string]any{"metadata": map[string]any{"resourceVersion": stale}}); !client.IsNotFound(err) && !client.IsConflict(err) {
		t.Fatalf("conditional patch must not be retried into success: %v", err)
	}
}
