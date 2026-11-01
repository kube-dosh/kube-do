package archive

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type entry struct {
	name     string
	body     string
	linkname string
	typeflag byte
	modTime  time.Time
}

func writeTarGz(t *testing.T, path string, entries []entry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	for _, e := range entries {
		typeflag := e.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		size := int64(len(e.body))
		if typeflag != tar.TypeReg {
			size = 0
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o755, Size: size, Typeflag: typeflag, Linkname: e.linkname, ModTime: e.modTime}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestExtractTarGz(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "a.tar.gz")
	writeTarGz(t, archivePath, []entry{{name: "bin/registry", body: "binary"}})

	out := filepath.Join(dir, "out")
	written, err := ExtractTarGz(archivePath, out)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 1 {
		t.Fatalf("written = %v", written)
	}
	data, err := os.ReadFile(filepath.Join(out, "bin", "registry"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "binary" {
		t.Fatalf("content = %q", data)
	}
}

func TestExtractTarGzRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "a.tar.gz")
	writeTarGz(t, archivePath, []entry{{name: "../evil", body: "x"}})
	if _, err := ExtractTarGz(archivePath, filepath.Join(dir, "out")); err == nil {
		t.Fatal("expected traversal error")
	}
}

func TestExtractTarGzAllowsSafeSymlink(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "a.tar.gz")
	writeTarGz(t, archivePath, []entry{
		{name: "data/common.pem", body: "certificate"},
		{name: "links/common.pem", typeflag: tar.TypeSymlink, linkname: "../data/common.pem"},
	})

	out := filepath.Join(dir, "out")
	if _, err := ExtractTarGz(archivePath, out); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(out, "links", "common.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("expected a symlink")
	}
	data, err := os.ReadFile(filepath.Join(out, "links", "common.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "certificate" {
		t.Fatalf("content = %q", data)
	}
}

func TestExtractTarGzAllowsSafeHardlink(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "a.tar.gz")
	writeTarGz(t, archivePath, []entry{
		{name: "data/file", body: "payload"},
		{name: "copy", typeflag: tar.TypeLink, linkname: "data/file"},
	})

	out := filepath.Join(dir, "out")
	if _, err := ExtractTarGz(archivePath, out); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(filepath.Join(out, "data", "file"))
	if err != nil {
		t.Fatal(err)
	}
	copyInfo, err := os.Stat(filepath.Join(out, "copy"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(original, copyInfo) {
		t.Fatal("expected a hardlink")
	}
}

func TestExtractTarGzRejectsEscapingLinks(t *testing.T) {
	tests := []struct {
		name  string
		entry entry
	}{
		{
			name:  "relative symlink",
			entry: entry{name: "link", typeflag: tar.TypeSymlink, linkname: "../outside"},
		},
		{
			name:  "absolute symlink",
			entry: entry{name: "link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
		},
		{
			name:  "hardlink",
			entry: entry{name: "link", typeflag: tar.TypeLink, linkname: "../outside"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			archivePath := filepath.Join(dir, "a.tar.gz")
			writeTarGz(t, archivePath, []entry{tt.entry})
			if _, err := ExtractTarGz(archivePath, filepath.Join(dir, "out")); err == nil {
				t.Fatal("expected unsafe link error")
			}
		})
	}
}

func TestExtractTarGzPreservesModTime(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "a.tar.gz")
	want := time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC)
	writeTarGz(t, archivePath, []entry{{name: "configure", body: "#!/bin/sh\n", modTime: want}})

	out := filepath.Join(dir, "out")
	if _, err := ExtractTarGz(archivePath, out); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(out, "configure"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(want) {
		t.Fatalf("modtime = %s, want %s", info.ModTime(), want)
	}
}
