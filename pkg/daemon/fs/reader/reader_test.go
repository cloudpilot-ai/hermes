package reader

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudpilot-ai/hermes/pkg/common/soci/ztoc/compression"
	"github.com/cloudpilot-ai/hermes/pkg/daemon/metadata"
)

func TestIsStartupReadaheadPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "opensearch core jar", path: "usr/share/opensearch/lib/opensearch-2.19.1.jar", want: true},
		{name: "module jar", path: "/usr/share/opensearch/modules/transport-netty4/netty-common.jar", want: true},
		{name: "plugin jar", path: "usr/share/opensearch/plugins/opensearch-security/opensearch-security.jar", want: true},
		{name: "jdk modules", path: "usr/share/opensearch/jdk/lib/modules", want: true},
		{name: "jdk shared object", path: "usr/share/opensearch/jdk/lib/libjava.so", want: false},
		{name: "non opensearch jar", path: "usr/local/lib/helper.jar", want: false},
		{name: "config file", path: "usr/share/opensearch/config/opensearch.yml", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsStartupHotPath(tt.path); got != tt.want {
				t.Fatalf("IsStartupHotPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestReaderMaterializedPathRequiresCleanRelativeTarName(t *testing.T) {
	dir := t.TempDir()
	localPath := filepath.Join(dir, "materialized")
	if err := os.WriteFile(localPath, []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	gr := &reader{
		materializedFiles: map[string]string{
			"usr/share/opensearch/lib/opensearch.jar": localPath,
		},
	}
	got, ok := gr.materializedPath("usr/share/opensearch/lib/opensearch.jar")
	if !ok {
		t.Fatalf("materialized path was not found")
	}
	if got != localPath {
		t.Fatalf("materialized path = %q, want %q", got, localPath)
	}
	if _, ok := gr.materializedPath("/../usr/share/opensearch/lib/opensearch.jar"); ok {
		t.Fatalf("malformed tar name resolved to a materialized file")
	}
}

func TestOpenFileUsesMaterializedOnlyWhenVerificationDisabled(t *testing.T) {
	dir := t.TempDir()
	localPath := filepath.Join(dir, "materialized")
	if err := os.WriteFile(localPath, []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	gr := &reader{
		r: fakeMetadataReader{file: fakeMetadataFile{name: "usr/share/opensearch/lib/opensearch.jar"}},
		materializedFiles: map[string]string{
			"usr/share/opensearch/lib/opensearch.jar": localPath,
		},
	}

	ra, err := gr.OpenFile(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ra.(LocalFileReaderAt); ok {
		t.Fatal("materialized fast path was used with verification enabled")
	}

	gr.disableVerification = true
	ra, err = gr.OpenFile(1)
	if err != nil {
		t.Fatal(err)
	}
	local, ok := ra.(LocalFileReaderAt)
	if !ok {
		t.Fatal("materialized fast path was not used with verification disabled")
	}
	if local.LocalPath() != localPath {
		t.Fatalf("local path = %q, want %q", local.LocalPath(), localPath)
	}
}

type fakeMetadataReader struct {
	metadata.Reader
	file metadata.File
}

func (r fakeMetadataReader) OpenFile(uint32) (metadata.File, error) {
	return r.file, nil
}

type fakeMetadataFile struct {
	name string
}

func (f fakeMetadataFile) GetUncompressedFileSize() compression.Offset { return 0 }
func (f fakeMetadataFile) GetUncompressedOffset() compression.Offset   { return 0 }
func (f fakeMetadataFile) TarName() string                             { return f.name }
func (f fakeMetadataFile) TarHeaderOffset() compression.Offset         { return 0 }
func (f fakeMetadataFile) TarHeaderSize() compression.Offset           { return 0 }
