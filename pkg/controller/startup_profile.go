package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	sociapi "github.com/cloudpilot-ai/hermes/pkg/common/soci"
)

const (
	buildProfileOpenSearchJVM         = "opensearch-jvm-v2"
	openSearchPrefetchMaxSpans        = 240
	openSearchPrefetchMaxSpansPerFile = 64
	openSearchPrefetchArchiveEdges    = 8
	openSearchMinLayerSize            = 0
)

var openSearchPrefetchPaths = []string{
	"usr/share/opensearch/opensearch-docker-entrypoint.sh",
	"usr/share/opensearch/opensearch-onetime-setup.sh",
	"usr/share/opensearch/config/",
	"usr/share/opensearch/bin/",
	"etc/ld.so.cache",
	"usr/bin/ld.so",
	"usr/lib/ld-linux*",
	"lib64/ld-linux*",
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
	"lib/x86_64-linux-gnu/libgcc_s.so*",
	"lib/x86_64-linux-gnu/libstdc++.so*",
	"lib/x86_64-linux-gnu/libnss_*.so*",
	"lib/x86_64-linux-gnu/libresolv.so*",
	"usr/lib/x86_64-linux-gnu/libc.so*",
	"usr/lib/x86_64-linux-gnu/libm.so*",
	"usr/lib/x86_64-linux-gnu/libpthread.so*",
	"usr/lib/x86_64-linux-gnu/libdl.so*",
	"usr/lib/x86_64-linux-gnu/libz.so*",
	"usr/lib/x86_64-linux-gnu/libgcc_s.so*",
	"usr/lib/x86_64-linux-gnu/libstdc++.so*",
	"usr/lib/x86_64-linux-gnu/libnss_*.so*",
	"usr/lib/x86_64-linux-gnu/libresolv.so*",
	"usr/share/opensearch/jdk/bin/java",
	"usr/share/opensearch/jdk/release",
	"usr/share/opensearch/jdk/conf/",
	"usr/share/opensearch/jdk/lib/jvm.cfg",
	"usr/share/opensearch/jdk/lib/classlist",
	"usr/share/opensearch/jdk/lib/security/",
	"usr/share/opensearch/jdk/lib/tzdb.dat",
	"usr/share/opensearch/jdk/lib/*.so",
	"usr/share/opensearch/jdk/lib/jli/*",
	"usr/share/opensearch/jdk/lib/server/*",
	"usr/share/opensearch/jdk/lib/modules",
	"usr/share/opensearch/lib/*.jar",
	"usr/share/opensearch/modules/*/plugin-descriptor.properties",
	"usr/share/opensearch/modules/*/plugin-security.policy",
	"usr/share/opensearch/modules/*/*.jar",
	"usr/share/opensearch/modules/*/*/*.jar",
	"usr/share/opensearch/plugins/*/plugin-descriptor.properties",
	"usr/share/opensearch/plugins/*/plugin-security.policy",
	"usr/share/opensearch/plugins/*/*.jar",
	"usr/share/opensearch/plugins/*/*/*.jar",
}

func buildAccelerationForImageRef(image string) BuildAcceleration {
	if strings.Contains(strings.ToLower(image), "opensearchproject/opensearch") {
		return BuildAcceleration{Profile: buildProfileOpenSearchJVM}
	}
	return BuildAcceleration{}
}

func (a BuildAcceleration) enabled() bool {
	return a.Profile == buildProfileOpenSearchJVM
}

func (a BuildAcceleration) Key() string {
	if !a.enabled() {
		return ""
	}
	normalized := struct {
		Profile                 string   `json:"profile"`
		PrefetchPaths           []string `json:"prefetchPaths"`
		PrefetchMaxSpans        int      `json:"prefetchMaxSpans"`
		PrefetchMaxSpansPerFile int      `json:"prefetchMaxSpansPerFile"`
		PrefetchArchiveEdges    int      `json:"prefetchArchiveEdges"`
		SkipFileVerification    bool     `json:"skipFileVerification"`
		MinLayerSize            int64    `json:"minLayerSize"`
	}{
		Profile:                 a.Profile,
		PrefetchPaths:           a.PrefetchPaths(),
		PrefetchMaxSpans:        a.PrefetchMaxSpans(),
		PrefetchMaxSpansPerFile: a.prefetchMaxSpansPerFile(),
		PrefetchArchiveEdges:    a.prefetchArchiveEdges(),
		SkipFileVerification:    a.SkipFileVerification(),
		MinLayerSize:            a.MinLayerSize(0),
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
	if a.enabled() {
		return openSearchPrefetchPaths
	}
	return nil
}

func (a BuildAcceleration) PrefetchMaxSpans() int {
	if a.enabled() {
		return openSearchPrefetchMaxSpans
	}
	return 0
}

func (a BuildAcceleration) SkipFileVerification() bool {
	return a.enabled()
}

func (a BuildAcceleration) MinLayerSize(base int64) int64 {
	if a.enabled() {
		return openSearchMinLayerSize
	}
	return base
}

func (a BuildAcceleration) prefetchMaxSpansPerFile() int {
	if a.enabled() {
		return openSearchPrefetchMaxSpansPerFile
	}
	return 0
}

func (a BuildAcceleration) prefetchArchiveEdges() int {
	if a.enabled() {
		return openSearchPrefetchArchiveEdges
	}
	return 0
}
