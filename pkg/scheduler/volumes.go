package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	storagev1 "mini-k8s/pkg/apis/storage/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/scheduler/framework/plugins"
	"mini-k8s/pkg/util/deepcopy"
)

// volumeCaches serves the VolumeBinding plugin from informers and the
// scheduler's claim assumptions.
type volumeCaches struct {
	s       *Scheduler
	volumes *informers.Informer[corev1.PersistentVolume, *corev1.PersistentVolume]
	claims  *informers.Informer[corev1.PersistentVolumeClaim, *corev1.PersistentVolumeClaim]
	classes *informers.Informer[storagev1.StorageClass, *storagev1.StorageClass]
}

func (c volumeCaches) NodeExists(name string) bool {
	_, ok := c.s.nodes.Get("", name)
	return ok
}

func (c volumeCaches) Assumed(ns, name string) (plugins.ClaimAssumption, bool) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	a, ok := c.s.claimAssumptions[ns+"/"+name]
	return a, ok
}

func (c volumeCaches) Claim(ns, name string) (*corev1.PersistentVolumeClaim, bool) {
	return c.claims.Get(ns, name)
}
func (c volumeCaches) Volume(name string) (*corev1.PersistentVolume, bool) {
	return c.volumes.Get("", name)
}
func (c volumeCaches) Volumes() []*corev1.PersistentVolume { return c.volumes.List("", nil) }
func (c volumeCaches) Class(name string) (*storagev1.StorageClass, bool) {
	return c.classes.Get("", name)
}

// WithVolumes enables the VolumeBinding plugin on shared informers: Pods
// with claims are placed where their volumes can be used, and claims
// waiting for their first consumer are bound (or provisioned) for the
// chosen node before the Pod is bound. Changes to claims, volumes and
// classes retry pending Pods.
func (s *Scheduler) WithVolumes(volumes *informers.Informer[corev1.PersistentVolume, *corev1.PersistentVolume],
	claims *informers.Informer[corev1.PersistentVolumeClaim, *corev1.PersistentVolumeClaim],
	classes *informers.Informer[storagev1.StorageClass, *storagev1.StorageClass]) *Scheduler {
	vb := plugins.VolumeBinding{Lister: volumeCaches{s: s, volumes: volumes, claims: claims, classes: classes}}
	s.volumeBinding = &vb
	s.framework.PreFilters = append(s.framework.PreFilters, vb)
	s.framework.Filters = append(s.framework.Filters, vb)
	// An assumption ends once the cache shows the claim bound (or gone).
	settle := func(pvc *corev1.PersistentVolumeClaim, gone bool) {
		if gone || pvc.Spec.VolumeName != "" {
			s.mu.Lock()
			delete(s.claimAssumptions, informers.Key(pvc))
			s.mu.Unlock()
		}
		s.retryPending()
	}
	claims.AddHandler(informers.Handler[*corev1.PersistentVolumeClaim]{
		OnAdd:    func(pvc *corev1.PersistentVolumeClaim) { settle(pvc, false) },
		OnUpdate: func(_, pvc *corev1.PersistentVolumeClaim) { settle(pvc, false) },
		OnDelete: func(pvc *corev1.PersistentVolumeClaim) { settle(pvc, true) },
	})
	volumes.AddHandler(informers.Handler[*corev1.PersistentVolume]{
		OnAdd: func(*corev1.PersistentVolume) { s.retryPending() }, OnUpdate: func(_, _ *corev1.PersistentVolume) { s.retryPending() },
	})
	classes.AddHandler(informers.Handler[*storagev1.StorageClass]{OnAdd: func(*storagev1.StorageClass) { s.retryPending() }})
	return s
}

// bindVolumesAndPod is the binding cycle of a Pod whose claims still need
// volumes (PreBind of the VolumeBinding plugin, then Bind): it reserves
// the chosen static volumes or asks for provisioning on the node, waits
// until every claim is bound and binds the Pod. It runs on its own so
// that waiting for a provisioner does not hold up other Pods.
func (s *Scheduler) bindVolumesAndPod(ctx context.Context, pod *corev1.Pod, node string, bindings []plugins.ClaimBinding) {
	uid := pod.UID
	defer func() {
		s.mu.Lock()
		delete(s.binding, uid)
		s.mu.Unlock()
	}()
	err := s.preBindVolumes(ctx, pod, node, bindings)
	if err == nil {
		err = s.bind(ctx, pod, node)
		if err == nil || client.IsNotFound(err) || client.IsConflict(err) {
			return // bound, or the Pod is gone or was bound meanwhile
		}
	}
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	delete(s.assumed, uid)
	for _, b := range bindings {
		delete(s.claimAssumptions, informers.Key(b.Claim))
	}
	s.attempts[uid]++
	delay := Backoff(s.attempts[uid])
	s.mu.Unlock()
	if cur, ok := s.pods.Get(pod.Namespace, pod.Name); ok && cur.UID == uid {
		s.markUnschedulable(ctx, cur, fmt.Sprintf("running PreBind plugin %q: binding volumes: %v", "VolumeBinding", err))
		s.queue.AddAfter(informers.Key(cur), delay)
	}
}

// assumeClaims records where the claims of a Pod being bound go, so that
// Pods sharing them are scheduled to the same place meanwhile.
func (s *Scheduler) assumeClaims(node string, bindings []plugins.ClaimBinding) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range bindings {
		a := plugins.ClaimAssumption{Node: node}
		if b.Volume != nil {
			a = plugins.ClaimAssumption{Volume: b.Volume.Name}
		}
		s.claimAssumptions[informers.Key(b.Claim)] = a
	}
}

func (s *Scheduler) preBindVolumes(ctx context.Context, pod *corev1.Pod, node string, bindings []plugins.ClaimBinding) error {
	for _, b := range bindings {
		var err error
		if b.Volume != nil {
			err = s.reserveVolume(ctx, b.Volume, b.Claim)
		} else {
			err = s.selectNode(ctx, b.Claim, node)
		}
		if err != nil {
			return err
		}
	}
	deadline := time.Now().Add(s.BindTimeout)
	for {
		claims, reason := s.volumeBinding.Claims(pod)
		if reason != "" {
			return errors.New(reason)
		}
		bound := true
		for _, pvc := range claims {
			bound = bound && pvc.Spec.VolumeName != "" && pvc.Status.Phase == corev1.ClaimBound
		}
		if bound {
			return s.checkVolumesFit(claims, node)
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for the condition")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// checkVolumesFit makes sure the volumes the claims got can be used from
// the node (another scheduler decision may have bound them elsewhere).
func (s *Scheduler) checkVolumesFit(claims []*corev1.PersistentVolumeClaim, node string) error {
	n, ok := s.nodes.Get("", node)
	if !ok {
		return fmt.Errorf("node %q not found", node)
	}
	for _, pvc := range claims {
		pv, ok := s.volumeBinding.Lister.Volume(pvc.Spec.VolumeName)
		if !ok || !corev1.VolumeFitsNode(pv, n) {
			return fmt.Errorf("volume %q of claim %q cannot be used from node %q", pvc.Spec.VolumeName, pvc.Name, node)
		}
	}
	return nil
}

// reserveVolume points a static volume at the claim; the PersistentVolume
// controller completes the binding.
func (s *Scheduler) reserveVolume(ctx context.Context, pv *corev1.PersistentVolume, pvc *corev1.PersistentVolumeClaim) error {
	pvs := client.PersistentVolumes(s.client)
	cur, err := pvs.Get(ctx, pv.Name)
	if err != nil {
		return err
	}
	if ref := cur.Spec.ClaimRef; ref != nil {
		if ref.Namespace == pvc.Namespace && ref.Name == pvc.Name {
			return nil
		}
		return fmt.Errorf("volume %q was taken by another claim", pv.Name)
	}
	cp := deepcopy.Copy(cur)
	if cp.Annotations == nil {
		cp.Annotations = map[string]string{}
	}
	cp.Annotations[corev1.AnnBoundByController] = "yes"
	cp.Spec.ClaimRef = &corev1.ObjectReference{Kind: "PersistentVolumeClaim", APIVersion: "v1", Namespace: pvc.Namespace,
		Name: pvc.Name, UID: pvc.UID, ResourceVersion: pvc.ResourceVersion}
	_, err = pvs.Update(ctx, cp)
	return err
}

// selectNode tells the provisioner where the claim's volume is needed.
func (s *Scheduler) selectNode(ctx context.Context, pvc *corev1.PersistentVolumeClaim, node string) error {
	claims := client.PersistentVolumeClaims(s.client, pvc.Namespace)
	cur, err := claims.Get(ctx, pvc.Name)
	if err != nil {
		return err
	}
	switch selected := cur.Annotations[corev1.AnnSelectedNode]; {
	case selected == node:
		return nil
	case selected != "":
		if _, exists := s.nodes.Get("", selected); exists {
			return fmt.Errorf("claim %q is selected for node %q", pvc.Name, selected)
		}
	}
	cp := deepcopy.Copy(cur)
	if cp.Annotations == nil {
		cp.Annotations = map[string]string{}
	}
	cp.Annotations[corev1.AnnSelectedNode] = node
	_, err = claims.Update(ctx, cp)
	return err
}
