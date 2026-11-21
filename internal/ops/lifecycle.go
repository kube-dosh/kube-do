package ops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kube-dosh/kube-do/internal/cluster"
	"github.com/kube-dosh/kube-do/internal/install"
	"github.com/kube-dosh/kube-do/internal/manifest"
)

const (
	DefaultLifecycleTimeout = 2 * time.Hour
	RoleControlPlane        = "control-plane"
	RoleWorker              = "worker"
)

// RemoveNodeResult describes a completed node removal.
type RemoveNodeResult struct {
	Cluster   string       `json:"cluster"`
	Node      string       `json:"node"`
	Address   string       `json:"address"`
	Role      string       `json:"role"`
	RemovedAt time.Time    `json:"removedAt"`
	Backup    BackupResult `json:"backup"`
}

// UpgradeOptions controls an in-place Kubernetes upgrade.
type UpgradeOptions struct {
	Timeout   time.Duration `json:"timeout,omitempty"`
	Backup    BackupOptions `json:"backup,omitempty"`
	ExtraVars []string      `json:"extraVars,omitempty"`
}

// UpgradeResult describes a completed upgrade.
type UpgradeResult struct {
	Cluster          string       `json:"cluster"`
	FromBuild        string       `json:"fromBuild"`
	ToBuild          string       `json:"toBuild"`
	FromVersion      string       `json:"fromVersion"`
	ToVersion        string       `json:"toVersion"`
	UpgradedAt       time.Time    `json:"upgradedAt"`
	PreUpgradeBackup BackupResult `json:"preUpgradeBackup"`
}

// RollbackOptions controls restoration to a previously installed Build.
type RollbackOptions struct {
	Timeout   time.Duration `json:"timeout,omitempty"`
	ExtraVars []string      `json:"extraVars,omitempty"`
}

// RollbackResult describes a completed rollback.
type RollbackResult struct {
	Cluster        string       `json:"cluster"`
	FromBuild      string       `json:"fromBuild"`
	ToBuild        string       `json:"toBuild"`
	FromVersion    string       `json:"fromVersion"`
	ToVersion      string       `json:"toVersion"`
	SnapshotPath   string       `json:"snapshotPath"`
	SnapshotSHA256 string       `json:"snapshotSha256"`
	RolledBackAt   time.Time    `json:"rolledBackAt"`
	SafetyBackup   BackupResult `json:"safetyBackup"`
}

// RestoreOptions controls restoration of an etcd snapshot.
type RestoreOptions struct {
	Timeout      time.Duration `json:"timeout,omitempty"`
	SnapshotPath string        `json:"snapshotPath"`
	TargetNode   string        `json:"targetNode,omitempty"`
	Confirm      bool          `json:"confirm"`
	ExtraVars    []string      `json:"extraVars,omitempty"`
	Backup       BackupOptions `json:"backup,omitempty"`
}

// RestoreResult describes a completed etcd restore.
type RestoreResult struct {
	Cluster        string       `json:"cluster"`
	SnapshotPath   string       `json:"snapshotPath"`
	SnapshotSHA256 string       `json:"snapshotSha256"`
	SnapshotSize   int64        `json:"snapshotSize"`
	TargetNode     string       `json:"targetNode,omitempty"`
	RestoredAt     time.Time    `json:"restoredAt"`
	SafetyBackup   BackupResult `json:"safetyBackup"`
}

// UninstallOptions controls cluster teardown.
type UninstallOptions struct {
	Timeout   time.Duration `json:"timeout,omitempty"`
	Confirm   bool          `json:"confirm"`
	ExtraVars []string      `json:"extraVars,omitempty"`
}

// UninstallResult describes a completed teardown.
type UninstallResult struct {
	Cluster       string    `json:"cluster"`
	UninstalledAt time.Time `json:"uninstalledAt"`
}

func normalizeRole(role string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "", RoleWorker:
		return RoleWorker, nil
	case RoleControlPlane:
		return RoleControlPlane, nil
	default:
		return "", fmt.Errorf("unsupported node role %q", role)
	}
}

func findNode(nodes []cluster.Node, name string) (cluster.Node, bool) {
	for _, node := range nodes {
		if node.Name == name {
			return node, true
		}
	}
	return cluster.Node{}, false
}

func validateSnapshot(path string, expectedSHA256 string, expectedSize int64) (int64, string, error) {
	if strings.TrimSpace(path) == "" {
		return 0, "", errors.New("snapshot path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return 0, "", fmt.Errorf("resolve snapshot path: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return 0, "", fmt.Errorf("inspect snapshot %s: %w", absolute, err)
	}
	if !info.Mode().IsRegular() {
		return 0, "", fmt.Errorf("snapshot %s is not a regular file", absolute)
	}
	if info.Size() == 0 {
		return 0, "", fmt.Errorf("snapshot %s is empty", absolute)
	}
	if expectedSize > 0 && info.Size() != expectedSize {
		return 0, "", fmt.Errorf("snapshot size is %d bytes, expected %d", info.Size(), expectedSize)
	}
	sum, err := fileSHA256(absolute)
	if err != nil {
		return 0, "", err
	}
	if expectedSHA256 != "" && !strings.EqualFold(sum, expectedSHA256) {
		return 0, "", fmt.Errorf("snapshot SHA-256 is %s, expected %s", sum, expectedSHA256)
	}
	return info.Size(), sum, nil
}

func runLifecyclePlaybook(ctx context.Context, ansibleDir, workDir, playbook string, timeout time.Duration, extraVars []string, log io.Writer) error {
	if timeout <= 0 {
		timeout = DefaultLifecycleTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := install.Run(runCtx, install.Options{
		AnsibleDir: ansibleDir,
		Playbook:   playbook,
		Inventory:  filepath.Join(workDir, "inventory.ini"),
		WorkingDir: ansibleDir,
		ExtraVars:  extraVars,
		Stdout:     log,
		Stderr:     log,
	})
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%s timed out after %s", playbook, timeout)
	}
	return err
}

var kubernetesVersionRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:[-+].*)?$`)

type kubeVersion struct {
	major, minor, patch int
}

func parseKubernetesVersion(value string) (kubeVersion, error) {
	match := kubernetesVersionRe.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return kubeVersion{}, fmt.Errorf("invalid Kubernetes version %q", value)
	}
	parts := make([]int, 3)
	for i := range parts {
		n, err := strconv.Atoi(match[i+1])
		if err != nil {
			return kubeVersion{}, fmt.Errorf("invalid Kubernetes version %q", value)
		}
		parts[i] = n
	}
	return kubeVersion{major: parts[0], minor: parts[1], patch: parts[2]}, nil
}

func (v kubeVersion) compare(other kubeVersion) int {
	if v.major != other.major {
		return v.major - other.major
	}
	if v.minor != other.minor {
		return v.minor - other.minor
	}
	return v.patch - other.patch
}

// ValidateUpgradeTarget enforces the supported in-place upgrade path. Patch
// releases and a single minor-version step are accepted. Downgrades and skips
// across multiple minor releases are rejected.
func ValidateUpgradeTarget(from, to *manifest.Manifest) error {
	if from == nil || to == nil {
		return errors.New("source and target manifests are required")
	}
	if from.Metadata.Name == to.Metadata.Name {
		return fmt.Errorf("target build %q is already installed", from.Metadata.Name)
	}
	fromVersion, err := parseKubernetesVersion(from.Metadata.KubernetesVersion)
	if err != nil {
		return fmt.Errorf("source manifest: %w", err)
	}
	toVersion, err := parseKubernetesVersion(to.Metadata.KubernetesVersion)
	if err != nil {
		return fmt.Errorf("target manifest: %w", err)
	}
	if toVersion.major != fromVersion.major {
		return fmt.Errorf("upgrading across Kubernetes major versions is not supported (%s to %s)", from.Metadata.KubernetesVersion, to.Metadata.KubernetesVersion)
	}
	if toVersion.compare(fromVersion) < 0 {
		return fmt.Errorf("downgrades are not supported by kubeadm (%s to %s); use upgrade rollback", from.Metadata.KubernetesVersion, to.Metadata.KubernetesVersion)
	}
	if toVersion.minor-fromVersion.minor > 1 {
		return fmt.Errorf("upgrades may advance only one Kubernetes minor version at a time (%s to %s)", from.Metadata.KubernetesVersion, to.Metadata.KubernetesVersion)
	}
	return nil
}

func requireConfirm(confirm bool, action string) error {
	if !confirm {
		return fmt.Errorf("%s requires explicit confirmation", action)
	}
	return nil
}

func newOperationTimestamp() time.Time { return time.Now().UTC() }

func etcdInitialCluster(cfg *cluster.Config) string {
	parts := make([]string, 0, len(cfg.ControlPlane.Nodes))
	for _, node := range cfg.ControlPlane.Nodes {
		parts = append(parts, fmt.Sprintf("%s=https://%s:2380", node.Name, node.Address))
	}
	return strings.Join(parts, ",")
}
