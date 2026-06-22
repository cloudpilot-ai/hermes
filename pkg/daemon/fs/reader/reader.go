/*
   Copyright The Soci Snapshotter Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

/*
   Copyright 2019 The Go Authors. All rights reserved.
   Use of this source code is governed by a BSD-style
   license that can be found in the NOTICE.md file.
*/

package reader

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudpilot-ai/hermes/pkg/common/cache"
	"github.com/cloudpilot-ai/hermes/pkg/common/soci/ztoc"
	"github.com/cloudpilot-ai/hermes/pkg/common/soci/ztoc/compression"
	"github.com/cloudpilot-ai/hermes/pkg/common/util/ioutils"
	commonmetrics "github.com/cloudpilot-ai/hermes/pkg/daemon/fs/metrics/common"
	spanmanager "github.com/cloudpilot-ai/hermes/pkg/daemon/fs/spanmanager"
	"github.com/cloudpilot-ai/hermes/pkg/daemon/metadata"
	"github.com/containerd/log"
	digest "github.com/opencontainers/go-digest"
	"github.com/sirupsen/logrus"
)

const (
	startupReadaheadMaxSpans    = 32
	startupReadaheadConcurrency = 2
	startupReadaheadWindowSize  = compression.Offset(16 << 20)
)

var startupReadTraceEnabled = os.Getenv("HERMES_TRACE_STARTUP_READS") != ""

type MaterializedFileSet struct {
	Root  string
	Files map[string]string
}

type Reader interface {
	OpenFile(id uint32) (io.ReaderAt, error)
	Metadata() metadata.Reader
	Close() error
	LastOnDemandReadTime() time.Time
}

type LocalFileReaderAt interface {
	io.ReaderAt
	LocalPath() string
}

// NewReader creates a Reader based on the given soci blob and Span Manager.
func NewReader(r metadata.Reader, layerSha digest.Digest, spanManager *spanmanager.SpanManager, disableVerification bool, enableStartupReadahead ...bool) (Reader, error) {
	var startupReadahead bool
	if len(enableStartupReadahead) > 0 {
		startupReadahead = enableStartupReadahead[0]
	}
	return NewReaderWithMaterializedFiles(r, layerSha, spanManager, disableVerification, nil, startupReadahead)
}

func NewReaderWithMaterializedFiles(r metadata.Reader, layerSha digest.Digest, spanManager *spanmanager.SpanManager, disableVerification bool, materialized *MaterializedFileSet, enableStartupReadahead bool) (Reader, error) {
	gr := &reader{
		spanManager:         spanManager,
		r:                   r,
		layerSha:            layerSha,
		disableVerification: disableVerification,
	}
	if materialized != nil {
		gr.materializedRoot = materialized.Root
		gr.materializedFiles = materialized.Files
	}
	if enableStartupReadahead {
		gr.startupReadahead = true
		gr.readaheadSem = make(chan struct{}, startupReadaheadConcurrency)
	}
	return gr, nil
}

type reader struct {
	spanManager *spanmanager.SpanManager
	r           metadata.Reader
	layerSha    digest.Digest

	lastReadTime   time.Time
	lastReadTimeMu sync.Mutex

	closed   bool
	closedMu sync.Mutex

	disableVerification bool
	startupReadahead    bool
	readaheadSem        chan struct{}
	readaheadFiles      sync.Map
	readaheadWG         sync.WaitGroup
	openFileCache       sync.Map
	materializedRoot    string
	materializedFiles   map[string]string
}

type startupReadaheadKey struct {
	id     uint32
	window uint64
}

func (gr *reader) Metadata() metadata.Reader {
	return gr.r
}

func (gr *reader) setLastReadTime(lastReadTime time.Time) {
	gr.lastReadTimeMu.Lock()
	gr.lastReadTime = lastReadTime
	gr.lastReadTimeMu.Unlock()
}

func (gr *reader) LastOnDemandReadTime() time.Time {
	gr.lastReadTimeMu.Lock()
	t := gr.lastReadTime
	gr.lastReadTimeMu.Unlock()
	return t
}

func (gr *reader) OpenFile(id uint32) (io.ReaderAt, error) {
	if gr.isClosed() {
		return nil, fmt.Errorf("reader is already closed")
	}
	fr, err := gr.openMetadataFile(id)
	if err != nil {
		return nil, fmt.Errorf("failed to open file %d: %w", id, err)
	}
	if gr.disableVerification {
		if path, ok := gr.materializedPath(fr.TarName()); ok {
			return &materializedFile{path: path}, nil
		}
	}
	return &file{
		id: id,
		fr: fr,
		gr: gr,
	}, nil
}

func (gr *reader) materializedPath(name string) (string, bool) {
	if len(gr.materializedFiles) == 0 {
		return "", false
	}
	key, ok := MaterializedFileKey(name)
	if !ok {
		return "", false
	}
	path, ok := gr.materializedFiles[key]
	if !ok || path == "" {
		return "", false
	}
	if _, err := os.Stat(path); err == nil {
		return path, true
	}
	return "", false
}

func MaterializedFileKey(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || strings.HasPrefix(name, "/") {
		return "", false
	}
	clean := pathpkg.Clean(name)
	if clean == "." || clean != name || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}

func (gr *reader) openMetadataFile(id uint32) (metadata.File, error) {
	if cached, ok := gr.openFileCache.Load(id); ok {
		return cached.(metadata.File), nil
	}
	fr, err := gr.r.OpenFile(id)
	if err != nil {
		return nil, err
	}
	cached, _ := gr.openFileCache.LoadOrStore(id, fr)
	return cached.(metadata.File), nil
}

func (gr *reader) Close() (retErr error) {
	gr.closedMu.Lock()
	if gr.closed {
		gr.closedMu.Unlock()
		return nil
	}
	gr.closed = true
	gr.closedMu.Unlock()

	gr.readaheadWG.Wait()
	if gr.spanManager != nil {
		gr.spanManager.Close()
	}
	if err := gr.r.Close(); err != nil {
		retErr = errors.Join(retErr, err)
	}
	if gr.materializedRoot != "" {
		if err := os.RemoveAll(gr.materializedRoot); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}
	return
}

func (gr *reader) isClosed() bool {
	gr.closedMu.Lock()
	closed := gr.closed
	gr.closedMu.Unlock()
	return closed
}

type materializedFile struct {
	path string
}

func (mf *materializedFile) LocalPath() string {
	return mf.path
}

func (mf *materializedFile) ReadAt(p []byte, offset int64) (int, error) {
	f, err := os.Open(mf.path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.ReadAt(p, offset)
}

type file struct {
	id       uint32
	fr       metadata.File
	gr       *reader
	verified atomic.Bool
	lock     sync.Mutex
}

// ReadAt reads the file when the file is requested by the container
func (sf *file) ReadAt(p []byte, offset int64) (int, error) {
	if !sf.gr.disableVerification {
		if err := sf.Verify(); err != nil {
			return 0, err
		}
	}
	if len(p) == 0 {
		return 0, nil
	}
	uncompFileSize := sf.fr.GetUncompressedFileSize()
	if compression.Offset(offset) >= uncompFileSize {
		return 0, io.EOF
	}
	expectedSize := uncompFileSize - compression.Offset(offset)
	if expectedSize > compression.Offset(len(p)) {
		expectedSize = compression.Offset(len(p))
	}
	fileOffsetStart := sf.fr.GetUncompressedOffset() + compression.Offset(offset)
	fileOffsetEnd := fileOffsetStart + expectedSize
	readStart := time.Now()
	r, err := sf.gr.spanManager.GetContents(fileOffsetStart, fileOffsetEnd)
	if err != nil {
		traceStartupRead(sf, offset, expectedSize, time.Since(readStart), err)
		return 0, fmt.Errorf("failed to read the file: %w", err)
	}
	defer r.Close()

	// TODO this is not the right place for this metric to be. It needs to go down the BlobReader, when the HTTP request is issued
	commonmetrics.IncOperationCount(commonmetrics.SynchronousReadRegistryFetchCount, sf.gr.layerSha) // increment the number of on demand file fetches from remote registry
	sf.gr.setLastReadTime(time.Now())

	n, err := io.ReadFull(r, p[0:expectedSize])
	if err != nil {
		traceStartupRead(sf, offset, expectedSize, time.Since(readStart), err)
		return 0, fmt.Errorf("unexpected copied data size for on-demand fetch. read = %d, expected = %d: %w", n, expectedSize, err)
	}

	commonmetrics.AddBytesCount(commonmetrics.SynchronousBytesServed, sf.gr.layerSha, int64(n)) // measure the number of bytes served synchronously
	traceStartupRead(sf, offset, expectedSize, time.Since(readStart), nil)
	sf.gr.queueStartupReadahead(sf.id, sf.fr, fileOffsetStart)

	return n, nil
}

func traceStartupRead(sf *file, offset int64, size compression.Offset, duration time.Duration, err error) {
	if !startupReadTraceEnabled {
		return
	}
	if err == nil && duration < 2*time.Millisecond && !IsStartupHotPath(sf.fr.TarName()) {
		return
	}
	fields := logrus.Fields{
		"file_id":     sf.id,
		"path":        sf.fr.TarName(),
		"offset":      offset,
		"size":        int64(size),
		"duration_ms": float64(duration.Microseconds()) / 1000,
	}
	if err != nil {
		log.L.WithError(err).WithFields(fields).Info("hermes startup read failed")
		return
	}
	log.L.WithFields(fields).Info("hermes startup read")
}

func (gr *reader) queueStartupReadahead(id uint32, fr metadata.File, readStart compression.Offset) {
	if !gr.startupReadahead || gr.spanManager == nil || gr.readaheadSem == nil {
		return
	}
	if !IsStartupHotPath(fr.TarName()) {
		return
	}
	fileStart := fr.GetUncompressedOffset()
	fileEnd := fileStart + fr.GetUncompressedFileSize()
	if fileEnd <= fileStart {
		return
	}
	start := readStart
	if start < fileStart || start >= fileEnd {
		start = fileStart
	}
	window := uint64((start - fileStart) / startupReadaheadWindowSize)
	key := startupReadaheadKey{id: id, window: window}
	if _, loaded := gr.readaheadFiles.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	if !gr.tryStartStartupReadahead() {
		gr.readaheadFiles.Delete(key)
		traceStartupReadahead(fr, start, fileEnd, window, 0, "skipped", nil)
		return
	}
	traceStartupReadahead(fr, start, fileEnd, window, 0, "queued", nil)

	go func() {
		defer gr.readaheadWG.Done()
		defer func() { <-gr.readaheadSem }()
		startedAt := time.Now()
		err := gr.spanManager.ResolveSpanRange(start, fileEnd, startupReadaheadMaxSpans)
		traceStartupReadahead(fr, start, fileEnd, window, time.Since(startedAt), "done", err)
	}()
}

func traceStartupReadahead(fr metadata.File, start, end compression.Offset, window uint64, duration time.Duration, status string, err error) {
	if !startupReadTraceEnabled {
		return
	}
	fields := logrus.Fields{
		"path":        fr.TarName(),
		"start":       int64(start),
		"end":         int64(end),
		"window":      window,
		"max_spans":   startupReadaheadMaxSpans,
		"status":      status,
		"duration_ms": float64(duration.Microseconds()) / 1000,
	}
	if err != nil {
		log.L.WithError(err).WithFields(fields).Info("hermes startup readahead")
		return
	}
	log.L.WithFields(fields).Info("hermes startup readahead")
}

func (gr *reader) tryStartStartupReadahead() bool {
	gr.closedMu.Lock()
	defer gr.closedMu.Unlock()
	if gr.closed || gr.readaheadSem == nil {
		return false
	}
	select {
	case gr.readaheadSem <- struct{}{}:
		gr.readaheadWG.Add(1)
		return true
	default:
		return false
	}
}

func IsStartupHotPath(name string) bool {
	name = normalizedTarPath(name)
	if name == "." || name == "" {
		return false
	}
	return isRuntimeBundlePath(name) || isStartupArchivePath(name)
}

func isStartupArchivePath(name string) bool {
	switch pathpkg.Ext(name) {
	case ".jar", ".zip", ".jmod", ".jimage", ".jsa":
		return hasStartupArchiveSegment(name)
	default:
		return false
	}
}

func isRuntimeBundlePath(name string) bool {
	return strings.HasSuffix(name, "/lib/modules") ||
		strings.HasSuffix(name, "/lib/tzdb.dat") ||
		strings.Contains(name, "/lib/security/")
}

func hasStartupArchiveSegment(name string) bool {
	for _, segment := range strings.Split(name, "/") {
		switch segment {
		case "lib", "modules", "plugins", "extensions":
			return true
		}
	}
	return false
}

func normalizedTarPath(name string) string {
	return strings.TrimLeft(pathpkg.Clean(strings.TrimSpace(name)), "/")
}

// Verify verifies that the file's attributes match the tar header in the image layer
func (sf *file) Verify() (retErr error) {
	if sf.verified.Load() {
		return nil
	}
	sf.lock.Lock()
	defer sf.lock.Unlock()
	if sf.verified.Load() {
		return nil
	}
	defer func() {
		if retErr == nil {
			sf.verified.Store(true)
		}
	}()

	attr, err := sf.gr.r.GetAttr(sf.id)
	if err != nil {
		return err
	}

	tarHeaderOffset := sf.fr.TarHeaderOffset()
	tarHeaderSize := sf.fr.TarHeaderSize()
	if sf.fr.TarHeaderSize() < 0 {
		return fmt.Errorf("invalid tar header size: %d", sf.fr.TarHeaderSize())
	}
	tarHeaderReader, err := sf.gr.spanManager.GetContents(tarHeaderOffset, tarHeaderOffset+tarHeaderSize)
	if err != nil {
		return err
	}
	counterReader := ioutils.NewPositionTrackerReader(tarHeaderReader)
	tarReader := tar.NewReader(counterReader)
	tarHeader, err := tarReader.Next()
	if err != nil {
		return fmt.Errorf("error reading tar header at %d, size %d: %w", tarHeaderOffset, tarHeaderSize, err)
	}
	if counterReader.CurrentPos() != int64(tarHeaderSize) {
		return fmt.Errorf("incorrect tar header size: expected %d, actual %d", tarHeaderSize, counterReader.CurrentPos())
	}
	if !attrMatchesTarHeader(attr, tarHeader) {
		return errors.New("file attributes do not match tar header")
	}
	if sf.fr.TarName() != tarHeader.Name {
		return errors.New("file name does not match tar header")
	}

	return nil
}

func attrMatchesTarHeader(attr metadata.Attr, tarh *tar.Header) bool {
	// specifically, we don't look at attr.NumLink because it doesn't exist in a tar header
	if attr.Size != tarh.Size ||
		!attr.ModTime.Equal(tarh.ModTime) ||
		attr.LinkName != tarh.Linkname ||
		attr.Mode != tarh.FileInfo().Mode() ||
		attr.UID != tarh.Uid ||
		attr.GID != tarh.Gid ||
		attr.DevMajor != int(tarh.Devmajor) ||
		attr.DevMinor != int(tarh.Devminor) {
		return false
	}

	tarXattrs := ztoc.Xattrs(tarh.PAXRecords)
	if len(attr.Xattrs) != len(tarXattrs) {
		return false
	}
	for k := range attr.Xattrs {
		attrV := attr.Xattrs[k]
		tarV := tarXattrs[k]
		if len(attrV) != len(tarV) {
			return false
		}
		for i := 0; i < len(attrV); i++ {
			if attrV[i] != tarV[i] {
				return false
			}
		}
	}

	return true
}

type CacheOption func(*cacheOptions)

type cacheOptions struct {
	cacheOpts []cache.Option
	filter    func(int64) bool
	reader    *io.SectionReader
}

func WithCacheOpts(cacheOpts ...cache.Option) CacheOption {
	return func(opts *cacheOptions) {
		opts.cacheOpts = cacheOpts
	}
}

func WithFilter(filter func(int64) bool) CacheOption {
	return func(opts *cacheOptions) {
		opts.filter = filter
	}
}

func WithReader(sr *io.SectionReader) CacheOption {
	return func(opts *cacheOptions) {
		opts.reader = sr
	}
}
