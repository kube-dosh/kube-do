// Package manifests exposes the Resource Manifests embedded in the binary.
package manifests

import (
	"embed"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/kubedo/kubedo/internal/manifest"
)

//go:embed builds/*.yaml
var embedded embed.FS

// Names lists the embedded Builds.
func Names() ([]string, error) {
	entries, err := embedded.ReadDir("builds")
	if err != nil {
		return nil, fmt.Errorf("read embedded manifests: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	sortBuildNames(names)
	return names, nil
}

var buildNameRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)-kubedo\.(\d+)$`)

func sortBuildNames(names []string) {
	sort.Slice(names, func(i, j int) bool {
		left, leftOK := parseBuildName(names[i])
		right, rightOK := parseBuildName(names[j])
		if leftOK && rightOK {
			for k := range left {
				if left[k] != right[k] {
					return left[k] < right[k]
				}
			}
			return names[i] < names[j]
		}
		if leftOK != rightOK {
			return leftOK
		}
		return names[i] < names[j]
	})
}

func parseBuildName(name string) ([4]int, bool) {
	var version [4]int
	match := buildNameRe.FindStringSubmatch(name)
	if match == nil {
		return version, false
	}
	for i := range version {
		n, err := strconv.Atoi(match[i+1])
		if err != nil {
			return version, false
		}
		version[i] = n
	}
	return version, true
}

// Get loads and validates one embedded Build.
func Get(build string) (*manifest.Manifest, error) {
	if build == "" {
		return nil, fmt.Errorf("build is empty")
	}
	if strings.ContainsAny(build, `/\`) || build == "." || build == ".." {
		return nil, fmt.Errorf("invalid build name %q", build)
	}
	data, err := embedded.ReadFile(path.Join("builds", build+".yaml"))
	if err != nil {
		names, _ := Names()
		return nil, fmt.Errorf("unknown build %q (embedded builds: %s)", build, strings.Join(names, ", "))
	}
	return manifest.Parse(data)
}
