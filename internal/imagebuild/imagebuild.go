// Package imagebuild assembles self-contained runtime images from Installer
// Host binaries. The images carry the binary, its dynamic libraries, and the
// minimal files glibc needs for local user/group lookup.
package imagebuild

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

const (
	binaryDir = "/usr/local/sbin"
)

var epoch = time.Unix(0, 0).UTC()

// Options describes one generated runtime image.
type Options struct {
	BinaryPath string
	BinaryName string
	Version    string
	OS         string
	Arch       string
	// Libraries overrides dynamic library discovery. Tests use this to make the
	// result independent of the host toolchain.
	Libraries []string
	// DiscoverLibraries defaults to DiscoverLibraries.
	DiscoverLibraries func(binaryPath string) ([]string, error)
}

// Build creates a single-platform OCI image containing BinaryPath and its
// runtime dependencies.
func Build(opts Options) (v1.Image, error) {
	if opts.BinaryPath == "" {
		return nil, errors.New("binary path is required")
	}
	if strings.ContainsAny(opts.BinaryName, `/\`) || opts.BinaryName == "" || opts.BinaryName == "." || opts.BinaryName == ".." {
		return nil, fmt.Errorf("invalid binary name %q", opts.BinaryName)
	}
	if opts.OS == "" || opts.Arch == "" {
		return nil, errors.New("image OS and architecture are required")
	}
	if opts.OS != "linux" {
		return nil, fmt.Errorf("runtime images currently require linux, got %s", opts.OS)
	}

	info, err := os.Stat(opts.BinaryPath)
	if err != nil {
		return nil, fmt.Errorf("stat binary: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("binary %s is not a regular file", opts.BinaryPath)
	}

	libraries := opts.Libraries
	if libraries == nil {
		discover := opts.DiscoverLibraries
		if discover == nil {
			discover = DiscoverLibraries
		}
		libraries, err = discover(opts.BinaryPath)
		if err != nil {
			return nil, err
		}
	}

	layerData, err := buildLayer(opts.BinaryPath, opts.BinaryName, libraries)
	if err != nil {
		return nil, err
	}
	layer, err := tarball.LayerFromReader(bytes.NewReader(layerData))
	if err != nil {
		return nil, fmt.Errorf("create image layer: %w", err)
	}
	image, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return nil, fmt.Errorf("append image layer: %w", err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("read image config: %w", err)
	}
	config.OS = opts.OS
	config.Architecture = opts.Arch
	config.Created = v1.Time{Time: epoch}
	config.Config.Entrypoint = []string{path.Join(binaryDir, opts.BinaryName)}
	config.Config.WorkingDir = "/"
	config.Config.Labels = map[string]string{
		"org.opencontainers.image.title":      "kube-do " + opts.BinaryName + " runtime",
		"org.opencontainers.image.version":    opts.Version,
		"io.kube-do.installer.binary":         opts.BinaryName,
		"io.kube-do.installer.binary.version": opts.Version,
	}
	image, err = mutate.ConfigFile(image, config)
	if err != nil {
		return nil, fmt.Errorf("set image config: %w", err)
	}
	return image, nil
}

// DiscoverLibraries runs ldd against binaryPath and returns every absolute
// dependency path. linux-vdso.so.1 has no filesystem path and is ignored.
func DiscoverLibraries(binaryPath string) ([]string, error) {
	cmd := exec.Command("ldd", binaryPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("run ldd for %s: %w: %s", binaryPath, err, strings.TrimSpace(string(output)))
	}
	libraries, err := parseLdd(string(output))
	if err != nil {
		return nil, fmt.Errorf("inspect dependencies of %s: %w", binaryPath, err)
	}
	libraries = mergeLibraries(libraries,
		"/lib64/libgcc_s.so.1",
		"/usr/lib64/libgcc_s.so.1",
		"/lib/libgcc_s.so.1",
		"/usr/lib/libgcc_s.so.1",
	)
	return libraries, nil
}

// mergeLibraries adds existing runtime libraries that are loaded dynamically
// rather than reported by ldd. glibc's pthread_exit loads libgcc_s.so.1 at
// runtime for unwinding; without it threaded programs abort inside the image.
func mergeLibraries(libraries []string, candidates ...string) []string {
	seen := make(map[string]bool, len(libraries)+len(candidates))
	merged := make([]string, 0, len(libraries)+len(candidates))
	add := func(path string) {
		if seen[path] {
			return
		}
		seen[path] = true
		merged = append(merged, path)
	}
	for _, library := range libraries {
		add(library)
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() {
			add(candidate)
		}
	}
	sort.Strings(merged)
	return merged
}

func parseLdd(output string) ([]string, error) {
	if strings.Contains(output, "not found") {
		return nil, errors.New("ldd reported a missing dependency")
	}
	seen := map[string]bool{}
	var libraries []string
	for _, line := range strings.Split(output, "\n") {
		for _, field := range strings.Fields(line) {
			field = strings.TrimSuffix(field, ":")
			if !filepath.IsAbs(field) || field == "/" {
				continue
			}
			field = filepath.Clean(field)
			if seen[field] {
				continue
			}
			seen[field] = true
			libraries = append(libraries, field)
		}
	}
	sort.Strings(libraries)
	return libraries, nil
}

type layerFile struct {
	name string
	mode int64
	data []byte
}

func buildLayer(binaryPath, binaryName string, libraries []string) ([]byte, error) {
	files := make([]layerFile, 0, len(libraries)+5)
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		return nil, fmt.Errorf("read binary: %w", err)
	}
	files = append(files, layerFile{name: path.Join(strings.TrimPrefix(binaryDir, "/"), binaryName), mode: 0o755, data: binary})
	for _, library := range libraries {
		name, err := layerPath(library)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(library)
		if err != nil {
			return nil, fmt.Errorf("read library %s: %w", library, err)
		}
		files = append(files, layerFile{name: name, mode: 0o755, data: data})
	}
	files = append(files,
		layerFile{name: "etc/passwd", mode: 0o644, data: []byte("root:x:0:0:root:/root:/sbin/nologin\nnobody:x:65534:65534:nobody:/nonexistent:/sbin/nologin\n")},
		layerFile{name: "etc/group", mode: 0o644, data: []byte("root:x:0:\nnobody:x:65534:\n")},
		layerFile{name: "etc/nsswitch.conf", mode: 0o644, data: []byte("passwd: files\ngroup: files\nhosts: files\n")},
	)

	byName := map[string]layerFile{}
	for _, file := range files {
		byName[file.name] = file
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	dirs := map[string]bool{}
	for _, name := range names {
		for dir := path.Dir(name); dir != "." && dir != "/"; dir = path.Dir(dir) {
			dirs[dir] = true
		}
	}
	dirNames := make([]string, 0, len(dirs))
	for dir := range dirs {
		dirNames = append(dirNames, dir)
	}
	sort.Strings(dirNames)

	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, dir := range dirNames {
		if err := writeTarHeader(writer, &tar.Header{
			Name:     dir + "/",
			Typeflag: tar.TypeDir,
			Mode:     0o755,
			ModTime:  epoch,
			Uid:      0,
			Gid:      0,
			Uname:    "root",
			Gname:    "root",
		}); err != nil {
			return nil, err
		}
	}
	for _, name := range names {
		file := byName[name]
		if err := writeTarHeader(writer, &tar.Header{
			Name:     file.name,
			Typeflag: tar.TypeReg,
			Mode:     file.mode,
			Size:     int64(len(file.data)),
			ModTime:  epoch,
			Uid:      0,
			Gid:      0,
			Uname:    "root",
			Gname:    "root",
		}); err != nil {
			return nil, err
		}
		if _, err := writer.Write(file.data); err != nil {
			return nil, fmt.Errorf("write %s to layer: %w", file.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close image layer: %w", err)
	}
	return buffer.Bytes(), nil
}

func layerPath(absolute string) (string, error) {
	cleaned := path.Clean(filepath.ToSlash(absolute))
	if !strings.HasPrefix(cleaned, "/") || cleaned == "/" {
		return "", fmt.Errorf("dependency path must be absolute, got %q", absolute)
	}
	name := strings.TrimPrefix(cleaned, "/")
	if name == "" || filepath.IsAbs(name) || strings.HasPrefix(name, "../") || name == ".." {
		return "", fmt.Errorf("unsafe dependency path %q", absolute)
	}
	return name, nil
}

func writeTarHeader(writer *tar.Writer, header *tar.Header) error {
	if err := writer.WriteHeader(header); err != nil {
		return fmt.Errorf("write tar header %s: %w", header.Name, err)
	}
	return nil
}

// ExtractFile reads one file from an image layer chain. It is exported for
// tests and callers that need to inspect a generated image without a runtime.
func ExtractFile(image v1.Image, name string) ([]byte, error) {
	reader := mutate.Extract(image)
	defer reader.Close()
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("file %s not found in image", name)
		}
		if err != nil {
			return nil, err
		}
		if header.Name == name {
			return io.ReadAll(tarReader)
		}
	}
}
