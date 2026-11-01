package manifest

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var (
	sha256Re     = regexp.MustCompile(`^[a-f0-9]{64}$`)
	versionRe    = regexp.MustCompile(`^v\d+\.\d+\.\d+([.-][0-9A-Za-z.-]+)?$`)
	archRe       = regexp.MustCompile(`^[a-z0-9]+$`)
	buildNameRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	namespaceRe  = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)
	binaryNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	binaryVerRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+~-]*$`)
	osRe         = regexp.MustCompile(`^[a-z0-9]+$`)
)

// Validate checks the Manifest for internal consistency and pinning rules.
func (m *Manifest) Validate() error {
	var errs []string
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	if m.APIVersion != APIVersion {
		add("apiVersion must be %q, got %q", APIVersion, m.APIVersion)
	}
	if m.Kind != Kind {
		add("kind must be %q, got %q", Kind, m.Kind)
	}
	if strings.TrimSpace(m.Metadata.Name) == "" {
		add("metadata.name is required")
	} else if !buildNameRe.MatchString(m.Metadata.Name) {
		add("metadata.name must be a safe single path segment (letters, digits, '.', '_' or '-'), got %q", m.Metadata.Name)
	}
	if !versionRe.MatchString(m.Metadata.KubernetesVersion) {
		add("metadata.kubernetesVersion must be a pinned version like v1.36.5, got %q", m.Metadata.KubernetesVersion)
	}
	if m.Spec.Platform != nil && len(m.Spec.Platforms) > 0 {
		add("spec.platform and spec.platforms are mutually exclusive")
	}
	platforms := m.SupportedPlatforms()
	if len(platforms) == 0 {
		add("spec.platforms must not be empty")
	}
	platformSet := map[string]bool{}
	for i, p := range platforms {
		prefix := fmt.Sprintf("spec.platforms[%d]", i)
		if !osRe.MatchString(p.OS) {
			add("%s.os must be lowercase alphanumeric, got %q", prefix, p.OS)
		}
		if !archRe.MatchString(p.Arch) {
			add("%s.arch must be lowercase alphanumeric, got %q", prefix, p.Arch)
		}
		key := p.OS + "/" + p.Arch
		if platformSet[key] {
			add("%s duplicates platform %s", prefix, key)
		}
		platformSet[key] = true
	}
	if strings.TrimSpace(m.Spec.Registry.Namespace) == "" {
		add("spec.registry.namespace is required")
	} else if !namespaceRe.MatchString(m.Spec.Registry.Namespace) {
		add("spec.registry.namespace must be a lowercase repository namespace, got %q", m.Spec.Registry.Namespace)
	}
	if len(m.Spec.Binaries) == 0 {
		add("spec.binaries must not be empty")
	}
	if len(m.Spec.Images) == 0 {
		add("spec.images must not be empty")
	}

	seenBinaries := map[string]bool{}
	binariesByPlatform := map[string]map[string]bool{}
	installerBinaries := map[string]map[string]Binary{}
	type binaryVersion struct {
		version  string
		platform string
	}
	versionsByBinary := map[string]binaryVersion{}
	checkBinaries := func(where string, bins []Binary) {
		for i, b := range bins {
			prefix := fmt.Sprintf("%s[%d] (%s)", where, i, b.Name)
			if !binaryNameRe.MatchString(b.Name) {
				add("%s: name must be a safe file name (letters, digits, '.', '_' or '-'), got %q", prefix, b.Name)
			}
			if !binaryVerRe.MatchString(b.Version) {
				add("%s: version must contain only letters, digits, '.', '_', '+', '~' or '-', got %q", prefix, b.Version)
			}
			platformKey := b.OS + "/" + b.Arch
			if !platformSet[platformKey] {
				add("%s: platform %s is not declared in spec.platforms", prefix, platformKey)
			}
			if binariesByPlatform[platformKey] == nil {
				binariesByPlatform[platformKey] = map[string]bool{}
			}
			binariesByPlatform[platformKey][where+":"+b.Name] = true
			if where == "spec.installer.binaries" {
				if installerBinaries[platformKey] == nil {
					installerBinaries[platformKey] = map[string]Binary{}
				}
				installerBinaries[platformKey][b.Name] = b
			}
			versionKey := where + ":" + b.Name
			if previous, ok := versionsByBinary[versionKey]; ok && previous.version != b.Version {
				add("%s: version %q on %s differs from %q on %s; a multi-platform Build must pin one version per Artifact", prefix, b.Version, platformKey, previous.version, previous.platform)
			} else if !ok {
				versionsByBinary[versionKey] = binaryVersion{version: b.Version, platform: platformKey}
			}
			u, err := url.Parse(b.URL)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				add("%s: url must be absolute http(s)", prefix)
			}
			if b.Archive != ArchiveNone && b.Archive != ArchiveTarGz {
				add("%s: archive must be empty or %q", prefix, ArchiveTarGz)
			}
			if b.IsBuilt() {
				if b.Build != BuildHAProxy && b.Build != BuildKeepalived {
					add("%s: build must be %q or %q", prefix, BuildHAProxy, BuildKeepalived)
				}
				if b.Archive != ArchiveTarGz {
					add("%s: built binaries require archive %q", prefix, ArchiveTarGz)
				}
				if !sha256Re.MatchString(b.SourceSHA256) {
					add("%s: sourceSha256 must be 64 lowercase hex characters", prefix)
				}
				if b.SHA256 != "" {
					add("%s: sha256 must be empty for a built binary", prefix)
				}
			} else {
				if !sha256Re.MatchString(b.SHA256) {
					add("%s: sha256 must be 64 lowercase hex characters", prefix)
				}
				if b.SourceSHA256 != "" {
					add("%s: sourceSha256 is only valid with a build recipe", prefix)
				}
			}
			key := b.Name + "/" + b.OS + "/" + b.Arch
			if seenBinaries[key] {
				add("%s: duplicate binary name for this platform", prefix)
			}
			seenBinaries[key] = true
		}
	}
	checkBinaries("spec.binaries", m.Spec.Binaries)
	checkBinaries("spec.installer.binaries", m.Spec.Installer.Binaries)

	if len(platforms) > 1 && len(binariesByPlatform) > 0 {
		var reference map[string]bool
		referencePlatform := ""
		for _, p := range platforms {
			key := p.OS + "/" + p.Arch
			names := binariesByPlatform[key]
			if reference == nil {
				reference = names
				referencePlatform = key
				continue
			}
			if !sameBinaryNames(reference, names) {
				add("platform %s does not provide the same binary set as %s", key, referencePlatform)
			}
		}
	}

	seenImages := map[string]bool{}
	for i, img := range m.Spec.Images {
		prefix := fmt.Sprintf("spec.images[%d] (%s)", i, img.Name)
		if strings.TrimSpace(img.Name) == "" {
			add("%s: name is required", prefix)
		}
		if seenImages[img.Name] {
			add("%s: duplicate image name", prefix)
		}
		seenImages[img.Name] = true
		if err := validateImageRef(img.Source, true); err != nil {
			add("%s: source: %v", prefix, err)
		}
		if err := validateImageRef(img.Target, true); err != nil {
			add("%s: target: %v", prefix, err)
		}
		if strings.HasPrefix(img.Target, "/") || strings.Contains(img.Target, "://") {
			add("%s: target must be relative to the registry namespace", prefix)
		}
	}

	seenInstallerImages := map[string]bool{}
	installerImagesByPlatform := map[string]map[string]bool{}
	installerTargets := map[string]string{}
	type installerImageVersion struct {
		version  string
		platform string
	}
	installerImageVersions := map[string]installerImageVersion{}
	for i, img := range m.Spec.Installer.Images {
		prefix := fmt.Sprintf("spec.installer.images[%d] (%s)", i, img.Name)
		platformKey := img.OS + "/" + img.Arch
		if !binaryNameRe.MatchString(img.Name) {
			add("%s: name must be a safe image name (letters, digits, '.', '_' or '-'), got %q", prefix, img.Name)
		}
		if !binaryVerRe.MatchString(img.Version) {
			add("%s: version must contain only letters, digits, '.', '_', '+', '~' or '-', got %q", prefix, img.Version)
		}
		if !platformSet[platformKey] {
			add("%s: platform %s is not declared in spec.platforms", prefix, platformKey)
		}
		if err := validateImageRef(img.Target, true); err != nil {
			add("%s: target: %v", prefix, err)
		} else if tag := imageTag(img.Target); tag != img.Version && !strings.HasPrefix(tag, img.Version+"-") {
			add("%s: target tag %q must equal version %q or start with %q", prefix, tag, img.Version, img.Version+"-")
		}
		if strings.HasPrefix(img.Target, "/") || strings.Contains(img.Target, "://") {
			add("%s: target must be relative to the registry namespace", prefix)
		}
		if target, ok := installerTargets[img.Target]; ok {
			add("%s: target %q is already used by %s", prefix, img.Target, target)
		} else {
			installerTargets[img.Target] = prefix
		}
		if !binaryNameRe.MatchString(img.Binary) {
			add("%s: binary must be a safe binary name, got %q", prefix, img.Binary)
		} else if binary, ok := installerBinaries[platformKey][img.Binary]; !ok {
			add("%s: binary %q is not an Installer binary for %s", prefix, img.Binary, platformKey)
		} else if img.Version != binary.Version {
			add("%s: version %q differs from Installer binary %q version %q", prefix, img.Version, img.Binary, binary.Version)
		}
		key := img.Name + "/" + platformKey
		if seenInstallerImages[key] {
			add("%s: duplicate Installer image name for this platform", prefix)
		}
		seenInstallerImages[key] = true
		if installerImagesByPlatform[platformKey] == nil {
			installerImagesByPlatform[platformKey] = map[string]bool{}
		}
		installerImagesByPlatform[platformKey][img.Name] = true
		if previous, ok := installerImageVersions[img.Name]; ok && previous.version != img.Version {
			add("%s: version %q on %s differs from %q on %s; a multi-platform Build must pin one version per Artifact", prefix, img.Version, platformKey, previous.version, previous.platform)
		} else if !ok {
			installerImageVersions[img.Name] = installerImageVersion{version: img.Version, platform: platformKey}
		}
	}

	if len(platforms) > 1 && len(installerImagesByPlatform) > 0 {
		var reference map[string]bool
		referencePlatform := ""
		for _, p := range platforms {
			key := p.OS + "/" + p.Arch
			names := installerImagesByPlatform[key]
			if reference == nil {
				reference = names
				referencePlatform = key
				continue
			}
			if !sameBinaryNames(reference, names) {
				add("platform %s does not provide the same Installer image set as %s", key, referencePlatform)
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid resource manifest: %s", strings.Join(errs, "; "))
	}
	return nil
}

func imageTag(ref string) string {
	i := strings.LastIndex(ref, ":")
	j := strings.LastIndex(ref, "/")
	if i <= j || i == len(ref)-1 {
		return ""
	}
	return ref[i+1:]
}

func sameBinaryNames(left, right map[string]bool) bool {
	if len(left) != len(right) {
		return false
	}
	for name := range left {
		if !right[name] {
			return false
		}
	}
	return true
}

func validateImageRef(ref string, requireTag bool) error {
	if strings.TrimSpace(ref) == "" {
		return fmt.Errorf("is empty")
	}
	if strings.ContainsAny(ref, " \t\r\n") {
		return fmt.Errorf("contains whitespace")
	}
	if strings.Contains(ref, "://") {
		return fmt.Errorf("must not contain a scheme")
	}
	if strings.Contains(ref, `\`) || strings.Contains(ref, "..") || strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") {
		return fmt.Errorf("contains an unsafe repository path")
	}
	if !requireTag {
		return nil
	}
	i := strings.LastIndex(ref, ":")
	j := strings.LastIndex(ref, "/")
	if i <= j || i == len(ref)-1 {
		return fmt.Errorf("must be pinned with a tag")
	}
	return nil
}
