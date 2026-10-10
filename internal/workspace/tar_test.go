package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func archive(entries ...struct {
	name string
	kind byte
	body string
}) []byte {
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, entry := range entries {
		h := &tar.Header{Name: entry.name, Typeflag: entry.kind, Size: int64(len(entry.body)), Mode: 0644}
		if entry.kind == tar.TypeDir {
			h.Size = 0
		}
		if err := w.WriteHeader(h); err != nil {
			panic(err)
		}
		if entry.body != "" {
			if _, err := w.Write([]byte(entry.body)); err != nil {
				panic(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func TestExtractAndArchive(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureLayout(dir); err != nil {
		t.Fatal(err)
	}
	if err := Extract(context.Background(), dir, Mount+"/.act", bytes.NewReader(archive(struct {
		name string
		kind byte
		body string
	}{"a.txt", tar.TypeReg, "hello"}))); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, ".act", "a.txt"))
	if err != nil || string(content) != "hello" {
		t.Fatalf("file=%q error=%v", content, err)
	}
	var result bytes.Buffer
	if err := Archive(context.Background(), dir, Mount+"/.act/a.txt", &result); err != nil {
		t.Fatal(err)
	}
	r := tar.NewReader(&result)
	h, err := r.Next()
	if err != nil || h.Name != "a.txt" {
		t.Fatalf("header=%+v error=%v", h, err)
	}
}

func TestRejectsTraversalAndSymlink(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"../escape", "/absolute", "a/../b"} {
		if err := Extract(context.Background(), dir, Mount, bytes.NewReader(archive(struct {
			name string
			kind byte
			body string
		}{name, tar.TypeReg, "bad"}))); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := Extract(context.Background(), dir, Mount+"/link", bytes.NewReader(archive(struct {
		name string
		kind byte
		body string
	}{"x", tar.TypeReg, "bad"}))); err == nil {
		t.Fatal("accepted symlink destination")
	}
	if err := Archive(context.Background(), dir, Mount+"/link", &bytes.Buffer{}); err == nil {
		t.Fatal("archived symlink")
	}
	if err := Extract(context.Background(), dir, Mount, bytes.NewReader(archive(struct {
		name string
		kind byte
		body string
	}{"link", tar.TypeSymlink, ""}))); err == nil {
		t.Fatal("accepted symlink entry")
	}
}

func TestRejectsOversizeBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	if err := w.WriteHeader(&tar.Header{Name: "huge", Typeflag: tar.TypeReg, Size: MaxFileBytes + 1}); err != nil {
		t.Fatal(err)
	}
	if err := Extract(context.Background(), dir, Mount, bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatal("accepted oversized entry")
	}
	if _, err := os.Stat(filepath.Join(dir, "huge")); !os.IsNotExist(err) {
		t.Fatalf("unexpected file: %v", err)
	}
}

func TestTruncatedFileDoesNotAppear(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	if err := w.WriteHeader(&tar.Header{Name: "incomplete", Typeflag: tar.TypeReg, Size: 4096, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("part")); err != nil {
		t.Fatal(err)
	}
	if err := Extract(context.Background(), dir, Mount, bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatal("accepted truncated archive")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("left staged files: %+v", entries)
	}
}

func TestRelativeRequiresCanonicalWorkspacePath(t *testing.T) {
	for _, value := range []string{"/etc/passwd", "/workspace/../etc", "/workspace//x", "/workspaces/x", "/workspace/", "/workspace/a//", "/workspace/a/../"} {
		if _, err := Relative(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	if rel, err := Relative(Mount + "/a"); err != nil || strings.TrimSpace(rel) != "a" {
		t.Fatalf("rel=%q err=%v", rel, err)
	}
	if rel, err := Relative(Mount + "/.act/"); err != nil || rel != ".act" {
		t.Fatalf("Runner directory destination rel=%q err=%v", rel, err)
	}
}

func TestEnsureDirectoryCreatesOnlyInsideWorkspace(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureDirectory(dir, Mount+"/owner/repo"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, "owner", "repo")); err != nil || !info.IsDir() {
		t.Fatalf("repository workdir missing: %v", err)
	}
	for _, target := range []string{Mount + "/../escape", "/etc", Mount + "//double"} {
		if err := EnsureDirectory(dir, target); err == nil {
			t.Fatalf("accepted workdir %q", target)
		}
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirectory(dir, Mount+"/link/child"); err == nil {
		t.Fatal("followed symlink workdir")
	}
}
