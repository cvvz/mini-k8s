// Package persistentvolume implements the PersistentVolume controller, like
// k8s.io/kubernetes/pkg/controller/volume/persistentvolume: it binds claims
// to matching volumes (or asks a provisioner for one), marks volumes whose
// claim went away Released and applies their reclaim policy.
package persistentvolume

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	storagev1 "mini-k8s/pkg/apis/storage/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/client/workqueue"
	"mini-k8s/pkg/util/deepcopy"
)

// Event messages, as the persistentvolume-controller words them.
const (
	msgWaitForConsumer = "waiting for first consumer to be created before binding"
	msgNoClass         = "no persistent volumes available for this claim and no storage class is set"
)

// Controller binds claims and volumes.
type Controller struct {
	client   *client.Client
	volumes  *informers.Informer[corev1.PersistentVolume, *corev1.PersistentVolume]
	claims   *informers.Informer[corev1.PersistentVolumeClaim, *corev1.PersistentVolumeClaim]
	classes  *informers.Informer[storagev1.StorageClass, *storagev1.StorageClass]
	recorder *record.Recorder
	queue    *workqueue.Queue
	// Resync re-examines everything periodically (15s, Kubernetes'
	// pvClaimBinderSyncPeriod), which also repeats waiting events.
	Resync time.Duration
}

// NewController wires the controller onto shared informers.
func NewController(c *client.Client, volumes *informers.Informer[corev1.PersistentVolume, *corev1.PersistentVolume],
	claims *informers.Informer[corev1.PersistentVolumeClaim, *corev1.PersistentVolumeClaim],
	classes *informers.Informer[storagev1.StorageClass, *storagev1.StorageClass], recorder *record.Recorder) *Controller {
	pc := &Controller{client: c, volumes: volumes, claims: claims, classes: classes, recorder: recorder,
		queue: workqueue.New(), Resync: 15 * time.Second}
	onClaim := func(pvc *corev1.PersistentVolumeClaim) {
		pc.queue.Add(claimKey(pvc.Namespace, pvc.Name))
		if pvc.Spec.VolumeName != "" {
			pc.queue.Add(volumeKey(pvc.Spec.VolumeName))
		}
	}
	claims.AddHandler(informers.Handler[*corev1.PersistentVolumeClaim]{
		OnAdd: onClaim, OnUpdate: func(_, pvc *corev1.PersistentVolumeClaim) { onClaim(pvc) },
		// A deleted claim releases its volume (or one reserved for it).
		OnDelete: func(pvc *corev1.PersistentVolumeClaim) {
			for _, pv := range volumes.List("", nil) {
				if ref := pv.Spec.ClaimRef; ref != nil && ref.Namespace == pvc.Namespace && ref.Name == pvc.Name {
					pc.queue.Add(volumeKey(pv.Name))
				}
			}
		},
	})
	onVolume := func(pv *corev1.PersistentVolume) {
		pc.queue.Add(volumeKey(pv.Name))
		if ref := pv.Spec.ClaimRef; ref != nil {
			pc.queue.Add(claimKey(ref.Namespace, ref.Name))
		}
		pc.enqueuePendingClaims("")
	}
	volumes.AddHandler(informers.Handler[*corev1.PersistentVolume]{
		OnAdd: onVolume, OnUpdate: func(_, pv *corev1.PersistentVolume) { onVolume(pv) }, OnDelete: onVolume,
	})
	classes.AddHandler(informers.Handler[*storagev1.StorageClass]{
		OnAdd: func(sc *storagev1.StorageClass) { pc.enqueuePendingClaims(sc.Name) },
	})
	return pc
}

func claimKey(ns, name string) string { return "claim/" + ns + "/" + name }
func volumeKey(name string) string    { return "volume/" + name }

func (pc *Controller) enqueuePendingClaims(class string) {
	for _, pvc := range pc.claims.List("", nil) {
		if pvc.Spec.VolumeName == "" && (class == "" || corev1.ClaimClass(pvc) == class) {
			pc.queue.Add(claimKey(pvc.Namespace, pvc.Name))
		}
	}
}

// Run processes claims and volumes until ctx ends.
func (pc *Controller) Run(ctx context.Context, workers int) {
	go func() {
		ticker := time.NewTicker(pc.Resync)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, pvc := range pc.claims.List("", nil) {
					pc.queue.Add(claimKey(pvc.Namespace, pvc.Name))
				}
				for _, pv := range pc.volumes.List("", nil) {
					pc.queue.Add(volumeKey(pv.Name))
				}
			}
		}
	}()
	workqueue.Run(ctx, pc.queue, "persistentvolume", workers, pc.sync)
}

func (pc *Controller) sync(ctx context.Context, key string) error {
	kind, rest, _ := strings.Cut(key, "/")
	if kind == "volume" {
		return pc.syncVolume(ctx, rest)
	}
	ns, name, _ := strings.Cut(rest, "/")
	return pc.syncClaim(ctx, ns, name)
}

func (pc *Controller) syncClaim(ctx context.Context, ns, name string) error {
	pvc, ok := pc.claims.Get(ns, name)
	if !ok || pvc.DeletionTimestamp != nil {
		return nil
	}
	if pvc.Spec.VolumeName != "" {
		return pc.syncBoundClaim(ctx, pvc)
	}
	class := corev1.ClaimClass(pvc)
	var sc *storagev1.StorageClass
	if class != "" {
		var found bool
		if sc, found = pc.classes.Get("", class); !found {
			sc = nil
		}
	}
	delayed := sc != nil && storagev1.WaitsForConsumer(sc)
	if pv := pc.findVolume(pvc, delayed); pv != nil {
		return pc.bind(ctx, pvc, pv)
	}
	switch {
	case class == "":
		pc.recorder.Eventf(pvc, corev1.EventTypeNormal, "FailedBinding", "%s", msgNoClass)
	case sc == nil:
		pc.recorder.Eventf(pvc, corev1.EventTypeWarning, "ProvisioningFailed", "storageclass.storage.k8s.io %q not found", class)
	case delayed && pvc.Annotations[corev1.AnnSelectedNode] == "":
		pc.recorder.Eventf(pvc, corev1.EventTypeNormal, "WaitForFirstConsumer", "%s", msgWaitForConsumer)
	default:
		if pvc.Annotations[corev1.AnnStorageProvisioner] != sc.Provisioner {
			cp := deepcopy.Copy(pvc)
			if cp.Annotations == nil {
				cp.Annotations = map[string]string{}
			}
			cp.Annotations[corev1.AnnStorageProvisioner] = sc.Provisioner
			cp.Annotations[corev1.AnnBetaStorageProvisioner] = sc.Provisioner
			if _, err := client.PersistentVolumeClaims(pc.client, ns).Update(ctx, cp); err != nil {
				return ignoreGone(err)
			}
		}
		pc.recorder.Eventf(pvc, corev1.EventTypeNormal, "ExternalProvisioning",
			"Waiting for a volume to be created either by the external provisioner '%s' or manually by the system administrator. "+
				"If volume creation is delayed, please verify that the provisioner is running and correctly registered.", sc.Provisioner)
	}
	return nil
}

// findVolume returns the volume reserved for the claim (its claimRef
// names it), or else, unless binding waits for a consumer, the smallest
// available volume that satisfies it.
func (pc *Controller) findVolume(pvc *corev1.PersistentVolumeClaim, delayed bool) *corev1.PersistentVolume {
	var candidates []*corev1.PersistentVolume
	for _, pv := range pc.volumes.List("", nil) {
		if pv.DeletionTimestamp != nil || !corev1.VolumeSatisfiesClaim(pv, pvc) {
			continue
		}
		if ref := pv.Spec.ClaimRef; ref != nil {
			if ref.Namespace == pvc.Namespace && ref.Name == pvc.Name && (ref.UID == "" || ref.UID == pvc.UID) {
				return pv
			}
			continue
		}
		if pv.Status.Phase == corev1.VolumeAvailable && !delayed {
			candidates = append(candidates, pv)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i].Spec.Capacity[corev1.ResourceStorage], candidates[j].Spec.Capacity[corev1.ResourceStorage]
		if c := a.Cmp(b); c != 0 {
			return c < 0
		}
		return candidates[i].Name < candidates[j].Name
	})
	if len(candidates) == 0 {
		return nil
	}
	return candidates[0]
}

// bind points the volume and the claim at each other and marks both Bound.
func (pc *Controller) bind(ctx context.Context, pvc *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume) error {
	if ref := pv.Spec.ClaimRef; ref == nil || ref.UID != pvc.UID {
		cp := deepcopy.Copy(pv)
		if ref == nil {
			if cp.Annotations == nil {
				cp.Annotations = map[string]string{}
			}
			cp.Annotations[corev1.AnnBoundByController] = "yes"
		}
		cp.Spec.ClaimRef = &corev1.ObjectReference{Kind: "PersistentVolumeClaim", APIVersion: "v1", Namespace: pvc.Namespace,
			Name: pvc.Name, UID: pvc.UID, ResourceVersion: pvc.ResourceVersion}
		updated, err := client.PersistentVolumes(pc.client).Update(ctx, cp)
		if err != nil {
			return ignoreGone(err)
		}
		pv = updated
	}
	if err := pc.setVolumePhase(ctx, pv, corev1.VolumeBound, "", ""); err != nil {
		return err
	}
	claims := client.PersistentVolumeClaims(pc.client, pvc.Namespace)
	if pvc.Spec.VolumeName != pv.Name || pvc.Annotations[corev1.AnnBindCompleted] != "yes" {
		cp := deepcopy.Copy(pvc)
		if cp.Annotations == nil {
			cp.Annotations = map[string]string{}
		}
		if cp.Spec.VolumeName == "" {
			cp.Annotations[corev1.AnnBoundByController] = "yes"
		}
		cp.Spec.VolumeName = pv.Name
		cp.Annotations[corev1.AnnBindCompleted] = "yes"
		updated, err := claims.Update(ctx, cp)
		if err != nil {
			return ignoreGone(err)
		}
		pvc = updated
	}
	if pvc.Status.Phase == corev1.ClaimBound {
		return nil
	}
	cp := deepcopy.Copy(pvc)
	cp.Status = corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, AccessModes: pv.Spec.AccessModes, Capacity: pv.Spec.Capacity}
	_, err := claims.UpdateStatus(ctx, cp)
	return ignoreGone(err)
}

// syncBoundClaim handles a claim naming its volume: it completes the
// binding, or reports a volume that is missing or taken.
func (pc *Controller) syncBoundClaim(ctx context.Context, pvc *corev1.PersistentVolumeClaim) error {
	pv, ok := pc.volumes.Get("", pvc.Spec.VolumeName)
	if !ok {
		if pvc.Annotations[corev1.AnnBindCompleted] == "yes" && pvc.Status.Phase != corev1.ClaimLost {
			cp := deepcopy.Copy(pvc)
			cp.Status.Phase = corev1.ClaimLost
			if _, err := client.PersistentVolumeClaims(pc.client, pvc.Namespace).UpdateStatus(ctx, cp); err != nil {
				return ignoreGone(err)
			}
			pc.recorder.Eventf(pvc, corev1.EventTypeWarning, "ClaimLost", "Bound claim has lost its PersistentVolume. Data on the volume is lost!")
		}
		return nil
	}
	if ref := pv.Spec.ClaimRef; ref != nil && (ref.Namespace != pvc.Namespace || ref.Name != pvc.Name || (ref.UID != "" && ref.UID != pvc.UID)) {
		pc.recorder.Eventf(pvc, corev1.EventTypeWarning, "FailedBinding", "volume %q already bound to a different claim.", pv.Name)
		return nil
	}
	if pv.Spec.ClaimRef == nil && !corev1.VolumeSatisfiesClaim(pv, pvc) {
		pc.recorder.Eventf(pvc, corev1.EventTypeWarning, "FailedBinding", "Cannot bind to requested volume %q: it does not satisfy the claim", pv.Name)
		return nil
	}
	return pc.bind(ctx, pvc, pv)
}

// syncVolume keeps a volume's phase in line with its claim and reclaims
// volumes whose claim is gone.
func (pc *Controller) syncVolume(ctx context.Context, name string) error {
	// Deleted volumes still move to Released, which lets pv-protection
	// remove its finalizer.
	pv, ok := pc.volumes.Get("", name)
	if !ok {
		return nil
	}
	ref := pv.Spec.ClaimRef
	if ref == nil {
		if pv.Status.Phase != corev1.VolumeAvailable && pv.Status.Phase != corev1.VolumeFailed {
			return pc.setVolumePhase(ctx, pv, corev1.VolumeAvailable, "", "")
		}
		return nil
	}
	pvc, ok := pc.claims.Get(ref.Namespace, ref.Name)
	if ok && (ref.UID == "" || ref.UID == pvc.UID) {
		if ref.UID != "" && pvc.Spec.VolumeName == pv.Name {
			return pc.setVolumePhase(ctx, pv, corev1.VolumeBound, "", "")
		}
		if pvc.Spec.VolumeName != "" && pv.Annotations[corev1.AnnBoundByController] == "yes" {
			// Reserved for a claim that got another volume: free it.
			cp := deepcopy.Copy(pv)
			cp.Spec.ClaimRef = nil
			delete(cp.Annotations, corev1.AnnBoundByController)
			_, err := client.PersistentVolumes(pc.client).Update(ctx, cp)
			return ignoreGone(err)
		}
		pc.queue.Add(claimKey(pvc.Namespace, pvc.Name)) // reserved for it: the claim binds
		if pv.Status.Phase == corev1.VolumePending {
			return pc.setVolumePhase(ctx, pv, corev1.VolumeAvailable, "", "")
		}
		return nil
	}
	if ref.UID == "" { // reserved for a claim that does not exist yet
		if pv.Status.Phase == corev1.VolumePending {
			return pc.setVolumePhase(ctx, pv, corev1.VolumeAvailable, "", "")
		}
		return nil
	}
	// The claim was deleted.
	switch pv.Status.Phase {
	case corev1.VolumeReleased, corev1.VolumeFailed:
	default:
		if err := pc.setVolumePhase(ctx, pv, corev1.VolumeReleased, "", ""); err != nil {
			return err
		}
	}
	switch pv.Spec.PersistentVolumeReclaimPolicy {
	case corev1.PersistentVolumeReclaimDelete:
		if pv.Annotations[corev1.AnnDynamicallyProvisioned] != "" {
			return nil // the provisioner deletes the storage and the volume
		}
		msg := fmt.Sprintf("error getting deleter volume plugin for volume %q: no deletable volume plugin matched", pv.Name)
		if pv.Status.Phase != corev1.VolumeFailed {
			pc.recorder.Eventf(pv, corev1.EventTypeWarning, "VolumeFailedDelete", "%s", msg)
		}
		return pc.setVolumePhase(ctx, pv, corev1.VolumeFailed, "", msg)
	case corev1.PersistentVolumeReclaimRecycle:
		msg := "recycling volumes is not supported"
		if pv.Status.Phase != corev1.VolumeFailed {
			pc.recorder.Eventf(pv, corev1.EventTypeWarning, "VolumeFailedRecycle", "%s", msg)
		}
		return pc.setVolumePhase(ctx, pv, corev1.VolumeFailed, "", msg)
	}
	return nil
}

func (pc *Controller) setVolumePhase(ctx context.Context, pv *corev1.PersistentVolume, phase, reason, message string) error {
	if pv.Status.Phase == phase && pv.Status.Message == message {
		return nil
	}
	cp := deepcopy.Copy(pv)
	if cp.Status.Phase != phase {
		now := metav1.Now()
		cp.Status.LastPhaseTransitionTime = &now
	}
	cp.Status.Phase, cp.Status.Reason, cp.Status.Message = phase, reason, message
	_, err := client.PersistentVolumes(pc.client).UpdateStatus(ctx, cp)
	return ignoreGone(err)
}

// ignoreGone drops errors a later sync resolves on its own: the object
// went away or changed meanwhile (the informer's update requeues it).
func ignoreGone(err error) error {
	if client.IsNotFound(err) || client.IsConflict(err) {
		return nil
	}
	return err
}
