package controlplane

import (
	"context"
	"fmt"
	"testing"
	"time"

	"mini-k8s/pkg/api/resource"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	storagev1 "mini-k8s/pkg/apis/storage/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/pkg/client/informers"
	"mini-k8s/pkg/client/record"
	"mini-k8s/pkg/controller/volume/persistentvolume"
	"mini-k8s/pkg/controller/volume/pvcprotection"
	"mini-k8s/pkg/controller/volume/pvprotection"
	"mini-k8s/pkg/scheduler"
	"mini-k8s/pkg/util/ptr"
	"mini-k8s/pkg/volume/localpath"
	"mini-k8s/test/integration/framework"
)

func startVolumeControllers(t *testing.T, c *client.Client) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rec := record.New(c, "persistentvolume-controller", "")
	go rec.Run(ctx)
	volumes := informers.New(client.PersistentVolumes(c), client.ListOptions{}, 0)
	claims := informers.New(client.PersistentVolumeClaims(c, ""), client.ListOptions{}, 0)
	classes := informers.New(client.StorageClasses(c), client.ListOptions{}, 0)
	pods := informers.New(client.Pods(c, ""), client.ListOptions{}, 0)
	binder := persistentvolume.NewController(c, volumes, claims, classes, rec)
	binder.Resync = 500 * time.Millisecond
	pvcProt := pvcprotection.NewController(c, claims, pods)
	pvProt := pvprotection.NewController(c, volumes)
	for _, run := range []func(context.Context){volumes.Run, claims.Run, classes.Run, pods.Run} {
		go run(ctx)
	}
	informers.WaitForSync(ctx, volumes.HasSynced, claims.HasSynced, classes.HasSynced, pods.HasSynced)
	go binder.Run(ctx, 2)
	go pvcProt.Run(ctx, 1)
	go pvProt.Run(ctx, 1)
	return ctx
}

func hostPathPV(name, size, class, policy string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.PersistentVolumeSpec{
		Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}, AccessModes: []string{corev1.ReadWriteOnce},
		StorageClassName: class, PersistentVolumeReclaimPolicy: policy,
		PersistentVolumeSource: corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/data/" + name}}}}
}

func claim(name, size string, class *string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.PersistentVolumeClaimSpec{
		AccessModes: []string{corev1.ReadWriteOnce}, StorageClassName: class,
		Resources: corev1.VolumeResources{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}}}}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func hasEvent(ctx context.Context, c *client.Client, name, reason, message string) bool {
	events, err := client.Events(c, "default").List(ctx, client.ListOptions{})
	if err != nil {
		return false
	}
	for _, e := range events.Items {
		if e.InvolvedObject.Name == name && e.Reason == reason && (message == "" || e.Message == message) {
			return true
		}
	}
	return false
}

func TestStaticBindingReleaseAndProtection(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := startVolumeControllers(t, c)
	pvs, pvcs := client.PersistentVolumes(c), client.PersistentVolumeClaims(c, "default")
	for _, pv := range []*corev1.PersistentVolume{hostPathPV("big", "5Gi", "", ""), hostPathPV("small", "2Gi", "", ""),
		hostPathPV("tiny", "500Mi", "", "")} {
		if _, err := pvs.Create(ctx, pv); err != nil {
			t.Fatal(err)
		}
	}
	created, err := pvs.Get(ctx, "big")
	if err != nil || created.Spec.PersistentVolumeReclaimPolicy != "Retain" || !created.HasFinalizer(corev1.PVProtectionFinalizer) {
		t.Fatalf("PV defaults: %+v %v", created, err)
	}
	if _, err := pvcs.Create(ctx, claim("data", "1Gi", ptr.To(""))); err != nil {
		t.Fatal(err)
	}
	var pvc *corev1.PersistentVolumeClaim
	waitFor(t, "the claim to bind", func() bool {
		pvc, err = pvcs.Get(ctx, "data")
		return err == nil && pvc.Status.Phase == corev1.ClaimBound
	})
	q := pvc.Status.Capacity[corev1.ResourceStorage]
	if pvc.Spec.VolumeName != "small" || q.String() != "2Gi" || pvc.Annotations[corev1.AnnBindCompleted] != "yes" ||
		pvc.Annotations[corev1.AnnBoundByController] != "yes" || !pvc.HasFinalizer(corev1.PVCProtectionFinalizer) {
		t.Fatalf("the smallest fitting volume binds: %+v", pvc)
	}
	pv, _ := pvs.Get(ctx, "small")
	if pv.Status.Phase != corev1.VolumeBound || pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != pvc.UID ||
		pv.Annotations[corev1.AnnBoundByController] != "yes" {
		t.Fatalf("volume side: %+v", pv)
	}
	waitFor(t, "unbound volumes to become Available", func() bool {
		pv, err := pvs.Get(ctx, "big")
		return err == nil && pv.Status.Phase == corev1.VolumeAvailable
	})

	// Protecting a claim a Pod uses.
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "user"}, Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: "c", Image: "img"}},
		Volumes:    []corev1.Volume{{Name: "d", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}}}}
	if _, err := client.Pods(c, "default").Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := pvcs.Delete(ctx, "data", nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if pvc, err := pvcs.Get(ctx, "data"); err != nil || pvc.DeletionTimestamp == nil {
		t.Fatalf("a claim in use stays Terminating: %v %+v", err, pvc)
	}
	if err := client.Pods(c, "default").Delete(ctx, "user", &metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the claim to go once unused", func() bool {
		_, err := pvcs.Get(ctx, "data")
		return client.IsNotFound(err)
	})
	waitFor(t, "the volume to be Released", func() bool {
		pv, err := pvs.Get(ctx, "small")
		return err == nil && pv.Status.Phase == corev1.VolumeReleased
	})

	// Failing to delete a volume no provisioner made.
	if _, err := pvs.Create(ctx, hostPathPV("doomed", "1Gi", "", "Delete")); err != nil {
		t.Fatal(err)
	}
	if _, err := pvcs.Create(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "short"}, Spec: corev1.PersistentVolumeClaimSpec{
		AccessModes: []string{corev1.ReadWriteOnce}, StorageClassName: ptr.To(""), VolumeName: "doomed",
		Resources: corev1.VolumeResources{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the pre-bound claim to bind", func() bool {
		pv, err := pvs.Get(ctx, "doomed")
		return err == nil && pv.Status.Phase == corev1.VolumeBound
	})
	if err := pvcs.Delete(ctx, "short", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the volume to fail", func() bool {
		pv, err := pvs.Get(ctx, "doomed")
		return err == nil && pv.Status.Phase == corev1.VolumeFailed &&
			pv.Status.Message == `error getting deleter volume plugin for volume "doomed": no deletable volume plugin matched`
	})

	// Deleting a bound volume waits for its claim.
	if _, err := pvcs.Create(ctx, claim("keep", "4Gi", ptr.To(""))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "keep to bind big", func() bool {
		pv, err := pvs.Get(ctx, "big")
		return err == nil && pv.Status.Phase == corev1.VolumeBound
	})
	if err := pvs.Delete(ctx, "big", nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if pv, err := pvs.Get(ctx, "big"); err != nil || pv.DeletionTimestamp == nil {
		t.Fatalf("a bound volume stays Terminating: %v", err)
	}
	if err := pvcs.Delete(ctx, "keep", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the volume to go with its claim", func() bool {
		_, err := pvs.Get(ctx, "big")
		return client.IsNotFound(err)
	})
}

// Pods sharing a claim that waits for its first consumer all land on the
// node its volume is provisioned on, also when scheduled back to back; a
// volume reserved for a claim that got another one is freed again.
func TestPodsSharingAClaimLandTogether(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := startVolumeControllers(t, c)
	for _, n := range []string{"worker-1", "worker-2"} {
		if _, err := client.Nodes(c).Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n,
			Labels: map[string]string{corev1.LabelHostname: n}}}); err != nil {
			t.Fatal(err)
		}
		go heartbeat(ctx, c, n)
	}
	rec := record.New(c, "test", "")
	go rec.Run(ctx)
	pods := informers.New(client.Pods(c, ""), client.ListOptions{}, 0)
	nodes := informers.New(client.Nodes(c), client.ListOptions{}, 0)
	volumes := informers.New(client.PersistentVolumes(c), client.ListOptions{}, 0)
	claims := informers.New(client.PersistentVolumeClaims(c, ""), client.ListOptions{}, 0)
	classes := informers.New(client.StorageClasses(c), client.ListOptions{}, 0)
	for _, run := range []func(context.Context){pods.Run, nodes.Run, volumes.Run, claims.Run, classes.Run} {
		go run(ctx)
	}
	informers.WaitForSync(ctx, pods.HasSynced, nodes.HasSynced, volumes.HasSynced, claims.HasSynced, classes.HasSynced)
	waitFor(t, "allocatable nodes", func() bool {
		list := nodes.List("", nil)
		return len(list) == 2 && len(list[0].Status.Allocatable) > 0 && len(list[1].Status.Allocatable) > 0
	})
	go scheduler.New(c, pods, nodes, rec).WithVolumes(volumes, claims, classes).Run(ctx)
	go localpath.New(c, claims, volumes, classes, rec, "default").Run(ctx, 1)
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "standard"}, Provisioner: localpath.Name,
		VolumeBindingMode: ptr.To(storagev1.VolumeBindingWaitForFirstConsumer)}
	if _, err := client.StorageClasses(c).Create(ctx, sc); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PersistentVolumeClaims(c, "default").Create(ctx, claim("shared", "1Gi", ptr.To("standard"))); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("user-%d", i)}, Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "img"}},
			Volumes: []corev1.Volume{{Name: "d", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "shared"}}}}}}
		if _, err := client.Pods(c, "default").Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	placed := map[string]int{}
	waitFor(t, "every Pod to be bound", func() bool {
		list, err := client.Pods(c, "default").List(ctx, client.ListOptions{})
		if err != nil {
			return false
		}
		placed = map[string]int{}
		for _, p := range list.Items {
			if p.Spec.NodeName == "" {
				return false
			}
			placed[p.Spec.NodeName]++
		}
		return len(list.Items) == 4
	})
	pvc, err := client.PersistentVolumeClaims(c, "default").Get(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if len(placed) != 1 || placed[pvc.Annotations[corev1.AnnSelectedNode]] != 4 {
		t.Fatalf("Pods sharing a claim must share its node: %v, claim on %q", placed, pvc.Annotations[corev1.AnnSelectedNode])
	}

	// A volume reserved for a claim that is bound elsewhere is freed.
	other := hostPathPV("other", "1Gi", "", "")
	if _, err := client.PersistentVolumes(c).Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	bound, err := client.PersistentVolumeClaims(c, "default").Create(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "picky"},
		Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []string{corev1.ReadWriteOnce}, StorageClassName: ptr.To(""), VolumeName: "other",
			Resources: corev1.VolumeResources{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}})
	if err != nil {
		t.Fatal(err)
	}
	reserved := hostPathPV("reserved", "1Gi", "", "")
	reserved.Annotations = map[string]string{corev1.AnnBoundByController: "yes"}
	reserved.Spec.ClaimRef = &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: "default", Name: "picky", UID: bound.UID}
	if _, err := client.PersistentVolumes(c).Create(ctx, reserved); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the reservation to be dropped", func() bool {
		pv, err := client.PersistentVolumes(c).Get(ctx, "reserved")
		return err == nil && pv.Spec.ClaimRef == nil && pv.Status.Phase == corev1.VolumeAvailable
	})
}

func TestDelayedBindingAndProvisioning(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := startVolumeControllers(t, c)
	wffc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "local",
		Annotations: map[string]string{storagev1.AnnDefaultStorageClass: "true"}},
		Provisioner: "example.com/local", VolumeBindingMode: ptr.To(storagev1.VolumeBindingWaitForFirstConsumer)}
	if _, err := client.StorageClasses(c).Create(ctx, wffc); err != nil {
		t.Fatal(err)
	}
	pvcs := client.PersistentVolumeClaims(c, "default")
	pvc, err := pvcs.Create(ctx, claim("data", "1Gi", nil))
	if err != nil || corev1.ClaimClass(pvc) != "local" || *pvc.Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
		t.Fatalf("a claim without a class gets the default class: %v %+v", err, pvc.Spec)
	}
	waitFor(t, "WaitForFirstConsumer", func() bool {
		return hasEvent(ctx, c, "data", "WaitForFirstConsumer", "waiting for first consumer to be created before binding")
	})
	if _, err := pvcs.Create(ctx, claim("lost", "1Gi", ptr.To("nonexistent"))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "ProvisioningFailed", func() bool {
		return hasEvent(ctx, c, "lost", "ProvisioningFailed", `storageclass.storage.k8s.io "nonexistent" not found`)
	})

	// The scheduler picks a node: the claim is handed to the provisioner.
	pvc.Annotations = map[string]string{corev1.AnnSelectedNode: "worker-1"}
	if pvc, err = pvcs.Update(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the provisioner annotation", func() bool {
		cur, err := pvcs.Get(ctx, "data")
		return err == nil && cur.Annotations[corev1.AnnStorageProvisioner] == "example.com/local" &&
			cur.Annotations[corev1.AnnBetaStorageProvisioner] == "example.com/local"
	})
	waitFor(t, "ExternalProvisioning", func() bool { return hasEvent(ctx, c, "data", "ExternalProvisioning", "") })

	// What the provisioner creates: a volume reserved for the claim.
	pv := hostPathPV("pvc-"+pvc.UID, "1Gi", "local", "Delete")
	pv.Spec.ClaimRef = &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: "default", Name: "data", UID: pvc.UID}
	pv.Annotations = map[string]string{corev1.AnnDynamicallyProvisioned: "example.com/local"}
	if _, err := client.PersistentVolumes(c).Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the provisioned volume to bind", func() bool {
		cur, err := pvcs.Get(ctx, "data")
		return err == nil && cur.Status.Phase == corev1.ClaimBound && cur.Spec.VolumeName == pv.Name
	})
	if err := pvcs.Delete(ctx, "data", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a provisioned volume to stay Released for its provisioner", func() bool {
		cur, err := client.PersistentVolumes(c).Get(ctx, pv.Name)
		return err == nil && cur.Status.Phase == corev1.VolumeReleased
	})
}
