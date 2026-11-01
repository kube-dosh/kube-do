// Package layout defines where Artifacts live in the cache and how they are
// addressed on the Download Server.
package layout

import (
	"net/url"
	"path"
	"path/filepath"
	"strings"

	"github.com/kubedo/kubedo/internal/manifest"
)

const artifactsPrefix = "artifacts"

// ArtifactDir returns the directory holding a Build's Artifacts for one platform.
func ArtifactDir(cacheDir, build, os, arch string) string {
	return filepath.Join(cacheDir, artifactsPrefix, build, os+"-"+arch)
}

// BinaryPath returns the cache path of a binary Artifact.
func BinaryPath(cacheDir, build string, b manifest.Binary) string {
	return filepath.Join(ArtifactDir(cacheDir, build, b.OS, b.Arch), b.Name)
}

// BinaryURL returns the Download Server URL of a binary Artifact.
func BinaryURL(baseURL, build string, b manifest.Binary) string {
	return strings.TrimRight(baseURL, "/") + "/" + path.Join(artifactsPrefix, build, b.OS+"-"+b.Arch, url.PathEscape(b.Name))
}

// BinaryChecksumPath returns the sidecar path for a built Artifact checksum.
func BinaryChecksumPath(cacheDir, build string, b manifest.Binary) string {
	return BinaryPath(cacheDir, build, b) + ".sha256"
}

// BinaryChecksumURL returns the Download Server URL of a built Artifact checksum.
func BinaryChecksumURL(baseURL, build string, b manifest.Binary) string {
	return BinaryURL(baseURL, build, b) + ".sha256"
}
