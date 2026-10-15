// Package volume builds the files of configMap, secret, downwardAPI and
// projected volumes and writes them atomically, like Kubernetes'
// pkg/volume/util.AtomicWriter and the volume plugins of those types.
package volume

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// FileProjection is the content and mode of one file of a volume.
type FileProjection struct {
	Data []byte
	Mode int32
}

const (
	dataDirName    = "..data"
	newDataDirName = "..data_tmp"
)

// Write makes dir hold exactly payload (relative paths to files) so that
// readers always see a complete version: the files live in a timestamped
// directory "..2006_01_02_15_04_05.<random>", "..data" links to it, and
// each top-level entry is a link through "..data" (key -> ..data/key). A new
// version is a new directory to which "..data" is switched by a rename.
// Unchanged content is not rewritten; changed reports whether it was.
func Write(dir string, payload map[string]FileProjection) (changed bool, err error) {
	payload, err = cleanPayload(payload)
	if err != nil {
		return false, err
	}
	current, err := os.Readlink(filepath.Join(dir, dataDirName))
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if err := removeStaleVersions(dir, current); err != nil {
		return false, err
	}
	if current != "" {
		same, err := sameContent(filepath.Join(dir, current), payload)
		if err != nil {
			return false, err
		}
		if same {
			return false, linkTopLevel(dir, payload)
		}
	}
	tsDir, err := os.MkdirTemp(dir, time.Now().UTC().Format("..2006_01_02_15_04_05."))
	if err != nil {
		return false, err
	}
	if err := writeVersion(tsDir, payload); err != nil {
		// Leave no partial version behind (it would pile up on every retry).
		return false, errors.Join(err, os.RemoveAll(tsDir))
	}
	tmpLink := filepath.Join(dir, newDataDirName)
	if err := os.Remove(tmpLink); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if err := os.Symlink(filepath.Base(tsDir), tmpLink); err != nil {
		return false, err
	}
	if err := os.Rename(tmpLink, filepath.Join(dir, dataDirName)); err != nil {
		return false, err
	}
	if err := linkTopLevel(dir, payload); err != nil {
		return true, err
	}
	if current != "" && current != filepath.Base(tsDir) {
		if err := os.RemoveAll(filepath.Join(dir, current)); err != nil {
			return true, err
		}
	}
	return true, nil
}

// writeVersion writes the payload's files into a new version directory.
// Errors name the payload path, not the random directory, so a repeated
// failure reads the same.
func writeVersion(tsDir string, payload map[string]FileProjection) error {
	if err := os.Chmod(tsDir, 0o755); err != nil {
		return err
	}
	for path, f := range payload {
		full := filepath.Join(tsDir, path)
		err := os.MkdirAll(filepath.Dir(full), 0o755)
		if err == nil {
			err = os.WriteFile(full, f.Data, fs.FileMode(f.Mode))
		}
		if err == nil {
			err = os.Chmod(full, fs.FileMode(f.Mode))
		}
		if err != nil {
			var pathErr *fs.PathError
			if errors.As(err, &pathErr) {
				err = pathErr.Err
			}
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

// cleanPayload normalises the paths ("./a" and "a/" are "a") and refuses
// paths that leave the volume, name its root, or are both a file and the
// directory of another file.
func cleanPayload(payload map[string]FileProjection) (map[string]FileProjection, error) {
	out := make(map[string]FileProjection, len(payload))
	for path, f := range payload {
		clean := filepath.Clean(filepath.ToSlash(path))
		if path == "" || clean == "." || filepath.IsAbs(path) || strings.HasPrefix(clean, "..") ||
			slices.Contains(strings.Split(filepath.ToSlash(path), "/"), "..") {
			return nil, fmt.Errorf("invalid path %q in volume payload", path)
		}
		if _, dup := out[clean]; dup {
			return nil, fmt.Errorf("duplicate path %q in volume payload", clean)
		}
		out[clean] = f
	}
	for path := range out {
		for dir := filepath.Dir(path); dir != "."; dir = filepath.Dir(dir) {
			if _, ok := out[dir]; ok {
				return nil, fmt.Errorf("path %q in volume payload is a file and the directory of %q", dir, path)
			}
		}
	}
	return out, nil
}

// removeStaleVersions deletes version directories other than the current
// one, left by an interrupted write.
func removeStaleVersions(dir, current string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "..") && e.Name() != current {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// topLevel returns the first element of every path.
func topLevel(payload map[string]FileProjection) map[string]bool {
	names := map[string]bool{}
	for path := range payload {
		names[strings.SplitN(filepath.ToSlash(path), "/", 2)[0]] = true
	}
	return names
}

// linkTopLevel creates the user-visible links of the payload and removes
// those of paths no longer in it.
func linkTopLevel(dir string, payload map[string]FileProjection) error {
	names := topLevel(payload)
	for name := range names {
		link := filepath.Join(dir, name)
		if _, err := os.Lstat(link); err == nil {
			continue
		}
		if err := os.Symlink(filepath.Join(dataDirName, name), link); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "..") || names[e.Name()] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// sameContent reports whether dir holds exactly the payload's files with
// the same content and modes.
func sameContent(dir string, payload map[string]FileProjection) (bool, error) {
	seen := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		want, ok := payload[filepath.ToSlash(rel)]
		if !ok {
			return errDiffers
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(data, want.Data) || info.Mode().Perm() != fs.FileMode(want.Mode).Perm() {
			return errDiffers
		}
		seen++
		return nil
	})
	if err == errDiffers || os.IsNotExist(err) {
		return false, nil
	}
	return err == nil && seen == len(payload), err
}

var errDiffers = errors.New("content differs")
