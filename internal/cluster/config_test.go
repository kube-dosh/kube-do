package cluster

import (
	"strings"
	"testing"
)

func validConfigYAML() string {
	return `apiVersion: kube-do.io/v1alpha1
kind: ClusterConfig
metadata:
  name: test
build: v1.36.5-kube-do.1
installer:
  address: 10.0.0.10
registry:
  address: 10.0.0.10:5000
controlPlane:
  vip: 10.0.0.100
  nodes:
    - name: cp-1
      address: 10.0.0.11
cni:
  name: cilium
  kubeProxyReplacement: true
`
}

func TestParseDefaults(t *testing.T) {
	c, err := Parse([]byte(validConfigYAML()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Installer.DownloadPort != 8080 {
		t.Fatalf("downloadPort = %d", c.Installer.DownloadPort)
	}
	if c.ControlPlane.APIPort != 16443 {
		t.Fatalf("apiPort = %d", c.ControlPlane.APIPort)
	}
	if c.Registry.DataDir != "/var/lib/kube-do/registry" {
		t.Fatalf("dataDir = %q", c.Registry.DataDir)
	}
	if c.Registry.TLS.Mode != TLSModeSelfSigned {
		t.Fatalf("tls mode = %q", c.Registry.TLS.Mode)
	}
	if got := c.DownloadBaseURL(); got != "http://10.0.0.10:8080" {
		t.Fatalf("download URL = %q", got)
	}
	if got := c.ControlPlaneEndpoint(); got != "https://10.0.0.100:16443" {
		t.Fatalf("endpoint = %q", got)
	}
	if c.SSH.User != "root" || c.SSH.Port != 22 {
		t.Fatalf("ssh = %#v", c.SSH)
	}
	if c.Platform.OSFamily != OSFamilyRHEL || c.Platform.Arch != ArchAMD64 {
		t.Fatalf("platform defaults = %#v", c.Platform)
	}
	if c.CNI.PodCIDR != "10.244.0.0/16" || c.CNI.ServiceCIDR != "10.96.0.0/12" {
		t.Fatalf("cni CIDRs = %q %q", c.CNI.PodCIDR, c.CNI.ServiceCIDR)
	}
}

func TestParseExplicitPlatform(t *testing.T) {
	yaml := strings.Replace(validConfigYAML(), "build: v1.36.5-kube-do.1", "build: v1.36.5-kube-do.1\nplatform:\n  osFamily: debian\n  arch: arm64", 1)
	c, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Platform.OSFamily != OSFamilyDebian || c.Platform.Arch != ArchARM64 {
		t.Fatalf("platform = %#v", c.Platform)
	}
	if c.Platform.ManifestOS() != "linux" || c.Platform.AnsibleArchitecture() != "aarch64" {
		t.Fatalf("platform helpers = %#v", c.Platform)
	}
}

func TestValidateRejectsUnsupportedPlatform(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		{"platform:\n  osFamily: alpine\n  arch: amd64", "platform.osFamily"},
		{"platform:\n  osFamily: rhel\n  arch: riscv64", "platform.arch"},
	} {
		yaml := strings.Replace(validConfigYAML(), "build: v1.36.5-kube-do.1", "build: v1.36.5-kube-do.1\n"+tc.value, 1)
		_, err := Parse([]byte(yaml))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("expected %q error, got %v", tc.want, err)
		}
	}
}

func TestValidateRejectsDuplicateAddress(t *testing.T) {
	bad := validConfigYAML() + `workers:
  - name: worker-1
    address: 10.0.0.11
`
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "duplicate node address") {
		t.Fatalf("expected duplicate address error, got %v", err)
	}
}

func TestParseRejectsMultipleYAMLDocuments(t *testing.T) {
	_, err := Parse([]byte(validConfigYAML() + "---\nmetadata:\n  name: smuggled\n"))
	if err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("expected multiple document error, got %v", err)
	}
}

func TestValidateRejectsNodeAddressesInsideClusterCIDRs(t *testing.T) {
	for _, tc := range []struct {
		from string
		to   string
		want string
	}{
		{"cni:\n  name: cilium", "cni:\n  name: cilium\n  podCIDR: 10.0.0.0/8", "cni.podCIDR"},
		{"cni:\n  name: cilium", "cni:\n  name: cilium\n  serviceCIDR: 10.0.0.0/8", "cni.serviceCIDR"},
	} {
		bad := strings.Replace(validConfigYAML(), tc.from, tc.to, 1)
		_, err := Parse([]byte(bad))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("expected %s collision error, got %v", tc.want, err)
		}
	}
}

func TestValidateRejectsVIPInsideClusterCIDRs(t *testing.T) {
	bad := strings.Replace(validConfigYAML(), "cni:\n  name: cilium", "cni:\n  name: cilium\n  podCIDR: 10.0.0.0/8", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "controlPlane.vip") {
		t.Fatalf("expected VIP collision error, got %v", err)
	}
}

func TestValidateRejectsBadRegistryAddress(t *testing.T) {
	bad := strings.Replace(validConfigYAML(), "address: 10.0.0.10:5000", "address: 10.0.0.10", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "host:port") {
		t.Fatalf("expected registry address error, got %v", err)
	}
}

func TestValidateRequiresProvidedTLSFiles(t *testing.T) {
	bad := strings.Replace(validConfigYAML(), "registry:\n  address: 10.0.0.10:5000", "registry:\n  address: 10.0.0.10:5000\n  tls:\n    mode: provided", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "requires caFile") {
		t.Fatalf("expected TLS files error, got %v", err)
	}
}

func TestValidateRejectsRelativeRegistryDataDir(t *testing.T) {
	bad := strings.Replace(validConfigYAML(), "registry:\n  address: 10.0.0.10:5000", "registry:\n  address: 10.0.0.10:5000\n  dataDir: registry-data", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "registry.dataDir must be an absolute path") {
		t.Fatalf("expected absolute dataDir error, got %v", err)
	}
}

func TestValidateRejectsRelativeProvidedTLSPaths(t *testing.T) {
	bad := strings.Replace(validConfigYAML(), "registry:\n  address: 10.0.0.10:5000", "registry:\n  address: 10.0.0.10:5000\n  tls:\n    mode: provided\n    caFile: certs/ca.crt\n    certFile: /tmp/server.crt\n    keyFile: /tmp/server.key", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "registry.tls.caFile must be an absolute path") {
		t.Fatalf("expected absolute CA path error, got %v", err)
	}
}

func TestValidateRejectsTLSFilesInSelfSignedMode(t *testing.T) {
	bad := strings.Replace(validConfigYAML(), "registry:\n  address: 10.0.0.10:5000", "registry:\n  address: 10.0.0.10:5000\n  tls:\n    mode: selfSigned\n    caFile: /tmp/ca.crt", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "does not accept caFile") {
		t.Fatalf("expected self-signed TLS file error, got %v", err)
	}
}

func TestRegistryCAPathFollowsTLSMode(t *testing.T) {
	c, err := Parse([]byte(validConfigYAML()))
	if err != nil {
		t.Fatalf("Parse self-signed config: %v", err)
	}
	if got := c.RegistryCAPath(); got != DefaultRegistryCAPath {
		t.Fatalf("self-signed CA path = %q, want %q", got, DefaultRegistryCAPath)
	}

	provided := strings.Replace(validConfigYAML(), "registry:\n  address: 10.0.0.10:5000", "registry:\n  address: 10.0.0.10:5000\n  tls:\n    mode: provided\n    caFile: /tmp/ca.crt\n    certFile: /tmp/server.crt\n    keyFile: /tmp/server.key", 1)
	c, err = Parse([]byte(provided))
	if err != nil {
		t.Fatalf("Parse provided config: %v", err)
	}
	if got := c.RegistryCAPath(); got != "/tmp/ca.crt" {
		t.Fatalf("provided CA path = %q", got)
	}
}

func TestValidateRejectsKubeProxyWithoutReplacement(t *testing.T) {
	bad := strings.Replace(validConfigYAML(), "kubeProxyReplacement: true", "kubeProxyReplacement: false", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "kubeProxyReplacement must be true") {
		t.Fatalf("expected kube-proxy error, got %v", err)
	}
}

func TestValidateRejectsAPIPort6443(t *testing.T) {
	bad := strings.Replace(validConfigYAML(), "  vip: 10.0.0.100", "  vip: 10.0.0.100\n  apiPort: 6443", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "must not be 6443") {
		t.Fatalf("expected apiPort conflict error, got %v", err)
	}
}

func TestValidateRejectsBadVRRPPassword(t *testing.T) {
	bad := strings.Replace(validConfigYAML(), "  vip: 10.0.0.100", "  vip: 10.0.0.100\n  vrrp:\n    password: too-long-password", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "vrrp.password") {
		t.Fatalf("expected vrrp password error, got %v", err)
	}
}

func TestValidateRejectsNonIPv4NodeAddress(t *testing.T) {
	for _, address := range []string{`"2001:db8::1"`, "cp-1.example.com"} {
		bad := strings.Replace(validConfigYAML(), "address: 10.0.0.11", "address: "+address, 1)
		_, err := Parse([]byte(bad))
		if err == nil || !strings.Contains(err.Error(), "must be an IPv4 address") {
			t.Fatalf("address %s: expected IPv4 error, got %v", address, err)
		}
	}
}

func TestValidateRejectsInvalidNodeName(t *testing.T) {
	bad := strings.Replace(validConfigYAML(), "name: cp-1", "name: CP_1", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "lowercase DNS subdomain") {
		t.Fatalf("expected node name error, got %v", err)
	}
}

func TestValidateRejectsWhitespaceInSSHUser(t *testing.T) {
	bad := validConfigYAML() + "ssh:\n  user: \"root user\"\n"
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "ssh.user must not contain whitespace") {
		t.Fatalf("expected ssh user error, got %v", err)
	}
}

func TestValidateRejectsInvalidInstallerAddress(t *testing.T) {
	bad := strings.Replace(validConfigYAML(), "address: 10.0.0.10", "address: http://10.0.0.10", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "installer.address") {
		t.Fatalf("expected installer address error, got %v", err)
	}
}

func TestValidateRejectsOverlappingCIDRs(t *testing.T) {
	bad := strings.Replace(validConfigYAML(), "  kubeProxyReplacement: true", "  kubeProxyReplacement: true\n  podCIDR: 10.96.0.0/16", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "must not overlap") {
		t.Fatalf("expected CIDR overlap error, got %v", err)
	}
}

func TestValidateRejectsNonIPv4CIDR(t *testing.T) {
	bad := strings.Replace(validConfigYAML(), "  kubeProxyReplacement: true", "  kubeProxyReplacement: true\n  podCIDR: fd00::/64", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "IPv4 CIDR") {
		t.Fatalf("expected IPv4 CIDR error, got %v", err)
	}
}

func TestValidateRejectsUnsafeBuildName(t *testing.T) {
	bad := strings.Replace(validConfigYAML(), "build: v1.36.5-kube-do.1", "build: ../escape", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "safe single path segment") {
		t.Fatalf("expected build name error, got %v", err)
	}
}

func TestValidateRejectsUnsafeClusterName(t *testing.T) {
	bad := strings.Replace(validConfigYAML(), "name: test", "name: test cluster", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "metadata.name") {
		t.Fatalf("expected cluster name error, got %v", err)
	}
}

func TestValidateRejectsWhitespaceInPrivateKeyPath(t *testing.T) {
	bad := validConfigYAML() + "ssh:\n  privateKeyFile: /root/.ssh/id with space\n"
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "privateKeyFile") {
		t.Fatalf("expected private key error, got %v", err)
	}
}

func TestValidateRejectsUnsafeVRRPPassword(t *testing.T) {
	bad := strings.Replace(validConfigYAML(), "  vip: 10.0.0.100", "  vip: 10.0.0.100\n  vrrp:\n    password: 'abc}def'", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "vrrp.password") {
		t.Fatalf("expected vrrp password error, got %v", err)
	}
}
