package workspace

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

const (
	Mount                 = "/workspace"
	MaxArchiveBytes int64 = 512 << 20
	MaxFileBytes    int64 = 256 << 20
	MaxEntries            = 10000
)

// Relative validates a protocol path and returns a path within Mount.
func Relative(value string) (string, error) {
	if value == Mount {
		return ".", nil
	}
	// Runner supplies directory destinations such as /workspace/.act/.
	// Accept one trailing slash while retaining the canonical path checks.
	canonical := strings.TrimSuffix(value, "/")
	if len(value) > 4096 || canonical == Mount || !strings.HasPrefix(canonical, Mount+"/") || path.Clean(canonical) != canonical || strings.ContainsRune(value, 0) {
		return "", errors.New("path must be canonical and inside /workspace")
	}
	return strings.TrimPrefix(canonical, Mount+"/"), nil
}

func entryName(value string) (string, error) {
	value = strings.TrimSuffix(value, "/")
	if value == "." {
		return value, nil
	}
	if value == "" || len(value) > 4096 || path.IsAbs(value) || path.Clean(value) != value || value == ".." || strings.HasPrefix(value, "../") || strings.ContainsRune(value, 0) {
		return "", errors.New("invalid archive entry path")
	}
	return value, nil
}

// EnsureLayout prepares runner staging directories before the Pod is ready.
func EnsureLayout(mount string) error {
	root, err := os.OpenRoot(mount)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, name := range []string{".act", ".toolcache", ".tmp"} {
		if err := ensureDirs(root, name); err != nil {
			return err
		}
	}
	return nil
}

// EnsureDirectory creates a canonical directory below the workspace mount.
// The runner may execute a shell action before checkout creates its default
// repository working directory.
func EnsureDirectory(mount, target string) error {
	rel, err := Relative(target)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(mount)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return ensureDirs(root, rel)
}

func ensureDirs(root *os.Root, relative string) error {
	if relative == "." {
		return nil
	}
	parts := strings.Split(relative, "/")
	current := ""
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return errors.New("invalid directory component")
		}
		current = path.Join(current, part)
		info, err := root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err = root.Mkdir(current, 0755); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("non-directory in workspace path: %s", current)
		}
	}
	return nil
}

type cappedReader struct {
	source    io.Reader
	remaining int64
}

type zeroWriter struct{}

func (zeroWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		if b != 0 {
			return 0, errors.New("nonzero data after tar trailer")
		}
	}
	return len(p), nil
}

func (r *cappedReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		var one [1]byte
		n, err := r.source.Read(one[:])
		if n > 0 {
			return 0, errors.New("archive exceeds byte limit")
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.source.Read(p)
	r.remaining -= int64(n)
	return n, err
}

// Extract writes regular files and directories from a tar stream into dest.
// Each file is staged and renamed only after its complete payload arrives.
// A failed archive can leave previously completed entries; callers must
// discard the whole environment after a failed transfer.
func Extract(ctx context.Context, mount, dest string, input io.Reader) error {
	rel, err := Relative(dest)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(mount)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := ensureDirs(root, rel); err != nil {
		return err
	}
	capped := &cappedReader{source: input, remaining: MaxArchiveBytes}
	reader := tar.NewReader(capped)
	var total int64
	seen := make(map[string]struct{})
	for count := 0; ; count++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}
		if count >= MaxEntries {
			return errors.New("archive exceeds entry limit")
		}
		name, err := entryName(hdr.Name)
		if err != nil {
			return err
		}
		if name == "." {
			if hdr.Typeflag == tar.TypeDir {
				continue
			}
			return errors.New("root archive entry is not a directory")
		}
		target := path.Join(rel, name)
		if _, exists := seen[target]; exists {
			return errors.New("duplicate archive entry")
		}
		seen[target] = struct{}{}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			return errors.New("unsupported archive entry type")
		}
		if hdr.Size < 0 || hdr.Size > MaxFileBytes || total > MaxArchiveBytes-hdr.Size {
			return errors.New("archive exceeds file or total size limit")
		}
		total += hdr.Size
		if err := ensureDirs(root, path.Dir(target)); err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeDir {
			if err := ensureDirs(root, target); err != nil {
				return err
			}
			continue
		}
		if info, err := root.Lstat(target); err == nil {
			if !info.Mode().IsRegular() {
				return errors.New("target is not a regular file")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		tmp := path.Join(path.Dir(target), ".spawnpod-"+hex.EncodeToString(random[:]))
		file, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(file, reader, hdr.Size)
		if copyErr == nil {
			mode := os.FileMode(hdr.Mode) & 0755
			copyErr = file.Chmod(mode)
		}
		closeErr := file.Close()
		if copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			_ = root.Remove(tmp)
			return fmt.Errorf("stage archive file: %w", copyErr)
		}
		if err := root.Rename(tmp, target); err != nil {
			_ = root.Remove(tmp)
			return err
		}
	}
	_, err = io.Copy(zeroWriter{}, capped)
	return err
}

// Archive streams one file or directory as tar. Symlinks and special files
// are rejected; no absolute host path enters the archive.
func Archive(ctx context.Context, mount, src string, output io.Writer) error {
	rel, err := Relative(src)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(mount)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := checkComponents(root, rel); err != nil {
		return err
	}
	writer := tar.NewWriter(output)
	count := 0
	var total int64
	var walk func(string, string) error
	walk = func(source, name string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if count >= MaxEntries {
			return errors.New("archive exceeds entry limit")
		}
		info, err := root.Lstat(source)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink in archived path")
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return errors.New("unsupported workspace file type")
		}
		count++
		if info.IsDir() {
			if name != "" {
				if err := writer.WriteHeader(&tar.Header{Name: name + "/", Mode: 0755, Typeflag: tar.TypeDir}); err != nil {
					return err
				}
			}
			f, err := root.Open(source)
			if err != nil {
				return err
			}
			for {
				entries, readErr := f.ReadDir(128)
				for _, entry := range entries {
					child := path.Join(source, entry.Name())
					childName := path.Join(name, entry.Name())
					if err := walk(child, childName); err != nil {
						_ = f.Close()
						return err
					}
				}
				if errors.Is(readErr, io.EOF) {
					break
				}
				if readErr != nil {
					_ = f.Close()
					return readErr
				}
			}
			_ = f.Close()
			return nil
		}
		if info.Size() < 0 || info.Size() > MaxFileBytes || total > MaxArchiveBytes-info.Size() {
			return errors.New("archive exceeds file or total size limit")
		}
		file, err := root.Open(source)
		if err != nil {
			return err
		}
		opened, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return err
		}
		if !opened.Mode().IsRegular() {
			_ = file.Close()
			return errors.New("workspace file changed type")
		}
		hdr := &tar.Header{Name: name, Mode: int64(info.Mode().Perm() & 0755), Size: opened.Size(), Typeflag: tar.TypeReg}
		if hdr.Size < 0 || hdr.Size > MaxFileBytes || total > MaxArchiveBytes-hdr.Size {
			_ = file.Close()
			return errors.New("archive exceeds file or total size limit")
		}
		total += hdr.Size
		if err := writer.WriteHeader(hdr); err != nil {
			_ = file.Close()
			return err
		}
		_, err = io.CopyN(writer, file, hdr.Size)
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		return err
	}
	info, err := root.Lstat(rel)
	if err != nil {
		return err
	}
	name := path.Base(rel)
	if info.IsDir() {
		name = ""
	}
	if err := walk(rel, name); err != nil {
		return err
	}
	return writer.Close()
}

func checkComponents(root *os.Root, rel string) error {
	if rel == "." {
		return nil
	}
	current := ""
	for _, part := range strings.Split(rel, "/") {
		current = path.Join(current, part)
		info, err := root.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink in workspace path")
		}
	}
	return nil
}
