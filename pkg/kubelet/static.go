package kubelet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/apis/core/validation"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/util/deepcopy"
	"mini-k8s/pkg/util/ptr"
)

// DefaultStaticPodPath is where kubeadm puts the control plane's manifests.
const DefaultStaticPodPath = "/etc/kubernetes/manifests"

// LoadStaticPods reads the Pod manifests (*.yaml, *.yml, *.json) of dir for
// node, like the kubelet's file source: each Pod is named
// "<name>-<node>", bound to the node, defaulted and validated, and gets a UID
// derived from the node and the manifest, so an edited manifest is a new
// Pod. Invalid manifests, and manifests whose Pod name a manifest earlier in
// the directory already uses, are skipped with an error. The map is nil only
// when the directory cannot be read (a missing directory has no Pods).
func LoadStaticPods(dir, node string) (map[string]*corev1.Pod, []error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return map[string]*corev1.Pod{}, nil
	}
	if err != nil {
		return nil, []error{err}
	}
	pods := map[string]*corev1.Pod{}
	names := map[string]string{} // namespace/name -> file
	var errs []error
	for _, e := range entries {
		name := e.Name()
		ext := filepath.Ext(name)
		if e.IsDir() || strings.HasPrefix(name, ".") || (ext != ".yaml" && ext != ".yml" && ext != ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			var pod *corev1.Pod
			if pod, err = decodeStaticPod(data, node); err == nil {
				full := pod.Namespace + "/" + pod.Name
				if first, dup := names[full]; dup {
					err = fmt.Errorf("Pod %s is already defined in %s", full, first)
				} else {
					names[full] = name
					pods[pod.UID] = pod
					continue
				}
			}
		}
		errs = append(errs, fmt.Errorf("static Pod %s: %w", name, err))
	}
	return pods, errs
}

func decodeStaticPod(data []byte, node string) (*corev1.Pod, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var pod corev1.Pod
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pod); err != nil {
		return nil, err
	}
	if pod.APIVersion != "v1" || pod.Kind != "Pod" {
		return nil, fmt.Errorf("want apiVersion v1, kind Pod; got %s %s", pod.APIVersion, pod.Kind)
	}
	if pod.Name == "" {
		return nil, fmt.Errorf("metadata.name is required")
	}
	if pod.Namespace == "" {
		pod.Namespace = corev1.NamespaceDefault
	}
	sum := sha256.Sum256(append([]byte(node+"\n"), data...))
	h := hex.EncodeToString(sum[:16])
	pod.UID = fmt.Sprintf("%s-%s-%s-%s-%s", h[:8], h[8:12], h[12:16], h[16:20], h[20:])
	pod.Name += "-" + node
	pod.Spec.NodeName = node
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[corev1.AnnotationConfigSource] = "file"
	pod.Annotations[corev1.AnnotationConfigHash] = pod.UID
	corev1.SetDefaultsPodSpec(&pod.Spec)
	if errs := validation.ValidatePodSpec("spec", &pod.Spec); len(errs) > 0 {
		return nil, fmt.Errorf("invalid: %s", strings.Join(errs, "; "))
	}
	pod.CreationTimestamp = metav1.Now()
	pod.Status = corev1.PodStatus{Phase: corev1.PodPending, QOSClass: corev1.GetPodQOS(&pod.Spec)}
	return &pod, nil
}

// staticLoop runs the node's static Pods without the API: it rereads the
// manifest directory every few seconds, starts new Pods, stops removed or
// changed ones, and wakes every static Pod's worker each second (container
// exits are not pushed to the kubelet).
func (k *Kubelet) staticLoop(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for i := 0; ; i++ {
		if i%5 == 0 {
			k.reloadStaticPods()
		}
		k.mu.Lock()
		pods := make([]*corev1.Pod, 0, len(k.static))
		for _, p := range k.static {
			pods = append(pods, p)
		}
		k.mu.Unlock()
		for _, p := range pods {
			k.dispatch(p)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// reloadStaticPods applies the manifest directory: new Pods are added, and
// Pods whose manifest disappeared or changed are marked for termination.
func (k *Kubelet) reloadStaticPods() {
	loaded, errs := LoadStaticPods(k.cfg.StaticPodPath, k.cfg.NodeName)
	for _, err := range errs {
		log.Printf("kubelet: %v", err)
	}
	if loaded == nil {
		return // the directory could not be read: change nothing
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for uid, p := range loaded {
		if _, ok := k.static[uid]; !ok {
			log.Printf("kubelet: static Pod %s/%s added", p.Namespace, p.Name)
			k.static[uid] = p
		}
	}
	for uid, p := range k.static {
		if _, ok := loaded[uid]; !ok && p.DeletionTimestamp == nil {
			log.Printf("kubelet: static Pod %s/%s removed", p.Namespace, p.Name)
			cp := deepcopy.Copy(p)
			now := metav1.Now()
			cp.DeletionTimestamp, cp.DeletionGracePeriodSeconds = &now, cp.Spec.TerminationGracePeriodSeconds
			k.static[uid] = cp
		}
	}
}

// staticPod returns the static Pod with the UID.
func (k *Kubelet) staticPod(uid string) (*corev1.Pod, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	p, ok := k.static[uid]
	return p, ok
}

// staticPodNamed returns the running (not terminating) static Pod with the
// name, preferring it over others with the same name.
func (k *Kubelet) staticPodNamed(namespace, name string) (*corev1.Pod, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var found []*corev1.Pod
	for _, p := range k.static {
		if p.Namespace == namespace && p.Name == name {
			found = append(found, p)
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].DeletionTimestamp == nil && found[j].DeletionTimestamp != nil })
	if len(found) == 0 {
		return nil, false
	}
	return found[0], true
}

// syncStatic keeps the kubelet's copy of a static Pod's status and its
// mirror Pod: the mirror is created when missing, replaced when it belongs
// to another version of the Pod or is being deleted, and carries the status.
func (k *Kubelet) syncStatic(ctx context.Context, pod *corev1.Pod, status corev1.PodStatus) {
	k.mu.Lock()
	if p, ok := k.static[pod.UID]; ok {
		cp := deepcopy.Copy(p)
		cp.Status = status
		k.static[pod.UID] = cp
	}
	k.mu.Unlock()

	pods := client.Pods(k.client, pod.Namespace)
	mirror, ok := k.pods.Get(pod.Namespace, pod.Name)
	switch {
	case ok && !corev1.IsMirrorPod(mirror):
		log.Printf("kubelet: static Pod %s/%s: a Pod with its name exists in the API; not creating a mirror Pod", pod.Namespace, pod.Name)
	case ok && (mirror.Annotations[corev1.AnnotationConfigMirror] != pod.UID || mirror.DeletionTimestamp != nil):
		// Deleted by a user or left by an earlier version: replace it.
		err := pods.Delete(ctx, mirror.Name, &metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0),
			Preconditions: &metav1.Preconditions{UID: &mirror.UID}})
		if err != nil && !client.IsNotFound(err) && !client.IsConflict(err) {
			log.Printf("kubelet: delete mirror Pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
	case ok:
		if !equalStatus(mirror.Status, status) {
			cp := deepcopy.Copy(mirror)
			cp.Status = status
			if _, err := pods.UpdateStatus(ctx, cp); err != nil && !client.IsConflict(err) && !client.IsNotFound(err) {
				log.Printf("kubelet: update mirror Pod %s/%s: %v", pod.Namespace, pod.Name, err)
			}
		}
	default:
		node, found := k.nodes.Get("", k.cfg.NodeName)
		if !found {
			return // the node registers first; retried on the next sync
		}
		m := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace, Labels: pod.Labels,
			Annotations: map[string]string{}, OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "v1", Kind: "Node", Name: node.Name, UID: node.UID}}}, Spec: pod.Spec}
		for key, v := range pod.Annotations {
			m.Annotations[key] = v
		}
		m.Annotations[corev1.AnnotationConfigMirror] = pod.UID
		created, err := pods.Create(ctx, m)
		if err != nil {
			if !client.IsAlreadyExists(err) {
				log.Printf("kubelet: create mirror Pod %s/%s: %v", pod.Namespace, pod.Name, err)
			}
			return
		}
		created.Status = status
		if _, err := pods.UpdateStatus(ctx, created); err != nil && !client.IsConflict(err) {
			log.Printf("kubelet: update mirror Pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
	}
}

// removeStatic finishes a static Pod whose containers were stopped: the
// kubelet forgets it and deletes its mirror Pod. The API may be down (the
// Pod may have been the API server); the Pod is forgotten anyway, so its
// manifest can come back, and housekeeping deletes the orphaned mirror later.
func (k *Kubelet) removeStatic(ctx context.Context, pod *corev1.Pod) {
	if mirror, ok := k.pods.Get(pod.Namespace, pod.Name); ok && mirror.Annotations[corev1.AnnotationConfigMirror] == pod.UID {
		err := client.Pods(k.client, pod.Namespace).Delete(ctx, mirror.Name, &metav1.DeleteOptions{
			GracePeriodSeconds: ptr.To[int64](0), Preconditions: &metav1.Preconditions{UID: &mirror.UID}})
		if err != nil && !client.IsNotFound(err) && !client.IsConflict(err) {
			log.Printf("kubelet: delete mirror Pod %s/%s (housekeeping retries): %v", pod.Namespace, pod.Name, err)
		}
	}
	k.mu.Lock()
	delete(k.static, pod.UID)
	k.mu.Unlock()
}
