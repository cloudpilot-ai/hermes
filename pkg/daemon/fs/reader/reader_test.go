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
		{name: "app lib jar", path: "opt/acme/lib/acme.jar", want: true},
		{name: "module jar", path: "/opt/acme/modules/transport/netty-common.jar", want: true},
		{name: "plugin jar", path: "opt/acme/plugins/security/security.jar", want: true},
		{name: "runtime modules", path: "opt/acme/runtime/lib/modules", want: true},
		{name: "shared object", path: "opt/acme/runtime/lib/libvm.so", want: false},
		{name: "generic lib jar", path: "usr/local/lib/helper.jar", want: true},
		{name: "config file", path: "opt/acme/config/app.yml", want: false},
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
			"opt/acme/lib/app.jar": localPath,
		},
	}
	got, ok := gr.materializedPath("opt/acme/lib/app.jar")
	if !ok {
		t.Fatalf("materialized path was not found")
	}
	if got != localPath {
		t.Fatalf("materialized path = %q, want %q", got, localPath)
	}
	if _, ok := gr.materializedPath("/../opt/acme/lib/app.jar"); ok {
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
		r: fakeMetadataReader{file: fakeMetadataFile{name: "opt/acme/lib/app.jar"}},
		materializedFiles: map[string]string{
			"opt/acme/lib/app.jar": localPath,
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
