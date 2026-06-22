package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"sort"
	"strconv"
	"strings"

	sociapi "github.com/cloudpilot-ai/hermes/pkg/common/soci"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	buildProfileStartupLocal         = "startup-local-v1"
	startupLocalPrefetchMaxSpans     = 240
	startupLocalMaxSpansPerFile      = 64
	startupLocalPrefetchArchiveEdges = 8
	startupLocalMinLayerSize         = 0
	defaultContainerPATH             = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

var startupLocalBasePrefetchPaths = []string{
	"etc/ld.so.cache",
	"etc/nsswitch.conf",
	"etc/passwd",
	"etc/group",
	"etc/hosts",
	"etc/resolv.conf",
	"etc/localtime",
	"etc/ssl/certs/",
	"etc/pki/",
	"usr/share/ca-certificates/",
	"usr/share/zoneinfo/",
	"usr/bin/env",
	"bin/sh",
	"bin/bash",
	"lib/ld-linux*",
	"lib64/ld-linux*",
	"usr/lib/ld-linux*",
	"lib64/libc.so*",
	"lib64/libm.so*",
	"lib64/libpthread.so*",
	"lib64/libdl.so*",
	"lib64/libz.so*",
	"lib64/libzstd.so*",
	"lib64/libgcc_s.so*",
	"lib64/libstdc++.so*",
	"lib64/libnss_*.so*",
	"lib64/libresolv.so*",
	"usr/lib64/libc.so*",
	"usr/lib64/libm.so*",
	"usr/lib64/libpthread.so*",
	"usr/lib64/libdl.so*",
	"usr/lib64/libz.so*",
	"usr/lib64/libzstd.so*",
	"usr/lib64/libgcc_s.so*",
	"usr/lib64/libstdc++.so*",
	"usr/lib64/libnss_*.so*",
	"usr/lib64/libresolv.so*",
	"lib/x86_64-linux-gnu/ld-linux*",
	"lib/x86_64-linux-gnu/libc.so*",
	"lib/x86_64-linux-gnu/libm.so*",
	"lib/x86_64-linux-gnu/libpthread.so*",
	"lib/x86_64-linux-gnu/libdl.so*",
	"lib/x86_64-linux-gnu/libz.so*",
	"lib/x86_64-linux-gnu/libzstd.so*",
	"lib/x86_64-linux-gnu/libgcc_s.so*",
	"lib/x86_64-linux-gnu/libstdc++.so*",
	"lib/x86_64-linux-gnu/libnss_*.so*",
	"lib/x86_64-linux-gnu/libresolv.so*",
	"usr/lib/x86_64-linux-gnu/libc.so*",
	"usr/lib/x86_64-linux-gnu/libm.so*",
	"usr/lib/x86_64-linux-gnu/libpthread.so*",
	"usr/lib/x86_64-linux-gnu/libdl.so*",
	"usr/lib/x86_64-linux-gnu/libz.so*",
	"usr/lib/x86_64-linux-gnu/libzstd.so*",
	"usr/lib/x86_64-linux-gnu/libgcc_s.so*",
	"usr/lib/x86_64-linux-gnu/libstdc++.so*",
	"usr/lib/x86_64-linux-gnu/libnss_*.so*",
	"usr/lib/x86_64-linux-gnu/libresolv.so*",
}

var startupLocalAppRootPrefetchSuffixes = []string{
	"bin/",
	"sbin/",
	"config/",
	"conf/",
	"etc/",
	"lib/*.so*",
	"lib/*.jar",
	"lib/*.zip",
	"lib/*.jmod",
	"lib/*.jimage",
	"lib/*.jsa",
	"lib/modules",
	"modules/*/*.so*",
	"modules/*/*.jar",
	"modules/*/*.zip",
	"modules/*/*/*.so*",
	"modules/*/*/*.jar",
	"plugins/*/*.so*",
	"plugins/*/*.jar",
	"plugins/*/*.zip",
	"plugins/*/*/*.so*",
	"plugins/*/*/*.jar",
	"extensions/*/*.so*",
	"extensions/*/*.jar",
	"*/bin/*",
	"*/lib/*.so*",
	"*/lib/modules",
	"*/lib/security/",
	"*/lib/tzdb.dat",
}

func buildAccelerationForImageRef(image string) BuildAcceleration {
	return BuildAcceleration{
		Profile:              buildProfileStartupLocal,
		PrefetchPathPatterns: genericStartupPrefetchPaths(nil),
	}
}

func (a BuildAcceleration) WithImageConfig(config ocispec.ImageConfig) BuildAcceleration {
	if !a.enabled() {
		return a
	}
	out := a
	out.PrefetchPathPatterns = genericStartupPrefetchPaths(&config)
	return out
}

func (a BuildAcceleration) enabled() bool {
	return a.Profile == buildProfileStartupLocal
}

func (a BuildAcceleration) Key() string {
	if !a.enabled() {
		return ""
	}
	normalized := struct {
		Profile              string `json:"profile"`
		PrefetchMaxSpans     int    `json:"prefetchMaxSpans"`
		MaxSpansPerFile      int    `json:"maxSpansPerFile"`
		PrefetchArchiveEdges int    `json:"prefetchArchiveEdges"`
		SkipFileVerification bool   `json:"skipFileVerification"`
		MinLayerSize         int64  `json:"minLayerSize"`
	}{
		Profile:              a.Profile,
		PrefetchMaxSpans:     a.PrefetchMaxSpans(),
		MaxSpansPerFile:      a.prefetchMaxSpansPerFile(),
		PrefetchArchiveEdges: a.prefetchArchiveEdges(),
		SkipFileVerification: a.SkipFileVerification(),
		MinLayerSize:         a.MinLayerSize(0),
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return normalized.Profile
	}
	sum := sha256.Sum256(payload)
	return normalized.Profile + ":" + hex.EncodeToString(sum[:8])
}

func (a BuildAcceleration) IndexAnnotations() map[string]string {
	if !a.enabled() {
		return nil
	}
	return map[string]string{
		sociapi.IndexAnnotationHermesBackgroundFetch:      sociapi.IndexAnnotationHermesBackgroundFetchDisabled,
		sociapi.IndexAnnotationHermesPrefetchProfile:      a.Profile,
		sociapi.IndexAnnotationHermesSkipFileVerification: strconv.FormatBool(a.SkipFileVerification()),
	}
}

func (a BuildAcceleration) PrefetchProfile() string {
	if a.enabled() {
		return a.Profile
	}
	return ""
}

func (a BuildAcceleration) PrefetchPaths() []string {
	if !a.enabled() {
		return nil
	}
	return append([]string(nil), a.PrefetchPathPatterns...)
}

func (a BuildAcceleration) PrefetchMaxSpans() int {
	if a.enabled() {
		return startupLocalPrefetchMaxSpans
	}
	return 0
}

func (a BuildAcceleration) SkipFileVerification() bool {
	return a.enabled()
}

func (a BuildAcceleration) MinLayerSize(base int64) int64 {
	if a.enabled() {
		return startupLocalMinLayerSize
	}
	return base
}

func (a BuildAcceleration) prefetchMaxSpansPerFile() int {
	if a.enabled() {
		return startupLocalMaxSpansPerFile
	}
	return 0
}

func (a BuildAcceleration) prefetchArchiveEdges() int {
	if a.enabled() {
		return startupLocalPrefetchArchiveEdges
	}
	return 0
}

func genericStartupPrefetchPaths(config *ocispec.ImageConfig) []string {
	paths := newStringSetBuilder()
	paths.add(startupLocalBasePrefetchPaths...)

	if config != nil {
		for _, cmd := range startupCommandCandidates(*config) {
			paths.add(commandPrefetchPaths(cmd, *config)...)
		}
		if wd := cleanContainerPath(config.WorkingDir); wd != "" && wd != "." {
			paths.add(appRootPrefetchPaths(wd)...)
		}
	}

	out := paths.items()
	sort.Strings(out)
	return out
}

func startupCommandCandidates(config ocispec.ImageConfig) []string {
	var out []string
	if len(config.Entrypoint) > 0 {
		out = append(out, config.Entrypoint[0])
		if len(config.Entrypoint) >= 3 && isShellName(config.Entrypoint[0]) && config.Entrypoint[1] == "-c" {
			out = append(out, firstShellToken(config.Entrypoint[2]))
		}
		return compactStrings(out)
	}
	if len(config.Cmd) > 0 {
		out = append(out, config.Cmd[0])
		if len(config.Cmd) >= 3 && isShellName(config.Cmd[0]) && config.Cmd[1] == "-c" {
			out = append(out, firstShellToken(config.Cmd[2]))
		}
	}
	return compactStrings(out)
}

func commandPrefetchPaths(command string, config ocispec.ImageConfig) []string {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}
	var out []string
	if strings.Contains(command, "/") {
		cleaned := cleanContainerPath(command)
		if cleaned == "" {
			return nil
		}
		out = append(out, cleaned)
		for _, root := range inferAppRoots(cleaned) {
			out = append(out, appRootPrefetchPaths(root)...)
		}
		return out
	}
	for _, dir := range pathSearchDirs(config.Env) {
		if cleaned := cleanContainerPath(path.Join(dir, command)); cleaned != "" {
			out = append(out, cleaned)
		}
	}
	return out
}

func inferAppRoots(commandPath string) []string {
	commandPath = cleanContainerPath(commandPath)
	if commandPath == "" {
		return nil
	}
	dir := path.Dir(commandPath)
	parts := strings.Split(dir, "/")
	var roots []string
	switch {
	case len(parts) >= 3 && parts[0] == "usr" && parts[1] == "share":
		roots = append(roots, path.Join(parts[:3]...))
	case len(parts) >= 2 && parts[0] == "opt":
		roots = append(roots, path.Join(parts[:2]...))
	case len(parts) >= 3 && parts[0] == "usr" && parts[1] == "local":
		roots = append(roots, path.Join(parts[:3]...))
	case len(parts) >= 1 && (parts[0] == "app" || parts[0] == "workspace"):
		roots = append(roots, parts[0])
	}
	roots = append(roots, dir)
	return uniqueStrings(roots)
}

func appRootPrefetchPaths(root string) []string {
	root = cleanContainerPath(root)
	if root == "" || root == "." {
		return nil
	}
	out := make([]string, 0, len(startupLocalAppRootPrefetchSuffixes))
	for _, suffix := range startupLocalAppRootPrefetchSuffixes {
		out = append(out, path.Join(root, suffix))
		if strings.HasSuffix(suffix, "/") {
			out[len(out)-1] += "/"
		}
	}
	return out
}

func pathSearchDirs(env []string) []string {
	value := defaultContainerPATH
	for _, item := range env {
		if strings.HasPrefix(item, "PATH=") {
			value = strings.TrimPrefix(item, "PATH=")
			break
		}
	}
	var out []string
	for _, dir := range strings.Split(value, ":") {
		if cleaned := cleanContainerPath(dir); cleaned != "" {
			out = append(out, cleaned)
		}
	}
	return uniqueStrings(out)
}

func cleanContainerPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.TrimLeft(value, "/")
	cleaned := path.Clean(value)
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return ""
	}
	return cleaned
}

func cleanPrefetchPattern(value string) string {
	value = strings.TrimSpace(value)
	trailingSlash := strings.HasSuffix(value, "/")
	cleaned := cleanContainerPath(value)
	if cleaned == "" {
		return ""
	}
	if trailingSlash && !strings.HasSuffix(cleaned, "/") {
		cleaned += "/"
	}
	return cleaned
}

func firstShellToken(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func isShellName(name string) bool {
	base := path.Base(strings.TrimSpace(name))
	return base == "sh" || base == "bash" || base == "dash" || base == "ash"
}

func compactStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

type stringSetBuilder struct {
	seen map[string]struct{}
	out  []string
}

func newStringSetBuilder() *stringSetBuilder {
	return &stringSetBuilder{seen: map[string]struct{}{}}
}

func (b *stringSetBuilder) add(values ...string) {
	for _, value := range values {
		value = cleanPrefetchPattern(value)
		if value == "" {
			continue
		}
		if _, ok := b.seen[value]; ok {
			continue
		}
		b.seen[value] = struct{}{}
		b.out = append(b.out, value)
	}
}

func (b *stringSetBuilder) items() []string {
	return append([]string(nil), b.out...)
}
