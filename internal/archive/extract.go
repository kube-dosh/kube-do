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

	var written []string
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
		dest, err := secureJoin(destDir, name)
		if err != nil {
			return nil, fmt.Errorf("unsafe archive path %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return nil, err
			}
			if !hdr.ModTime.IsZero() {
				directories = append(directories, directoryTime{path: dest, mod: hdr.ModTime})
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return nil, err
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
			if _, err := secureJoin(destDir, filepath.Join(filepath.Dir(name), hdr.Linkname)); err != nil {
				return nil, fmt.Errorf("unsafe archive symlink %q -> %q", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return nil, err
			}
			if err := os.Symlink(hdr.Linkname, dest); err != nil {
				return nil, err
			}
		case tar.TypeLink:
			target, err := secureJoin(destDir, hdr.Linkname)
			if err != nil {
				return nil, fmt.Errorf("unsafe archive hardlink %q -> %q", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return nil, err
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
