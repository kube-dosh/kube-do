package imagebuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildIncludesBinaryAndLibraries(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "keepalived")
	if err := os.WriteFile(binaryPath, []byte("fake-keepalived"), 0o755); err != nil {
		t.Fatal(err)
	}
	libraryPath := filepath.Join(dir, "libssl.so.3")
	if err := os.WriteFile(libraryPath, []byte("fake-libssl"), 0o644); err != nil {
		t.Fatal(err)
	}

	image, err := Build(Options{
		BinaryPath: binaryPath,
		BinaryName: "keepalived",
		Version:    "2.4.3",
		OS:         "linux",
		Arch:       "amd64",
		Libraries:  []string{libraryPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if config.OS != "linux" || config.Architecture != "amd64" {
		t.Fatalf("platform = %s/%s", config.OS, config.Architecture)
	}
	if len(config.Config.Entrypoint) != 1 || config.Config.Entrypoint[0] != "/usr/local/sbin/keepalived" {
		t.Fatalf("entrypoint = %#v", config.Config.Entrypoint)
	}
	for _, tc := range []struct {
		name string
		want string
	}{
		{"usr/local/sbin/keepalived", "fake-keepalived"},
		{strings.TrimPrefix(filepath.ToSlash(libraryPath), "/"), "fake-libssl"},
		{"etc/passwd", "root:x:0:0"},
		{"etc/group", "root:x:0:"},
		{"etc/nsswitch.conf", "passwd: files"},
	} {
		got, err := ExtractFile(image, tc.name)
		if err != nil {
			t.Fatalf("ExtractFile(%q): %v", tc.name, err)
		}
		if !strings.Contains(string(got), tc.want) {
			t.Fatalf("%s = %q, want substring %q", tc.name, got, tc.want)
		}
	}
}

func TestBuildRejectsMissingLibrary(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(binaryPath, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Build(Options{
		BinaryPath: binaryPath,
		BinaryName: "binary",
		Version:    "1.0.0",
		OS:         "linux",
		Arch:       "amd64",
		Libraries:  []string{"/does/not/exist.so"},
	})
	if err == nil || !strings.Contains(err.Error(), "read library") {
		t.Fatalf("expected missing library error, got %v", err)
	}
}

func TestParseLdd(t *testing.T) {
	output := `linux-vdso.so.1 (0x00007ffd)
	libssl.so.3 => /lib64/libssl.so.3 (0x00007f)
	/lib64/ld-linux-x86-64.so.2 (0x00007f)
	libc.so.6 => /lib64/libc.so.6 (0x00007f)`
	got, err := parseLdd(output)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/lib64/ld-linux-x86-64.so.2", "/lib64/libc.so.6", "/lib64/libssl.so.3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("libraries = %#v, want %#v", got, want)
	}
}

func TestParseLddRejectsNotFound(t *testing.T) {
	_, err := parseLdd("libmissing.so => not found")
	if err == nil || !strings.Contains(err.Error(), "missing dependency") {
		t.Fatalf("expected missing dependency error, got %v", err)
	}
}
