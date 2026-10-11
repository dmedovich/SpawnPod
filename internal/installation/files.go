package installation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

func readRegular(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing non-regular bundle file %s", path)
	}
	if info.Size() > 8<<20 {
		return nil, fmt.Errorf("bundle file too large: %s", path)
	}
	return os.ReadFile(path)
}
func safeDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("output must be a real directory: %s", dir)
	}
	return nil
}

// VerifyFiles requires exactly the expected deterministic bundle. A local edit
// or mismatched config/image set must be resolved before checking the cluster.
func (b *Bundle) VerifyFiles(dir string) error {
	if err := safeDirectory(dir); err != nil {
		return err
	}
	for _, name := range b.names() {
		actual, err := readRegular(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if !bytes.Equal(actual, b.Files[name]) {
			return fmt.Errorf("bundle drift: %s differs from target config/images; render into a new directory and review", name)
		}
	}
	return nil
}
func (b *Bundle) names() []string {
	names := make([]string, 0, len(b.Files))
	for n := range b.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// RenderFiles reports changes before writing any file. It overwrites only
// unchanged files from a previous inventory or files already equal to target.
// The inventory is committed last; retry after interruption is safe.
func (b *Bundle) RenderFiles(dir string, checkOnly bool) ([]string, error) {
	if err := safeDirectory(dir); err != nil {
		return nil, err
	}
	previous := map[string]string{}
	indexBytes, err := readRegular(filepath.Join(dir, IndexName))
	if err == nil {
		var idx Index
		decoder := json.NewDecoder(bytes.NewReader(indexBytes))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&idx); err != nil || idx.Schema != 1 {
			return nil, errors.New("invalid previous bundle inventory; render into a new directory")
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return nil, errors.New("previous inventory contains trailing data; render into a new directory")
		}
		revision := idx.Revision
		idx.Revision = ""
		canonical, _ := json.Marshal(idx)
		if checksum(canonical) != revision {
			return nil, errors.New("previous bundle inventory checksum mismatch; render into a new directory")
		}
		for _, f := range idx.Files {
			if filepath.Base(f.Name) != f.Name {
				return nil, errors.New("unsafe previous inventory filename")
			}
			previous[f.Name] = f.SHA256
		}
		previous[IndexName] = checksum(indexBytes)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var changes []string
	for _, name := range b.names() {
		actual, err := readRegular(filepath.Join(dir, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err == nil && bytes.Equal(actual, b.Files[name]) {
			continue
		}
		if err == nil && previous[name] != checksum(actual) {
			return nil, fmt.Errorf("conflict: %s is not an unchanged managed file; render into a new directory", name)
		}
		changes = append(changes, name)
	}
	if checkOnly || len(changes) == 0 {
		return changes, nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".spawnpod-render.lock"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("lock bundle output (remove stale .spawnpod-render.lock only after confirming no renderer is running): %w", err)
	}
	_ = lock.Close()
	defer func() { _ = os.Remove(filepath.Join(dir, ".spawnpod-render.lock")) }()
	// Recheck after acquiring the lock, before the first write.
	changes, err = b.RenderFiles(dir, true)
	if err != nil {
		return nil, err
	}
	for _, name := range changes {
		tmp, err := os.CreateTemp(dir, ".spawnpod-stage-")
		if err != nil {
			return nil, err
		}
		tempName := tmp.Name()
		_, writeErr := tmp.Write(b.Files[name])
		syncErr := tmp.Sync()
		closeErr := tmp.Close()
		if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
			_ = os.Remove(tempName)
			return nil, err
		}
		if err := os.Rename(tempName, filepath.Join(dir, name)); err != nil {
			_ = os.Remove(tempName)
			return nil, err
		}
	}
	return changes, nil
}
