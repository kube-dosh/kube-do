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
	DefaultCertCheckTimeout = 10 * time.Minute
	DefaultCertWarnDays     = 90
	DefaultCertCriticalDays = 30
)

// CertCheckOptions controls certificate expiry classification.
type CertCheckOptions struct {
	Timeout      time.Duration `json:"timeout,omitempty"`
	WarnDays     int           `json:"warnDays,omitempty"`
	CriticalDays int           `json:"criticalDays,omitempty"`
}

// CertificateInfo is one certificate or certificate authority reported by kubeadm.
type CertificateInfo struct {
	Name                 string     `json:"name"`
	ExpiresAt            *time.Time `json:"expiresAt,omitempty"`
	DaysRemaining        int        `json:"daysRemaining"`
	Authority            string     `json:"authority,omitempty"`
	CertificateAuthority bool       `json:"certificateAuthority,omitempty"`
	ExternallyManaged    bool       `json:"externallyManaged"`
	Severity             string     `json:"severity"`
}

// CertNodeResult contains certificate information from one control-plane node.
type CertNodeResult struct {
	Name         string            `json:"name"`
	Address      string            `json:"address"`
	Certificates []CertificateInfo `json:"certificates"`
	Missing      []string          `json:"missing,omitempty"`
}

// CertCheckResult summarizes certificate expiry and missing-certificate findings.
type CertCheckResult struct {
	Cluster          string           `json:"cluster"`
	CheckedAt        time.Time        `json:"checkedAt"`
	WarnDays         int              `json:"warnDays"`
	CriticalDays     int              `json:"criticalDays"`
	CertificateCount int              `json:"certificateCount"`
	WarningCount     int              `json:"warningCount"`
	CriticalCount    int              `json:"criticalCount"`
	ExpiredCount     int              `json:"expiredCount"`
	MissingCount     int              `json:"missingCount"`
	Healthy          bool             `json:"healthy"`
	Nodes            []CertNodeResult `json:"nodes"`
}

func (o CertCheckOptions) withDefaults() CertCheckOptions {
	if o.Timeout <= 0 {
		o.Timeout = DefaultCertCheckTimeout
	}
	if o.WarnDays <= 0 {
		o.WarnDays = DefaultCertWarnDays
	}
	if o.CriticalDays <= 0 {
		o.CriticalDays = DefaultCertCriticalDays
	}
	if o.CriticalDays > o.WarnDays {
		o.CriticalDays = o.WarnDays
	}
	return o
}

// CheckCerts runs kubeadm certificate expiration checks on every control-plane
// node and parses the per-node output into a structured result.
func CheckCerts(ctx context.Context, cfg *cluster.Config, m *manifest.Manifest, ansibleDir, workDir string, opts CertCheckOptions, log io.Writer) (CertCheckResult, error) {
	var result CertCheckResult
	if cfg == nil {
		return result, errors.New("cluster config is nil")
	}
	if m == nil {
		return result, errors.New("resource manifest is nil")
	}
	opts = opts.withDefaults()
	ansibleDir, workDir, err := prepareDirs(ansibleDir, workDir)
	if err != nil {
		return result, err
	}
	if err := renderForOps(cfg, m, workDir, log); err != nil {
		return result, err
	}
	outputDir := filepath.Join(workDir, "cert-checks")
	if err := os.RemoveAll(outputDir); err != nil {
		return result, fmt.Errorf("reset certificate output directory: %w", err)
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return result, fmt.Errorf("create certificate output directory: %w", err)
	}
	logf(log, "==> checking Kubernetes certificate expiration")
	runCtx, cancel := contextWithDefaultTimeout(ctx, opts.Timeout)
	defer cancel()
	if err := install.Run(runCtx, install.Options{
		AnsibleDir: ansibleDir,
		Playbook:   "ops-certs-check.yml",
		Inventory:  filepath.Join(workDir, "inventory.ini"),
		WorkingDir: ansibleDir,
		ExtraVars:  []string{"kube_do_cert_output_dir=" + outputDir},
		Stdout:     log,
		Stderr:     log,
	}); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return result, fmt.Errorf("certificate check timed out after %s", opts.Timeout)
		}
		return result, err
	}

	now := time.Now().UTC()
	result = CertCheckResult{
		Cluster:      cfg.Metadata.Name,
		CheckedAt:    now,
		WarnDays:     opts.WarnDays,
		CriticalDays: opts.CriticalDays,
		Nodes:        make([]CertNodeResult, 0, len(cfg.ControlPlane.Nodes)),
	}
	for _, node := range cfg.ControlPlane.Nodes {
		path := filepath.Join(outputDir, node.Name+".txt")
		data, err := os.ReadFile(path)
		if err != nil {
			return result, fmt.Errorf("read certificate output for %s: %w", node.Name, err)
		}
		nodeResult, err := parseCertOutput(string(data), now, opts)
		if err != nil {
			return result, fmt.Errorf("parse certificate output for %s: %w", node.Name, err)
		}
		nodeResult.Name = node.Name
		nodeResult.Address = node.Address
		result.Nodes = append(result.Nodes, nodeResult)
		result.CertificateCount += len(nodeResult.Certificates)
		result.MissingCount += len(nodeResult.Missing)
		for _, cert := range nodeResult.Certificates {
			switch cert.Severity {
			case "warning":
				result.WarningCount++
			case "critical":
				result.CriticalCount++
			case "expired":
				result.ExpiredCount++
			}
		}
	}
	result.Healthy = result.CriticalCount == 0 && result.ExpiredCount == 0 && result.MissingCount == 0
	logf(log, "==> certificate summary: certificates=%d warning=%d critical=%d expired=%d missing=%d", result.CertificateCount, result.WarningCount, result.CriticalCount, result.ExpiredCount, result.MissingCount)
	return result, nil
}

var (
	certificateLine = regexp.MustCompile(`^(\S+)\s+([A-Z][a-z]{2}\s+\d{1,2},\s+\d{4}\s+\d{2}:\d{2}\s+[A-Z]{3,5})\s+(.*)$`)
	missingLine     = regexp.MustCompile(`!MISSING!\s+(\S+)`)
)

func parseCertOutput(output string, now time.Time, opts CertCheckOptions) (CertNodeResult, error) {
	var result CertNodeResult
	certificateAuthority := false
	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if missing := missingLine.FindStringSubmatch(line); len(missing) == 2 {
			result.Missing = append(result.Missing, missing[1])
			continue
		}
		if strings.HasPrefix(line, "CERTIFICATE AUTHORITY") {
			certificateAuthority = true
			continue
		}
		if strings.HasPrefix(line, "CERTIFICATE") {
			certificateAuthority = false
			continue
		}
		match := certificateLine.FindStringSubmatch(line)
		if len(match) != 4 {
			continue
		}
		expires, err := time.Parse("Jan 2, 2006 15:04 MST", match[2])
		if err != nil {
			continue
		}
		expires = expires.UTC()
		fields := strings.Fields(match[3])
		authority := ""
		externallyManaged := false
		if certificateAuthority {
			externallyManaged = len(fields) > 0 && fields[len(fields)-1] == "yes"
		} else {
			if len(fields) > 1 {
				authority = fields[1]
			}
			externallyManaged = len(fields) > 2 && fields[2] == "yes"
		}
		days := int(expires.Sub(now).Hours() / 24)
		severity := "ok"
		switch {
		case days < 0:
			severity = "expired"
		case days <= opts.CriticalDays:
			severity = "critical"
		case days <= opts.WarnDays:
			severity = "warning"
		}
		result.Certificates = append(result.Certificates, CertificateInfo{
			Name:                 match[1],
			ExpiresAt:            &expires,
			DaysRemaining:        days,
			Authority:            authority,
			CertificateAuthority: certificateAuthority,
			ExternallyManaged:    externallyManaged,
			Severity:             severity,
		})
	}
	if len(result.Certificates) == 0 && len(result.Missing) == 0 {
		return result, errors.New("no certificate rows found")
	}
	return result, nil
}

func FormatCertSummary(result CertCheckResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "certificates=%d warning=%d critical=%d expired=%d missing=%d healthy=%s\n",
		result.CertificateCount, result.WarningCount, result.CriticalCount, result.ExpiredCount, result.MissingCount, strconv.FormatBool(result.Healthy))
	for _, node := range result.Nodes {
		for _, cert := range node.Certificates {
			if cert.Severity == "ok" {
				continue
			}
			fmt.Fprintf(&b, "%s %s expires=%s days=%d severity=%s\n", node.Name, cert.Name, cert.ExpiresAt.Format(time.RFC3339), cert.DaysRemaining, cert.Severity)
		}
		for _, name := range node.Missing {
			fmt.Fprintf(&b, "%s %s missing\n", node.Name, name)
		}
	}
	return b.String()
}
