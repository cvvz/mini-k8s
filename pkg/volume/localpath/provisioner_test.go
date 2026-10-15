package localpath

import (
	"context"
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
	"mini-k8s/pkg/util/ptr"
	"mini-k8s/test/integration/framework"
)

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The provisioner with the PV controller: a claim scheduled to a node gets
// a hostPath volume there; deleting the claim runs a helper Pod on the node
// and then deletes the volume.
func TestProvisionAndDelete(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rec := record.New(c, Name, "")
	go rec.Run(ctx)
	claims := informers.New(client.PersistentVolumeClaims(c, ""), client.ListOptions{}, 0)
	volumes := informers.New(client.PersistentVolumes(c), client.ListOptions{}, 0)
	classes := informers.New(client.StorageClasses(c), client.ListOptions{}, 0)
	podInformer := informers.New(client.Pods(c, ""), client.ListOptions{}, 0)
	for _, run := range []func(context.Context){claims.Run, volumes.Run, classes.Run, podInformer.Run} {
		go run(ctx)
	}
	informers.WaitForSync(ctx, claims.HasSynced, volumes.HasSynced, classes.HasSynced, podInformer.HasSynced)
	binder := persistentvolume.NewController(c, volumes, claims, classes, rec)
	binder.Resync = 500 * time.Millisecond
	go binder.Run(ctx, 1)
	go pvprotection.NewController(c, volumes).Run(ctx, 1)
	go pvcprotection.NewController(c, claims, podInformer).Run(ctx, 1)
	go New(c, claims, volumes, classes, rec, "default").Run(ctx, 1)

	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "standard"}, Provisioner: Name,
		VolumeBindingMode: ptr.To(storagev1.VolumeBindingWaitForFirstConsumer)}
	if _, err := client.StorageClasses(c).Create(ctx, sc); err != nil {
		t.Fatal(err)
	}
	pvcs := client.PersistentVolumeClaims(c, "default")
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data",
		Annotations: map[string]string{corev1.AnnSelectedNode: "worker-2"}}, Spec: corev1.PersistentVolumeClaimSpec{
		AccessModes: []string{corev1.ReadWriteOnce}, StorageClassName: ptr.To("standard"),
		Resources: corev1.VolumeResources{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}
	pvc, err := pvcs.Create(ctx, pvc)
	if err != nil {
		t.Fatal(err)
	}
	var bound *corev1.PersistentVolumeClaim
	eventually(t, "the claim to bind a provisioned volume", func() bool {
		bound, err = pvcs.Get(ctx, "data")
		return err == nil && bound.Status.Phase == corev1.ClaimBound
	})
	pv, err := client.PersistentVolumes(c).Get(ctx, bound.Spec.VolumeName)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Name != "pvc-"+pvc.UID || pv.Spec.HostPath == nil || pv.Spec.HostPath.Path != "/var/local-path-provisioner/pvc-"+pvc.UID+"_default_data" ||
		*pv.Spec.HostPath.Type != corev1.HostPathDirectoryOrCreate || volumeNode(pv) != "worker-2" ||
		pv.Spec.PersistentVolumeReclaimPolicy != "Delete" || pv.Annotations[corev1.AnnDynamicallyProvisioned] != Name {
		t.Fatalf("provisioned volume: %+v", pv)
	}

	if err := pvcs.Delete(ctx, "data", nil); err != nil {
		t.Fatal(err)
	}
	pods := client.Pods(c, "default")
	var helper *corev1.Pod
	eventually(t, "the helper Pod", func() bool {
		helper, err = pods.Get(ctx, HelperPodName(pv.Name))
		return err == nil
	})
	if helper.Spec.NodeName != "worker-2" || helper.Spec.Containers[0].Command[2] != "/data/pvc-"+pvc.UID+"_default_data" ||
		helper.Spec.Volumes[0].HostPath.Path != "/var/local-path-provisioner" {
		t.Fatalf("helper Pod: %+v", helper.Spec)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := client.PersistentVolumes(c).Get(ctx, pv.Name); err != nil {
		t.Fatal("the volume stays until the helper Pod succeeded")
	}
	helper.Status.Phase = corev1.PodSucceeded
	if _, err := pods.UpdateStatus(ctx, helper); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the volume and the helper Pod to go", func() bool {
		_, pvErr := client.PersistentVolumes(c).Get(ctx, pv.Name)
		_, podErr := pods.Get(ctx, helper.Name)
		return client.IsNotFound(pvErr) && client.IsNotFound(podErr)
	})

	// A volume that merely claims to be ours, with a path of its own, is
	// never deleted.
	foreign := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "foreign",
		Annotations: map[string]string{corev1.AnnDynamicallyProvisioned: Name}}, Spec: corev1.PersistentVolumeSpec{
		Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}, AccessModes: []string{corev1.ReadWriteOnce},
		PersistentVolumeReclaimPolicy: "Delete", ClaimRef: &corev1.ObjectReference{Namespace: "default", Name: "gone", UID: "gone-uid"},
		PersistentVolumeSource: corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}
	if _, err := client.PersistentVolumes(c).Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the foreign volume to be Released", func() bool {
		pv, err := client.PersistentVolumes(c).Get(ctx, "foreign")
		return err == nil && pv.Status.Phase == corev1.VolumeReleased
	})
	time.Sleep(time.Second)
	if _, err := pods.Get(ctx, HelperPodName("foreign")); !client.IsNotFound(err) {
		t.Fatalf("no helper Pod may delete a path the provisioner did not create: %v", err)
	}
	if _, err := client.PersistentVolumes(c).Get(ctx, "foreign"); err != nil {
		t.Fatalf("the foreign volume stays: %v", err)
	}
}
