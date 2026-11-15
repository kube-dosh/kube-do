// Package ops implements safe operational actions against an installed cluster.
package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kube-dosh/kube-do/internal/cluster"
	"github.com/kube-dosh/kube-do/internal/manifest"
	"github.com/kube-dosh/kube-do/internal/render"
)

const defaultTimeout = 30 * time.Minute

func prepareDirs(ansibleDir, workDir string) (string, string, error) {
	ansibleDir, err := filepath.Abs(ansibleDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve ansible directory: %w", err)
	}
	workDir, err = filepath.Abs(workDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve working directory: %w", err)
	}
	return ansibleDir, workDir, nil
}

func renderForOps(cfg *cluster.Config, m *manifest.Manifest, workDir string, log io.Writer) error {
	files, err := render.Render(cfg, m, workDir)
	if err != nil {
		return err
	}
	for _, file := range files {
		logf(log, "rendered %s", file)
	}
	return nil
}

func contextWithDefaultTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func ensureFreeSpace(dir string, minBytes int64) error {
	if minBytes <= 0 {
		return nil
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return fmt.Errorf("inspect free space in %s: %w", dir, err)
	}
	available := int64(stat.Bavail) * int64(stat.Bsize)
	if available < minBytes {
		return fmt.Errorf("insufficient free space in %s: have %d bytes, require at least %d", dir, available, minBytes)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open snapshot: %w", err)
	}
	defer file.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return "", fmt.Errorf("hash snapshot: %w", err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func uniqueBackupFilename(backupDir, clusterName string, now time.Time) (string, string, error) {
	stamp := now.UTC().Format("20060102T150405Z")
	for suffix := 0; suffix < 100; suffix++ {
		name := fmt.Sprintf("%s-etcd-%s", clusterName, stamp)
		if suffix > 0 {
			name = fmt.Sprintf("%s-%d", name, suffix+1)
		}
		filename := name + ".db"
		path := filepath.Join(backupDir, filename)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return filename, path, nil
		} else if err != nil {
			return "", "", fmt.Errorf("inspect backup path: %w", err)
		}
	}
	return "", "", errors.New("could not allocate a unique backup filename")
}

func logf(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, format+"\n", args...)
}
