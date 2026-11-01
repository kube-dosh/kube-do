// Package registry deploys the private Registry on the Installer Host.
package registry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/kubedo/kubedo/internal/archive"
)

const (
	TLSModeSelfSigned = "selfSigned"
	TLSModeProvided   = "provided"
)

type Options struct {
	Address       string
	DataDir       string
	TLSMode       string
	CAFile        string
	CertFile      string
	KeyFile       string
	SourceArchive string

	// DryRun renders into OutputDir and skips systemd.
	DryRun    bool
	OutputDir string

	ProbeTimeout time.Duration
	Runner       func(name string, args ...string) error
	Logf         func(format string, args ...any)
}

type Result struct {
	Reused bool
	Files  []string
}

// Ensure reuses an existing Registry or deploys one from SourceArchive.
func Ensure(ctx context.Context, opts Options) (Result, error) {
	var res Result
	if opts.Address == "" {
		return res, errors.New("registry address is required")
	}
	if opts.DataDir == "" {
		return res, errors.New("registry data dir is required")
	}
	if opts.DryRun && opts.OutputDir == "" {
		return res, errors.New("output dir is required in dry-run mode")
	}
	if opts.DryRun {
		absoluteOutputDir, err := filepath.Abs(opts.OutputDir)
		if err != nil {
			return res, fmt.Errorf("resolve dry-run output dir: %w", err)
		}
		opts.OutputDir = absoluteOutputDir
	}
	host, port, err := net.SplitHostPort(opts.Address)
	if err != nil {
		return res, fmt.Errorf("registry address must be host:port: %w", err)
	}
	if opts.ProbeTimeout == 0 {
		opts.ProbeTimeout = 5 * time.Second
	}

	caFile := opts.CAFile
	if opts.TLSMode == TLSModeSelfSigned || opts.TLSMode == "" {
		caFile = filepath.Join(opts.certDir(), "ca.crt")
	}
	if !opts.DryRun {
		if err := Probe(ctx, opts.Address, caFile, opts.ProbeTimeout); err == nil {
			logf(opts, "registry %s is reachable; reusing it", opts.Address)
			res.Reused = true
			return res, nil
		} else if portInUse(opts.Address, opts.ProbeTimeout) {
			return res, fmt.Errorf("port %s is in use but the Registry probe failed (existing TLS certificate?): %w", opts.Address, err)
		}
		if !isLocalHost(host) {
			return res, fmt.Errorf("registry %s is not on this Installer Host; deploy it there or point cluster.yaml at a reachable Registry", opts.Address)
		}
	}
	if opts.SourceArchive == "" {
		return res, errors.New("registry binary archive is missing; run 'kubedo download --binaries-only' first")
	}

	switch opts.TLSMode {
	case TLSModeSelfSigned, "":
		if err := writeSelfSigned(opts, host, &res); err != nil {
			return res, err
		}
	case TLSModeProvided:
		if opts.CertFile == "" || opts.KeyFile == "" || opts.CAFile == "" {
			return res, errors.New("provided TLS requires caFile, certFile and keyFile")
		}
		for _, f := range []string{opts.CAFile, opts.CertFile, opts.KeyFile} {
			if _, err := os.Stat(f); err != nil {
				return res, fmt.Errorf("TLS file %s: %w", f, err)
			}
		}
	default:
		return res, fmt.Errorf("unknown TLS mode %q", opts.TLSMode)
	}

	if !opts.DryRun {
		if err := os.MkdirAll(opts.DataDir, 0o755); err != nil {
			return res, fmt.Errorf("create Registry data directory: %w", err)
		}
	}

	binPath := opts.binaryPath()
	if err := installBinary(opts.SourceArchive, binPath); err != nil {
		return res, err
	}
	res.Files = append(res.Files, binPath)

	certFile, keyFile := opts.CertFile, opts.KeyFile
	if opts.TLSMode == TLSModeSelfSigned || opts.TLSMode == "" {
		certFile = filepath.Join(opts.certDir(), "server.crt")
		keyFile = filepath.Join(opts.certDir(), "server.key")
	}
	configPath := opts.configPath()
	if err := writeFile(configPath, RenderConfig(opts.DataDir, port, certFile, keyFile), 0o644); err != nil {
		return res, err
	}
	res.Files = append(res.Files, configPath)

	unitPath := opts.unitPath()
	if err := writeFile(unitPath, RenderUnit(binPath, configPath), 0o644); err != nil {
		return res, err
	}
	res.Files = append(res.Files, unitPath)

	if opts.DryRun {
		logf(opts, "dry-run: wrote registry bundle to %s", opts.OutputDir)
		return res, nil
	}
	runner := opts.Runner
	if runner == nil {
		runner = defaultRunner
	}
	if err := runner("systemctl", "daemon-reload"); err != nil {
		return res, fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if err := runner("systemctl", "enable", "--now", "kubedo-registry.service"); err != nil {
		return res, fmt.Errorf("start kubedo-registry.service: %w", err)
	}
	if err := waitReady(ctx, opts, caFile); err != nil {
		return res, err
	}
	logf(opts, "registry deployed at %s", opts.Address)
	return res, nil
}

func waitReady(ctx context.Context, opts Options, caFile string) error {
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for {
		probeCtx, cancel := context.WithTimeout(ctx, opts.ProbeTimeout)
		lastErr = Probe(probeCtx, opts.Address, caFile, opts.ProbeTimeout)
		cancel()
		if lastErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("registry did not become ready at %s: %w", opts.Address, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func writeSelfSigned(opts Options, host string, res *Result) error {
	dir := opts.certDir()
	caPath := filepath.Join(dir, "ca.crt")
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")
	if fileExists(caPath) && fileExists(certPath) && fileExists(keyPath) && certificateUsable(caPath, certPath, keyPath, host) {
		logf(opts, "reusing existing self-signed certificates in %s", dir)
		res.Files = append(res.Files, caPath, certPath, keyPath)
		return nil
	}
	if fileExists(caPath) && fileExists(certPath) && fileExists(keyPath) {
		logf(opts, "existing self-signed certificate does not match %s; regenerating", host)
	}
	caPEM, certPEM, keyPEM, err := GenerateSelfSigned(host)
	if err != nil {
		return fmt.Errorf("generate self-signed certificates: %w", err)
	}
	for _, f := range []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{caPath, caPEM, 0o644},
		{certPath, certPEM, 0o644},
		{keyPath, keyPEM, 0o600},
	} {
		if err := writeFile(f.path, f.data, f.mode); err != nil {
			return err
		}
		res.Files = append(res.Files, f.path)
	}
	logf(opts, "generated self-signed certificates in %s", dir)
	return nil
}

func certificateUsable(caPath, certPath, keyPath, host string) bool {
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return false
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return false
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		return false
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return false
	}
	_, err = cert.Verify(x509.VerifyOptions{
		Roots:     roots,
		DNSName:   host,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	return err == nil
}

func installBinary(archivePath, dest string) error {
	tmp, err := os.MkdirTemp("", "kubedo-registry-bin-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	written, err := archive.ExtractTarGz(archivePath, tmp)
	if err != nil {
		return fmt.Errorf("extract registry archive: %w", err)
	}
	src := ""
	for _, f := range written {
		if filepath.Base(f) == "registry" {
			src = f
			break
		}
	}
	if src == "" {
		return errors.New("registry binary not found in archive")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFile(dest, data, 0o755)
}

// RenderConfig returns the Distribution config for this Registry.
func RenderConfig(dataDir, port, certFile, keyFile string) []byte {
	return []byte(fmt.Sprintf(`version: 0.1
log:
  level: info
storage:
  filesystem:
    rootdirectory: %q
  delete:
    enabled: true
http:
  addr: :%s
  headers:
    X-Content-Type-Options: [nosniff]
  tls:
    certificate: %q
    key: %q
`, dataDir, port, certFile, keyFile))
}

// RenderUnit returns the systemd unit for the Registry.
func RenderUnit(binPath, configPath string) []byte {
	return []byte(fmt.Sprintf(`[Unit]
Description=kubedo private container registry
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Environment=OTEL_TRACES_EXPORTER=none
ExecStart=%s serve %s
Restart=on-failure
RestartSec=5
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
`, binPath, configPath))
}

func (o Options) binaryPath() string {
	if o.DryRun {
		return filepath.Join(o.OutputDir, "bin", "registry")
	}
	return "/usr/local/bin/registry"
}

func (o Options) configPath() string {
	if o.DryRun {
		return filepath.Join(o.OutputDir, "config.yml")
	}
	return "/etc/kubedo/registry/config.yml"
}

func (o Options) certDir() string {
	if o.DryRun {
		return filepath.Join(o.OutputDir, "tls")
	}
	return "/etc/kubedo/registry/tls"
}

func (o Options) unitPath() string {
	if o.DryRun {
		return filepath.Join(o.OutputDir, "kubedo-registry.service")
	}
	return "/etc/systemd/system/kubedo-registry.service"
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, mode)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func isLocalHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ips := []net.IP{}
	if ip := net.ParseIP(host); ip != nil {
		ips = append(ips, ip)
	} else {
		resolved, err := net.LookupIP(host)
		if err != nil {
			return false
		}
		ips = append(ips, resolved...)
	}
	for _, ip := range ips {
		if ip.IsLoopback() {
			return true
		}
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		for _, ip := range ips {
			if ipnet.IP.Equal(ip) {
				return true
			}
		}
	}
	return false
}

func defaultRunner(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func logf(opts Options, format string, args ...any) {
	if opts.Logf != nil {
		opts.Logf(format, args...)
	}
}
