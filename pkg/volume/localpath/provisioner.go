// Package localpath is a dynamic provisioner of node-local volumes, like the
// local-path-provisioner kind ships: for a claim scheduled to a node it
// creates a hostPath PersistentVolume in a directory of that node, pinned
// there by node affinity; when such a volume is released with the Delete
// policy, a helper Pod on the node removes the directory before the volume
// is deleted.
package localpath

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	storagev1 "mini-k8s/pkg/apis/storage/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/client/workqueue"
	"mini-k8s/pkg/util/ptr"
)

// Name is the provisioner StorageClasses name.
const Name = "mini-k8s.io/local-path"

// DefaultBaseDir holds the volumes on every node (the node's /var disk).
const DefaultBaseDir = "/var/local-path-provisioner"

// Provisioner creates and deletes local-path volumes.
type Provisioner struct {
	client   *client.Client
	claims   *informers.Informer[corev1.PersistentVolumeClaim, *corev1.PersistentVolumeClaim]
	volumes  *informers.Informer[corev1.PersistentVolume, *corev1.PersistentVolume]
	classes  *informers.Informer[storagev1.StorageClass, *storagev1.StorageClass]
	recorder *record.Recorder
	queue    *workqueue.Queue
	// Namespace runs the helper Pods; HelperImage has /mini.
	Namespace, HelperImage, BaseDir string
}

// New wires the provisioner onto informers.
func New(c *client.Client, claims *informers.Informer[corev1.PersistentVolumeClaim, *corev1.PersistentVolumeClaim],
	volumes *informers.Informer[corev1.PersistentVolume, *corev1.PersistentVolume],
	classes *informers.Informer[storagev1.StorageClass, *storagev1.StorageClass], recorder *record.Recorder, namespace string) *Provisioner {
	p := &Provisioner{client: c, claims: claims, volumes: volumes, classes: classes, recorder: recorder, queue: workqueue.New(),
		Namespace: namespace, HelperImage: "mini-k8s:local", BaseDir: DefaultBaseDir}
	onClaim := func(pvc *corev1.PersistentVolumeClaim) { p.queue.Add("claim/" + informers.Key(pvc)) }
	claims.AddHandler(informers.Handler[*corev1.PersistentVolumeClaim]{
		OnAdd: onClaim, OnUpdate: func(_, pvc *corev1.PersistentVolumeClaim) { onClaim(pvc) },
	})
	onVolume := func(pv *corev1.PersistentVolume) { p.queue.Add("volume/" + pv.Name) }
	volumes.AddHandler(informers.Handler[*corev1.PersistentVolume]{
		OnAdd: onVolume, OnUpdate: func(_, pv *corev1.PersistentVolume) { onVolume(pv) },
		// A helper Pod created by a sync racing with the deletion goes too.
		OnDelete: func(pv *corev1.PersistentVolume) { p.queue.Add("deleted/" + pv.Name) },
	})
	return p
}

// Run provisions and deletes until ctx ends.
func (p *Provisioner) Run(ctx context.Context, workers int) {
	workqueue.Run(ctx, p.queue, "local-path-provisioner", workers, p.sync)
}

func (p *Provisioner) sync(ctx context.Context, key string) error {
	kind, rest, _ := strings.Cut(key, "/")
	switch kind {
	case "volume":
		return p.syncVolume(ctx, rest)
	case "deleted":
		err := client.Pods(p.client, p.Namespace).Delete(ctx, HelperPodName(rest), nil)
		if client.IsNotFound(err) {
			return nil
		}
		return err
	}
	ns, name := informers.SplitKey(rest)
	return p.syncClaim(ctx, ns, name)
}

// VolumeName is the name of the volume provisioned for a claim.
func VolumeName(pvc *corev1.PersistentVolumeClaim) string { return "pvc-" + pvc.UID }

func (p *Provisioner) syncClaim(ctx context.Context, ns, name string) error {
	pvc, ok := p.claims.Get(ns, name)
	if !ok || pvc.DeletionTimestamp != nil || pvc.Spec.VolumeName != "" || pvc.Annotations[corev1.AnnStorageProvisioner] != Name {
		return nil
	}
	sc, ok := p.classes.Get("", corev1.ClaimClass(pvc))
	if !ok || sc.Provisioner != Name {
		return nil
	}
	pvName := VolumeName(pvc)
	if _, exists := p.volumes.Get("", pvName); exists {
		return nil
	}
	node := pvc.Annotations[corev1.AnnSelectedNode]
	if node == "" {
		p.recorder.Eventf(pvc, corev1.EventTypeWarning, "ProvisioningFailed", "configuration error, no node was specified")
		return nil
	}
	p.recorder.Eventf(pvc, corev1.EventTypeNormal, "Provisioning", "External provisioner is provisioning volume for claim %q", ns+"/"+name)
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName, Annotations: map[string]string{corev1.AnnDynamicallyProvisioned: Name}},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: corev1.ClaimRequest(pvc)},
			AccessModes:                   pvc.Spec.AccessModes,
			PersistentVolumeReclaimPolicy: reclaimPolicy(sc),
			StorageClassName:              sc.Name,
			VolumeMode:                    pvc.Spec.VolumeMode,
			ClaimRef: &corev1.ObjectReference{Kind: "PersistentVolumeClaim", APIVersion: "v1", Namespace: ns, Name: name,
				UID: pvc.UID, ResourceVersion: pvc.ResourceVersion},
			NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{node}}},
			}}}},
			PersistentVolumeSource: corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{
				Path: filepath.Join(p.BaseDir, fmt.Sprintf("%s_%s_%s", pvName, ns, name)), Type: ptr.To(corev1.HostPathDirectoryOrCreate)}},
		},
	}
	if _, err := client.PersistentVolumes(p.client).Create(ctx, pv); err != nil {
		if client.IsAlreadyExists(err) {
			return nil
		}
		p.recorder.Eventf(pvc, corev1.EventTypeWarning, "ProvisioningFailed", "failed to provision volume with StorageClass %q: %v", sc.Name, err)
		return err
	}
	p.recorder.Eventf(pvc, corev1.EventTypeNormal, "ProvisioningSucceeded", "Successfully provisioned volume %s", pvName)
	return nil
}

func reclaimPolicy(sc *storagev1.StorageClass) string {
	if sc.ReclaimPolicy == nil {
		return corev1.PersistentVolumeReclaimDelete
	}
	return *sc.ReclaimPolicy
}

// HelperPodName is the name of the Pod that deletes a volume's directory.
func HelperPodName(pv string) string { return "helper-pod-delete-" + pv }

// syncVolume deletes a released volume of ours: a helper Pod on its node
// removes the directory, then the volume goes.
func (p *Provisioner) syncVolume(ctx context.Context, name string) error {
	pv, ok := p.volumes.Get("", name)
	if !ok || !p.mustDelete(pv) {
		return nil
	}
	if !p.ownDirectory(pv) {
		p.recorder.Eventf(pv, corev1.EventTypeWarning, "VolumeFailedDelete",
			"refusing to delete %s: not a directory %s created on a node", pv.Spec.HostPath.Path, Name)
		return nil
	}
	node := volumeNode(pv)
	pods := client.Pods(p.client, p.Namespace)
	helper, err := pods.Get(ctx, HelperPodName(pv.Name))
	switch {
	case client.IsNotFound(err):
		// The cache may lag behind a deletion this provisioner finished.
		if fresh, err := client.PersistentVolumes(p.client).Get(ctx, name); err != nil || !p.mustDelete(fresh) {
			return ignoreNotFound(err)
		}
		dir := filepath.Clean(pv.Spec.HostPath.Path)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: HelperPodName(pv.Name), Namespace: p.Namespace},
			Spec: corev1.PodSpec{
				NodeName: node, RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{{Name: "helper-pod", Image: p.HelperImage, ImagePullPolicy: corev1.PullNever,
					Command:      []string{"/mini", "rm", "/data/" + filepath.Base(dir)},
					VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}}},
				Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
					Path: filepath.Dir(dir), Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}}},
			},
		}
		if _, err := pods.Create(ctx, pod); err != nil && !client.IsAlreadyExists(err) {
			return err
		}
	case err != nil:
		return err
	case helper.Status.Phase == corev1.PodSucceeded:
		if err := pods.Delete(ctx, helper.Name, nil); err != nil && !client.IsNotFound(err) {
			return err
		}
		err := client.PersistentVolumes(p.client).Delete(ctx, pv.Name, nil)
		if err == nil || client.IsNotFound(err) {
			return nil
		}
		return err
	case helper.Status.Phase == corev1.PodFailed:
		p.recorder.Eventf(pv, corev1.EventTypeWarning, "VolumeFailedDelete", "failed to delete volume %s: helper Pod %s failed", pv.Name, helper.Name)
		if err := pods.Delete(ctx, helper.Name, nil); err != nil && !client.IsNotFound(err) {
			return err
		}
	}
	p.queue.AddAfter("volume/"+name, time.Second) // check on the helper Pod
	return nil
}

// mustDelete reports whether a volume is ours, released with the Delete
// policy and not being deleted already.
func (p *Provisioner) mustDelete(pv *corev1.PersistentVolume) bool {
	return pv.DeletionTimestamp == nil && pv.Annotations[corev1.AnnDynamicallyProvisioned] == Name &&
		pv.Status.Phase == corev1.VolumeReleased && pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimDelete &&
		pv.Spec.HostPath != nil
}

// ownDirectory reports whether a volume's directory is one this provisioner
// creates (<base>/<volume>_<namespace>_<claim>) on a known node; anything
// else, such as a volume someone created with our annotation and a path
// of their choosing, is never deleted.
func (p *Provisioner) ownDirectory(pv *corev1.PersistentVolume) bool {
	dir := filepath.Clean(pv.Spec.HostPath.Path)
	return filepath.Dir(dir) == filepath.Clean(p.BaseDir) && strings.HasPrefix(filepath.Base(dir), pv.Name+"_") && volumeNode(pv) != ""
}

func ignoreNotFound(err error) error {
	if client.IsNotFound(err) {
		return nil
	}
	return err
}

// volumeNode returns the node a volume's affinity pins it to.
func volumeNode(pv *corev1.PersistentVolume) string {
	if a := pv.Spec.NodeAffinity; a != nil && a.Required != nil {
		for _, t := range a.Required.NodeSelectorTerms {
			for _, r := range t.MatchExpressions {
				if r.Key == corev1.LabelHostname && r.Operator == corev1.NodeSelectorOpIn && len(r.Values) == 1 {
					return r.Values[0]
				}
			}
		}
	}
	return ""
}
