package kuberuntime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	corev1 "mini-k8s/pkg/apis/core/v1"
)

// DefaultPodsDir holds per-Pod kubelet data such as emptyDir volumes (on the
// node container's persistent /var).
const DefaultPodsDir = "/var/lib/kubelet/pods"

// Mounter mounts tmpfs for memory-backed emptyDir volumes.
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

// setupVolumes prepares the Pod's volumes and returns their node paths by
// volume name. emptyDir directories are created once and survive container
// and node restarts; memory-backed ones are tmpfs mounts. hostPath volumes
// are checked (or created) according to their type.
func (m *Manager) setupVolumes(pod *corev1.Pod) (map[string]string, error) {
	paths := map[string]string{}
	for _, v := range pod.Spec.Volumes {
		switch {
		case v.EmptyDir != nil:
			dir := m.emptyDirPath(pod.UID, v.Name)
			if err := os.MkdirAll(dir, 0o777); err != nil {
				return nil, fmt.Errorf("MountVolume.SetUp failed for volume %q: %w", v.Name, err)
			}
			if err := os.Chmod(dir, 0o777); err != nil { // any container user may write, as in Kubernetes
				return nil, fmt.Errorf("MountVolume.SetUp failed for volume %q: %w", v.Name, err)
			}
			if v.EmptyDir.Medium == corev1.StorageMediumMemory {
				if err := m.mountTmpfs(dir, v.EmptyDir); err != nil {
					return nil, fmt.Errorf("MountVolume.SetUp failed for volume %q: %w", v.Name, err)
				}
			}
			paths[v.Name] = dir
		case v.HostPath != nil:
			if err := checkHostPath(v.HostPath); err != nil {
				return nil, fmt.Errorf("MountVolume.SetUp failed for volume %q: %w", v.Name, err)
			}
			paths[v.Name] = v.HostPath.Path
		}
	}
	return paths, nil
}

func (m *Manager) mountTmpfs(dir string, e *corev1.EmptyDirVolumeSource) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tmpfs[dir] {
		return nil
	}
	size := int64(0)
	if e.SizeLimit != nil {
		size = e.SizeLimit.Value()
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
