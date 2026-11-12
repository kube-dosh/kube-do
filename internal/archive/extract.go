// Package archive provides safe extraction helpers.
package archive

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ExtractTarGz extracts src into destDir. Paths and links that resolve outside
// destDir are rejected.
func ExtractTarGz(src, destDir string) ([]string, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("open gzip stream: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	root, err := filepath.Abs(destDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}

	var written []string
	seen := map[string]bool{}
	type directoryTime struct {
		path string
		mod  time.Time
	}
	var directories []directoryTime
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name := filepath.Clean(hdr.Name)
		if name == "." {
			continue
		}
		dest, err := secureJoin(root, name)
		if err != nil {
			return nil, fmt.Errorf("unsafe archive path %q", hdr.Name)
		}
		if hdr.Typeflag != tar.TypeDir {
			if seen[dest] {
				return nil, fmt.Errorf("duplicate archive path %q", hdr.Name)
			}
			seen[dest] = true
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return nil, err
			}
			if err := ensureWithinRoot(rootReal, dest); err != nil {
				return nil, fmt.Errorf("unsafe archive directory %q: %w", hdr.Name, err)
			}
			if !hdr.ModTime.IsZero() {
				directories = append(directories, directoryTime{path: dest, mod: hdr.ModTime})
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return nil, err
			}
			if err := ensureWithinRoot(rootReal, filepath.Dir(dest)); err != nil {
				return nil, fmt.Errorf("unsafe archive path %q: %w", hdr.Name, err)
			}
			if info, err := os.Lstat(dest); err == nil && info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("refusing to overwrite symlink %q", hdr.Name)
			}
			out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return nil, err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return nil, err
			}
			if err := out.Close(); err != nil {
				return nil, err
			}
			if !hdr.ModTime.IsZero() {
				if err := os.Chtimes(dest, hdr.ModTime, hdr.ModTime); err != nil {
					return nil, err
				}
			}
			written = append(written, dest)
		case tar.TypeSymlink:
			if filepath.IsAbs(hdr.Linkname) {
				return nil, fmt.Errorf("unsafe archive symlink %q -> %q", hdr.Name, hdr.Linkname)
			}
			target, err := secureJoin(root, filepath.Join(filepath.Dir(name), hdr.Linkname))
			if err != nil {
				return nil, fmt.Errorf("unsafe archive symlink %q -> %q", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return nil, err
			}
			if err := ensureWithinRoot(rootReal, filepath.Dir(dest)); err != nil {
				return nil, fmt.Errorf("unsafe archive symlink %q: %w", hdr.Name, err)
			}
			if _, err := os.Lstat(target); err == nil {
				if err := ensureWithinRoot(rootReal, target); err != nil {
					return nil, fmt.Errorf("unsafe archive symlink %q -> %q: %w", hdr.Name, hdr.Linkname, err)
				}
			}
			if err := os.Symlink(hdr.Linkname, dest); err != nil {
				return nil, err
			}
		case tar.TypeLink:
			target, err := secureJoin(root, hdr.Linkname)
			if err != nil {
				return nil, fmt.Errorf("unsafe archive hardlink %q -> %q", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return nil, err
			}
			if err := ensureWithinRoot(rootReal, filepath.Dir(dest)); err != nil {
				return nil, fmt.Errorf("unsafe archive hardlink %q: %w", hdr.Name, err)
			}
			info, err := os.Lstat(target)
			if err != nil {
				return nil, fmt.Errorf("hardlink target %q: %w", hdr.Linkname, err)
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("hardlink target %q is not a regular file", hdr.Linkname)
			}
			if err := ensureWithinRoot(rootReal, target); err != nil {
				return nil, fmt.Errorf("unsafe archive hardlink %q -> %q: %w", hdr.Name, hdr.Linkname, err)
			}
			if err := os.Link(target, dest); err != nil {
				return nil, err
			}
		}
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err := os.Chtimes(directories[i].path, directories[i].mod, directories[i].mod); err != nil {
			return nil, err
		}
	}
	return written, nil
}

func secureJoin(root, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) {
		return "", fmt.Errorf("unsafe path")
	}
	dest := filepath.Join(root, filepath.Clean(name))
	rel, err := filepath.Rel(root, dest)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes root")
	}
	return dest, nil
}

func ensureWithinRoot(rootReal, path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootReal, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path resolves outside extraction root")
	}
	return nil
}
