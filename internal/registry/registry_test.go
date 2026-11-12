package registry

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGenerateSelfSigned(t *testing.T) {
	caPEM, certPEM, keyPEM, err := GenerateSelfSigned("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(caPEM)
	if block == nil {
		t.Fatal("CA is not PEM")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.IsCA {
		t.Fatal("CA certificate is not a CA")
	}

	block, _ = pem.Decode(certPEM)
	server, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ip := range server.IPAddresses {
		if ip.String() == "127.0.0.1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("server certificate lacks 127.0.0.1 SAN: %v", server.IPAddresses)
	}
	if len(keyPEM) == 0 {
		t.Fatal("empty key")
	}
}

func TestSelfSignedCertificateRegeneratesWhenHostChanges(t *testing.T) {
	out := t.TempDir()
	opts := Options{DryRun: true, OutputDir: out, TLSMode: TLSModeSelfSigned}
	if err := writeSelfSigned(opts, "127.0.0.1", &Result{}); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(out, "tls", "server.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSelfSigned(opts, "127.0.0.2", &Result{}); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(out, "tls", "server.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) == string(second) {
		t.Fatal("certificate was reused despite a different host SAN")
	}
	block, _ := pem.Decode(second)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.VerifyHostname("127.0.0.2"); err != nil {
		t.Fatalf("new certificate does not cover 127.0.0.2: %v", err)
	}
}

func TestSelfSignedCertificateRegenerationTightensKeyPermissions(t *testing.T) {
	out := t.TempDir()
	opts := Options{DryRun: true, OutputDir: out, TLSMode: TLSModeSelfSigned}
	if err := writeSelfSigned(opts, "127.0.0.1", &Result{}); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(out, "tls", "server.key")
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeSelfSigned(opts, "127.0.0.2", &Result{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("server key mode = %o", info.Mode().Perm())
	}
}

func TestRenderUnitQuotesExecutablePaths(t *testing.T) {
	unit := string(RenderUnit("/tmp/kube do/bin/registry", "/etc/kube do/config.yml"))
	if !strings.Contains(unit, `ExecStart="/tmp/kube do/bin/registry" serve "/etc/kube do/config.yml"`) {
		t.Fatalf("unit does not quote paths with spaces:\n%s", unit)
	}
}

func TestEnsureDryRun(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "registry.tar.gz")
	writeRegistryArchive(t, archivePath)
	out := t.TempDir()
	addr := freeAddr(t)

	res, err := Ensure(context.Background(), Options{
		Address:       addr,
		DataDir:       "/var/lib/kube-do/registry",
		TLSMode:       TLSModeSelfSigned,
		SourceArchive: archivePath,
		DryRun:        true,
		OutputDir:     out,
		ProbeTimeout:  200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reused {
		t.Fatal("expected deployment, got reuse")
	}
	for _, rel := range []string{"bin/registry", "config.yml", "kube-do-registry.service", "tls/ca.crt", "tls/server.crt", "tls/server.key"} {
		if _, err := os.Stat(filepath.Join(out, rel)); err != nil {
			t.Errorf("%s missing: %v", rel, err)
		}
	}
	config, err := os.ReadFile(filepath.Join(out, "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "/var/lib/kube-do/registry") {
		t.Fatalf("config missing data dir:\n%s", config)
	}
	_, port, _ := net.SplitHostPort(addr)
	if !strings.Contains(string(config), ":"+port) {
		t.Fatalf("config missing port %s:\n%s", port, config)
	}
	unit, err := os.ReadFile(filepath.Join(out, "kube-do-registry.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "serve") {
		t.Fatalf("unit missing serve command:\n%s", unit)
	}
	if !strings.Contains(string(unit), "OTEL_TRACES_EXPORTER=none") {
		t.Fatalf("unit does not disable OTLP tracing:\n%s", unit)
	}
	info, err := os.Stat(filepath.Join(out, "tls", "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("server.key mode = %o", info.Mode().Perm())
	}
}

func TestEnsureReusesExistingRegistry(t *testing.T) {
	caPEM, certPEM, keyPEM, err := GenerateSelfSigned("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	srv.StartTLS()
	defer srv.Close()

	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")
	for path, data := range map[string][]byte{caPath: caPEM, certPath: certPEM, keyPath: keyPEM} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	res, err := Ensure(context.Background(), Options{
		Address:      srv.Listener.Addr().String(),
		DataDir:      t.TempDir(),
		TLSMode:      TLSModeProvided,
		CAFile:       caPath,
		CertFile:     certPath,
		KeyFile:      keyPath,
		ProbeTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reused {
		t.Fatal("expected the existing registry to be reused")
	}
}

func TestEnsureDryRunIgnoresOccupiedPort(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "registry.tar.gz")
	writeRegistryArchive(t, archivePath)
	out := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	res, err := Ensure(context.Background(), Options{
		Address:       ln.Addr().String(),
		DataDir:       "/var/lib/kube-do/registry",
		TLSMode:       TLSModeSelfSigned,
		SourceArchive: archivePath,
		DryRun:        true,
		OutputDir:     out,
		ProbeTimeout:  200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("dry-run should not care about a live port: %v", err)
	}
	if res.Reused {
		t.Fatal("dry-run must not report reuse")
	}
	if _, err := os.Stat(filepath.Join(out, "config.yml")); err != nil {
		t.Fatalf("dry-run bundle missing: %v", err)
	}
}

func TestEnsureDryRunUsesAbsolutePaths(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "registry.tar.gz")
	writeRegistryArchive(t, archivePath)
	t.Chdir(t.TempDir())

	res, err := Ensure(context.Background(), Options{
		Address:       freeAddr(t),
		DataDir:       "/var/lib/kube-do/registry",
		TLSMode:       TLSModeSelfSigned,
		SourceArchive: archivePath,
		DryRun:        true,
		OutputDir:     "registry-bundle",
		ProbeTimeout:  200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := filepath.Abs("registry-bundle")
	if err != nil {
		t.Fatal(err)
	}
	wantExec := "ExecStart=" + systemdQuote(filepath.Join(out, "bin", "registry")) + " serve " + systemdQuote(filepath.Join(out, "config.yml"))
	unit, err := os.ReadFile(filepath.Join(out, "kube-do-registry.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), wantExec) {
		t.Fatalf("unit does not use absolute paths:\n%s\nwant: %s", unit, wantExec)
	}
	config, err := os.ReadFile(filepath.Join(out, "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(out, "tls", "server.crt"), filepath.Join(out, "tls", "server.key")} {
		if !strings.Contains(string(config), path) {
			t.Fatalf("config does not use absolute certificate path %q:\n%s", path, config)
		}
	}
	for _, file := range res.Files {
		if !filepath.IsAbs(file) {
			t.Errorf("dry-run result contains relative path %q", file)
		}
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func writeRegistryArchive(t *testing.T, path string) {
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
	body := []byte("fake-registry-binary")
	if err := tw.WriteHeader(&tar.Header{Name: "registry", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
}
