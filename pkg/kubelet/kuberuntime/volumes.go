package kuberuntime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/volume"
)

// DefaultPodsDir holds per-Pod kubelet data such as emptyDir volumes (on the
// node container's persistent /var).
const DefaultPodsDir = "/var/lib/kubelet/pods"

// Mounter mounts tmpfs for memory-backed emptyDir volumes and the volumes
// that live in memory (secrets, the downward API, projected volumes).
type Mounter interface {
	MountTmpfs(dir string, sizeBytes int64) error
	Unmount(dir string) error
}

// podDir is the Pod's kubelet directory.
func (m *Manager) podDir(uid string) string { return filepath.Join(m.PodsDir, uid) }

// emptyDirPath is where an emptyDir volume lives, as in the kubelet.
func (m *Manager) emptyDirPath(uid, name string) string {
	return filepath.Join(m.podDir(uid), "volumes", "kubernetes.io~empty-dir", name)
}

// mountedVolumes are the node paths of a Pod's volumes by name, and which
// of them are always mounted read-only.
type mountedVolumes struct {
	paths    map[string]string
	readOnly map[string]bool
}

// setupVolumes prepares the Pod's volumes. emptyDir directories are created
// once and survive container and node restarts; memory-backed ones are
// tmpfs mounts. hostPath volumes are checked (or created) according to
// their type. configMap, secret, downwardAPI and projected volumes get
// their files written atomically on every call, so changes reach running
// containers; they are mounted read-only, and all but configMap volumes
// live on a tmpfs. Errors are worded like the kubelet's FailedMount events.
func (m *Manager) setupVolumes(pod *corev1.Pod) (mountedVolumes, error) {
	vols := mountedVolumes{paths: map[string]string{}, readOnly: map[string]bool{}}
	failed := func(name string, err error) (mountedVolumes, error) {
		return vols, fmt.Errorf("MountVolume.SetUp failed for volume %q : %w", name, err)
	}
	for _, v := range pod.Spec.Volumes {
		switch {
		case v.EmptyDir != nil:
			dir := m.emptyDirPath(pod.UID, v.Name)
			if err := os.MkdirAll(dir, 0o777); err != nil {
				return failed(v.Name, err)
			}
			if err := os.Chmod(dir, 0o777); err != nil { // any container user may write, as in Kubernetes
				return failed(v.Name, err)
			}
			if v.EmptyDir.Medium == corev1.StorageMediumMemory {
				size := int64(0)
				if v.EmptyDir.SizeLimit != nil {
					size = v.EmptyDir.SizeLimit.Value()
				}
				if err := m.mountTmpfs(dir, size); err != nil {
					return failed(v.Name, err)
				}
			}
			vols.paths[v.Name] = dir
		case v.HostPath != nil:
			if err := checkHostPath(v.HostPath); err != nil {
				return failed(v.Name, err)
			}
			vols.paths[v.Name] = v.HostPath.Path
		case v.PersistentVolumeClaim != nil:
			path, err := m.claimPath(pod.Namespace, v.PersistentVolumeClaim.ClaimName)
			if err != nil {
				return failed(v.Name, err)
			}
			vols.paths[v.Name] = path
			vols.readOnly[v.Name] = v.PersistentVolumeClaim.ReadOnly
		default:
			src := volume.Source{Pod: pod, Objects: m.Objects, Allocatable: m.Allocatable}
			if m.Objects == nil {
				src.Objects = noObjects{}
			}
			payload, ok, err := src.Payload(v.VolumeSource)
			if !ok {
				continue
			}
			if err != nil {
				return failed(v.Name, err)
			}
			dir := filepath.Join(m.podDir(pod.UID), "volumes", volume.PluginDir(v.VolumeSource), v.Name)
			if err := os.MkdirAll(dir, 0o777); err != nil {
				return failed(v.Name, err)
			}
			if volume.OnTmpfs(v.VolumeSource) {
				if err := m.mountTmpfs(dir, 0); err != nil {
					return failed(v.Name, err)
				}
			} else if err := os.Chmod(dir, 0o777); err != nil {
				return failed(v.Name, err)
			}
			if _, err := volume.Write(dir, payload); err != nil {
				return failed(v.Name, err)
			}
			vols.paths[v.Name], vols.readOnly[v.Name] = dir, true
		}
	}
	return vols, nil
}

// ClaimSource looks up claims and the volumes bound to them (the kubelet's
// informer caches).
type ClaimSource interface {
	Claim(namespace, name string) (*corev1.PersistentVolumeClaim, bool)
	Volume(name string) (*corev1.PersistentVolume, bool)
}

// claimPath resolves a claim to the node directory of its bound volume (a
// hostPath, created if its type says so, or a local volume).
func (m *Manager) claimPath(namespace, name string) (string, error) {
	if m.Claims == nil {
		return "", fmt.Errorf("persistentvolumeclaim %q not found", name)
	}
	pvc, ok := m.Claims.Claim(namespace, name)
	if !ok {
		return "", fmt.Errorf("persistentvolumeclaim %q not found", name)
	}
	if pvc.Spec.VolumeName == "" || pvc.Status.Phase != corev1.ClaimBound {
		return "", fmt.Errorf("error processing PVC %s/%s: PVC is not bound", namespace, name)
	}
	pv, ok := m.Claims.Volume(pvc.Spec.VolumeName)
	if !ok {
		return "", fmt.Errorf("persistentvolume %q not found", pvc.Spec.VolumeName)
	}
	switch {
	case pv.Spec.HostPath != nil:
		if err := checkHostPath(pv.Spec.HostPath); err != nil {
			return "", err
		}
		return pv.Spec.HostPath.Path, nil
	case pv.Spec.Local != nil:
		if info, err := os.Stat(pv.Spec.Local.Path); err != nil || !info.IsDir() {
			return "", fmt.Errorf("path %q of local volume %q is not a directory", pv.Spec.Local.Path, pv.Name)
		}
		return pv.Spec.Local.Path, nil
	}
	return "", fmt.Errorf("persistentvolume %q has no supported source", pv.Name)
}

// noObjects finds nothing (a runtime without an API, as in tests).
type noObjects struct{}

func (noObjects) ConfigMap(string, string) (*corev1.ConfigMap, bool) { return nil, false }
func (noObjects) Secret(string, string) (*corev1.Secret, bool)       { return nil, false }

// mountTmpfs mounts a tmpfs of size bytes (0: the kernel default) on dir
// once.
func (m *Manager) mountTmpfs(dir string, size int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tmpfs[dir] {
		return nil
	}
	if err := m.Mounter.MountTmpfs(dir, size); err != nil {
		return err
	}
	m.tmpfs[dir] = true
	return nil
}

// checkHostPath applies the hostPath type check, creating the path for the
// *OrCreate types.
func checkHostPath(h *corev1.HostPathVolumeSource) error {
	t := ""
	if h.Type != nil {
		t = *h.Type
	}
	info, err := os.Stat(h.Path)
	missing := errors.Is(err, os.ErrNotExist)
	switch t {
	case corev1.HostPathDirectoryOrCreate:
		if missing {
			return os.MkdirAll(h.Path, 0o755)
		}
		if err != nil || !info.IsDir() {
			return fmt.Errorf("hostPath type check failed: %s is not a directory", h.Path)
		}
	case corev1.HostPathDirectory:
		if err != nil || !info.IsDir() {
			return fmt.Errorf("hostPath type check failed: %s is not a directory", h.Path)
		}
	case corev1.HostPathFileOrCreate:
		if missing {
			if err := os.MkdirAll(filepath.Dir(h.Path), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(h.Path, os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			return f.Close()
		}
		if err != nil || info.IsDir() {
			return fmt.Errorf("hostPath type check failed: %s is not a file", h.Path)
		}
	case corev1.HostPathFile:
		if err != nil || info.IsDir() {
			return fmt.Errorf("hostPath type check failed: %s is not a file", h.Path)
		}
	}
	return nil
}

// removePodDir deletes the Pod's kubelet directory, unmounting its tmpfs
// volumes first.
func (m *Manager) removePodDir(uid string) error {
	dir := m.podDir(uid)
	m.mu.Lock()
	var errs []error
	for mounted := range m.tmpfs {
		if filepath.Dir(filepath.Dir(filepath.Dir(mounted))) == dir {
			if err := m.Mounter.Unmount(mounted); err != nil {
				errs = append(errs, err)
				continue
			}
			delete(m.tmpfs, mounted)
		}
	}
	m.mu.Unlock()
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return os.RemoveAll(dir)
}
