// Package cluster models cluster.yaml, the single user-facing configuration.
package cluster

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	APIVersion = "kubedo.io/v1alpha1"
	Kind       = "ClusterConfig"

	OSFamilyRHEL   = "rhel"
	OSFamilyDebian = "debian"

	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"

	TLSModeSelfSigned = "selfSigned"
	TLSModeProvided   = "provided"

	DefaultRegistryCAPath = "/etc/kubedo/registry/tls/ca.crt"
)

var (
	dns1123SubdomainRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*$`)
	hostnameRe         = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)
	buildNameRe        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	vrrpPasswordRe     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,8}$`)
)

type Config struct {
	APIVersion   string       `yaml:"apiVersion"`
	Kind         string       `yaml:"kind"`
	Metadata     Metadata     `yaml:"metadata"`
	Build        string       `yaml:"build"`
	Platform     Platform     `yaml:"platform,omitempty"`
	Installer    Installer    `yaml:"installer"`
	Registry     Registry     `yaml:"registry"`
	SSH          SSH          `yaml:"ssh,omitempty"`
	ControlPlane ControlPlane `yaml:"controlPlane"`
	Workers      []Node       `yaml:"workers"`
	CNI          CNI          `yaml:"cni"`
}

type Metadata struct {
	Name string `yaml:"name"`
}

// Platform selects the target node OS family and CPU architecture. The
// Resource Manifest still uses the generic Kubernetes OS name (linux); the OS
// family only selects the distribution-specific Ansible behavior.
type Platform struct {
	OSFamily string `yaml:"osFamily"`
	Arch     string `yaml:"arch"`
}

// ManifestOS is the OS string used by Resource Manifests for this platform.
func (p Platform) ManifestOS() string { return "linux" }

// AnsibleArchitecture maps Go/OCI architecture names to Ansible's uname values.
func (p Platform) AnsibleArchitecture() string {
	switch p.Arch {
	case ArchAMD64:
		return "x86_64"
	case ArchARM64:
		return "aarch64"
	default:
		return ""
	}
}

type Installer struct {
	Address      string `yaml:"address"`
	DownloadPort int    `yaml:"downloadPort"`
}

type Registry struct {
	Address         string `yaml:"address"`
	DataDir         string `yaml:"dataDir"`
	CredentialsFile string `yaml:"credentialsFile,omitempty"`
	TLS             TLS    `yaml:"tls"`
}

// SSH describes how Ansible reaches cluster nodes from the Installer Host.
type SSH struct {
	User           string `yaml:"user,omitempty"`
	Port           int    `yaml:"port,omitempty"`
	PrivateKeyFile string `yaml:"privateKeyFile,omitempty"`
}

type TLS struct {
	Mode     string `yaml:"mode"`
	CAFile   string `yaml:"caFile,omitempty"`
	CertFile string `yaml:"certFile,omitempty"`
	KeyFile  string `yaml:"keyFile,omitempty"`
}

type ControlPlane struct {
	VIP     string `yaml:"vip"`
	APIPort int    `yaml:"apiPort"`
	Nodes   []Node `yaml:"nodes"`
	VRRP    VRRP   `yaml:"vrrp,omitempty"`
}

// VRRP configures keepalived. Password must be 1-8 characters because VRRPv2
// authentication truncates longer values.
type VRRP struct {
	RouterID int    `yaml:"routerId,omitempty"`
	Password string `yaml:"password,omitempty"`
}

type Node struct {
	Name    string `yaml:"name"`
	Address string `yaml:"address"`
}

type CNI struct {
	Name                 string `yaml:"name"`
	KubeProxyReplacement *bool  `yaml:"kubeProxyReplacement"`
	PodCIDR              string `yaml:"podCIDR,omitempty"`
	ServiceCIDR          string `yaml:"serviceCIDR,omitempty"`
	ClusterDomain        string `yaml:"clusterDomain,omitempty"`
}

// Parse decodes, defaults and validates cluster.yaml.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("cluster config is empty")
		}
		return nil, fmt.Errorf("decode cluster config: %w", err)
	}
	c.Default()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// LoadFile reads cluster.yaml from disk.
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read cluster config: %w", err)
	}
	return Parse(data)
}

// Default fills in optional settings.
func (c *Config) Default() {
	if c.Platform.OSFamily == "" {
		c.Platform.OSFamily = OSFamilyRHEL
	}
	if c.Platform.Arch == "" {
		c.Platform.Arch = ArchAMD64
	}
	if c.Installer.DownloadPort == 0 {
		c.Installer.DownloadPort = 8080
	}
	if c.SSH.User == "" {
		c.SSH.User = "root"
	}
	if c.SSH.Port == 0 {
		c.SSH.Port = 22
	}
	if c.Registry.DataDir == "" {
		c.Registry.DataDir = "/var/lib/kubedo/registry"
	}
	if c.Registry.TLS.Mode == "" {
		c.Registry.TLS.Mode = TLSModeSelfSigned
	}
	if c.ControlPlane.APIPort == 0 {
		c.ControlPlane.APIPort = 16443
	}
	if c.ControlPlane.VRRP.RouterID == 0 {
		c.ControlPlane.VRRP.RouterID = 51
	}
	if c.ControlPlane.VRRP.Password == "" {
		c.ControlPlane.VRRP.Password = "kubedo"
	}
	if c.CNI.Name == "" {
		c.CNI.Name = "cilium"
	}
	if c.CNI.PodCIDR == "" {
		c.CNI.PodCIDR = "10.244.0.0/16"
	}
	if c.CNI.ServiceCIDR == "" {
		c.CNI.ServiceCIDR = "10.96.0.0/12"
	}
	if c.CNI.ClusterDomain == "" {
		c.CNI.ClusterDomain = "cluster.local"
	}
}

// KubeProxyReplacementEnabled reports the effective CNI mode (default true).
func (c *Config) KubeProxyReplacementEnabled() bool {
	return c.CNI.KubeProxyReplacement == nil || *c.CNI.KubeProxyReplacement
}

// DownloadBaseURL is the base URL served by `kubedo serve`.
func (c *Config) DownloadBaseURL() string {
	return "http://" + net.JoinHostPort(c.Installer.Address, strconv.Itoa(c.Installer.DownloadPort))
}

// ControlPlaneEndpoint is the stable API endpoint backed by the VIP.
func (c *Config) ControlPlaneEndpoint() string {
	return "https://" + c.ControlPlaneAddress()
}

// ControlPlaneAddress is the host:port kubeadm accepts as controlPlaneEndpoint.
func (c *Config) ControlPlaneAddress() string {
	return net.JoinHostPort(c.ControlPlane.VIP, strconv.Itoa(c.ControlPlane.APIPort))
}

// RegistryCAPath is the CA nodes must trust to pull images. In self-signed
// mode it is the path written by `kubedo registry`.
func (c *Config) RegistryCAPath() string {
	switch c.Registry.TLS.Mode {
	case TLSModeSelfSigned:
		return DefaultRegistryCAPath
	case TLSModeProvided:
		return c.Registry.TLS.CAFile
	}
	return ""
}

// Validate checks the configuration for internal consistency.
func (c *Config) Validate() error {
	var errs []string
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	if c.APIVersion != APIVersion {
		add("apiVersion must be %q, got %q", APIVersion, c.APIVersion)
	}
	if c.Kind != Kind {
		add("kind must be %q, got %q", Kind, c.Kind)
	}
	if strings.TrimSpace(c.Metadata.Name) == "" {
		add("metadata.name is required")
	} else if !validDNS1123Subdomain(c.Metadata.Name) {
		add("metadata.name must be a lowercase DNS subdomain, got %q", c.Metadata.Name)
	}
	if strings.TrimSpace(c.Build) == "" {
		add("build is required")
	} else if !buildNameRe.MatchString(c.Build) {
		add("build must be a safe single path segment (letters, digits, '.', '_' or '-'), got %q", c.Build)
	}
	switch c.Platform.OSFamily {
	case OSFamilyRHEL, OSFamilyDebian:
	default:
		add("platform.osFamily must be %q or %q, got %q", OSFamilyRHEL, OSFamilyDebian, c.Platform.OSFamily)
	}
	switch c.Platform.Arch {
	case ArchAMD64, ArchARM64:
	default:
		add("platform.arch must be %q or %q, got %q", ArchAMD64, ArchARM64, c.Platform.Arch)
	}
	if strings.TrimSpace(c.Installer.Address) == "" {
		add("installer.address is required")
	} else if !validHost(c.Installer.Address) {
		add("installer.address must be an IP address or DNS hostname without a scheme or port, got %q", c.Installer.Address)
	}
	if c.Installer.DownloadPort < 1 || c.Installer.DownloadPort > 65535 {
		add("installer.downloadPort must be between 1 and 65535")
	}
	if strings.TrimSpace(c.SSH.User) == "" {
		add("ssh.user must not be empty")
	} else if strings.ContainsAny(c.SSH.User, " \t\r\n") {
		add("ssh.user must not contain whitespace")
	}
	if c.SSH.Port < 1 || c.SSH.Port > 65535 {
		add("ssh.port must be between 1 and 65535")
	}
	if c.SSH.PrivateKeyFile != "" && strings.ContainsAny(c.SSH.PrivateKeyFile, " \t\r\n") {
		add("ssh.privateKeyFile must not contain whitespace")
	}

	if c.Registry.Address == "" {
		add("registry.address is required")
	} else {
		host, port, err := net.SplitHostPort(c.Registry.Address)
		if err != nil || host == "" || port == "" {
			add("registry.address must be host:port, got %q", c.Registry.Address)
		} else if !validHost(host) {
			add("registry.address host must be an IP address or DNS hostname, got %q", host)
		} else if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
			add("registry.address has an invalid port: %q", c.Registry.Address)
		}
	}
	if strings.TrimSpace(c.Registry.DataDir) == "" {
		add("registry.dataDir is required")
	} else if !filepath.IsAbs(c.Registry.DataDir) {
		add("registry.dataDir must be an absolute path, got %q", c.Registry.DataDir)
	}
	switch c.Registry.TLS.Mode {
	case TLSModeSelfSigned:
		if c.Registry.TLS.CAFile != "" || c.Registry.TLS.CertFile != "" || c.Registry.TLS.KeyFile != "" {
			add("registry.tls.mode=selfSigned does not accept caFile, certFile or keyFile; use mode=provided for an existing Registry")
		}
	case TLSModeProvided:
		if c.Registry.TLS.CAFile == "" || c.Registry.TLS.CertFile == "" || c.Registry.TLS.KeyFile == "" {
			add("registry.tls.mode=provided requires caFile, certFile and keyFile")
		}
		for _, item := range []struct {
			name string
			path string
		}{
			{"registry.tls.caFile", c.Registry.TLS.CAFile},
			{"registry.tls.certFile", c.Registry.TLS.CertFile},
			{"registry.tls.keyFile", c.Registry.TLS.KeyFile},
		} {
			if item.path != "" && !filepath.IsAbs(item.path) {
				add("%s must be an absolute path, got %q", item.name, item.path)
			}
		}
	default:
		add("registry.tls.mode must be %q or %q, got %q", TLSModeSelfSigned, TLSModeProvided, c.Registry.TLS.Mode)
	}

	if strings.TrimSpace(c.ControlPlane.VIP) == "" {
		add("controlPlane.vip is required")
	}
	if c.ControlPlane.APIPort < 1 || c.ControlPlane.APIPort > 65535 {
		add("controlPlane.apiPort must be between 1 and 65535")
	}
	if c.ControlPlane.APIPort == 6443 {
		add("controlPlane.apiPort must not be 6443: HAProxy needs a frontend port distinct from the local kube-apiserver")
	}
	if c.ControlPlane.VRRP.RouterID < 1 || c.ControlPlane.VRRP.RouterID > 255 {
		add("controlPlane.vrrp.routerId must be between 1 and 255")
	}
	if !vrrpPasswordRe.MatchString(c.ControlPlane.VRRP.Password) {
		add("controlPlane.vrrp.password must be 1-8 characters from A-Z, a-z, 0-9, '.', '_' or '-'")
	}
	if ip := net.ParseIP(c.ControlPlane.VIP); ip == nil || ip.To4() == nil {
		add("controlPlane.vip must be an IPv4 address")
	}
	if len(c.ControlPlane.Nodes) == 0 {
		add("controlPlane.nodes must not be empty")
	}

	names := map[string]bool{}
	addresses := map[string]bool{}
	checkNodes := func(where string, nodes []Node) {
		for i, n := range nodes {
			prefix := fmt.Sprintf("%s[%d]", where, i)
			if strings.TrimSpace(n.Name) == "" {
				add("%s: name is required", prefix)
			} else if !validDNS1123Subdomain(n.Name) {
				add("%s: name must be a lowercase DNS subdomain, got %q", prefix, n.Name)
			} else if names[n.Name] {
				add("%s: duplicate node name %q", prefix, n.Name)
			}
			names[n.Name] = true
			ip := net.ParseIP(n.Address)
			if n.Address == "" {
				add("%s: address is required", prefix)
			} else if ip == nil || ip.To4() == nil {
				add("%s: address must be an IPv4 address, got %q", prefix, n.Address)
			} else if addresses[ip.String()] {
				add("%s: duplicate node address %q", prefix, n.Address)
			}
			if ip != nil && ip.To4() != nil {
				addresses[ip.String()] = true
			}
		}
	}
	checkNodes("controlPlane.nodes", c.ControlPlane.Nodes)
	checkNodes("workers", c.Workers)
	if vip := net.ParseIP(c.ControlPlane.VIP); vip != nil {
		if vip4 := vip.To4(); vip4 != nil && addresses[vip4.String()] {
			add("controlPlane.vip %q collides with a node address", c.ControlPlane.VIP)
		}
	}

	if c.CNI.Name != "cilium" {
		add("cni.name must be %q in this Build, got %q", "cilium", c.CNI.Name)
	}
	if !c.KubeProxyReplacementEnabled() {
		add("cni.kubeProxyReplacement must be true: this Build does not include kube-proxy images")
	}
	if strings.TrimSpace(c.CNI.ClusterDomain) == "" {
		add("cni.clusterDomain must not be empty")
	} else if !validDNS1123Subdomain(c.CNI.ClusterDomain) {
		add("cni.clusterDomain must be a lowercase DNS domain, got %q", c.CNI.ClusterDomain)
	}
	podNetwork := validateIPv4CIDR(&errs, "cni.podCIDR", c.CNI.PodCIDR)
	serviceNetwork := validateIPv4CIDR(&errs, "cni.serviceCIDR", c.CNI.ServiceCIDR)
	if podNetwork != nil && serviceNetwork != nil &&
		(podNetwork.Contains(serviceNetwork.IP) || serviceNetwork.Contains(podNetwork.IP)) {
		add("cni.podCIDR and cni.serviceCIDR must not overlap")
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid cluster config: %s", strings.Join(errs, "; "))
	}
	return nil
}

func validHost(value string) bool {
	if value == "" || strings.ContainsAny(value, " \t\r\n/") || strings.Contains(value, "://") {
		return false
	}
	return net.ParseIP(value) != nil || (len(value) <= 253 && hostnameRe.MatchString(value))
}

func validDNS1123Subdomain(value string) bool {
	return len(value) <= 253 && dns1123SubdomainRe.MatchString(value)
}

func validateIPv4CIDR(errs *[]string, name, value string) *net.IPNet {
	ip, network, err := net.ParseCIDR(value)
	if err != nil || ip.To4() == nil || network.IP.To4() == nil {
		*errs = append(*errs, fmt.Sprintf("%s must be a valid IPv4 CIDR, got %q", name, value))
		return nil
	}
	return network
}

// ValidateURL is a hook used by tests and future commands.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("must be an absolute http(s) URL")
	}
	return nil
}
