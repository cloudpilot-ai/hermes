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

package layer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudpilot-ai/hermes/pkg/common/cache"
	"github.com/cloudpilot-ai/hermes/pkg/common/config"

	backgroundfetcher "github.com/cloudpilot-ai/hermes/pkg/daemon/fs/backgroundfetcher"
	commonmetrics "github.com/cloudpilot-ai/hermes/pkg/daemon/fs/metrics/common"
	"github.com/cloudpilot-ai/hermes/pkg/daemon/fs/reader"
	"github.com/cloudpilot-ai/hermes/pkg/daemon/fs/remote"

	"github.com/cloudpilot-ai/hermes/pkg/common/idtools"
	"github.com/cloudpilot-ai/hermes/pkg/common/soci"
	"github.com/cloudpilot-ai/hermes/pkg/common/soci/ztoc"
	"github.com/cloudpilot-ai/hermes/pkg/common/soci/ztoc/compression"
	"github.com/cloudpilot-ai/hermes/pkg/common/util/lrucache"
	"github.com/cloudpilot-ai/hermes/pkg/common/util/namedmutex"
	spanmanager "github.com/cloudpilot-ai/hermes/pkg/daemon/fs/spanmanager"
	"github.com/cloudpilot-ai/hermes/pkg/daemon/metadata"
	"github.com/containerd/containerd/reference"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/containerd/log"
	fusefs "github.com/hanwen/go-fuse/v2/fs"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/semaphore"
	"oras.land/oras-go/v2/content"
)

const (
	defaultResolveResultEntry         = 30
	defaultMaxLRUCacheEntry           = 10
	defaultMaxCacheFds                = 10
	memoryCacheType                   = "memory"
	startupHotSpanCacheEntries        = 256
	defaultStartupMaterializeMaxBytes = 128 << 20
	defaultStartupMaterializeMaxFiles = 512

	BackgroundFetchEnabled  BackgroundFetchMode = "enabled"
	BackgroundFetchDisabled BackgroundFetchMode = "disabled"
)

var (
	startupMaterializeMaxBytes = envInt64("HERMES_STARTUP_MATERIALIZE_MAX_BYTES", defaultStartupMaterializeMaxBytes)
	startupMaterializeMaxFiles = envInt("HERMES_STARTUP_MATERIALIZE_MAX_FILES", defaultStartupMaterializeMaxFiles)
)

type BackgroundFetchMode string

type BackgroundFetchPolicy struct {
	Mode BackgroundFetchMode
}

// Layer represents a layer.
type Layer interface {
	// Info returns the information of this layer.
	Info() Info

	// RootNode returns the root node of this layer.
	RootNode(baseInode uint32, idMapper idtools.IDMap) (fusefs.InodeEmbedder, error)

	// Check checks if the layer is still connectable.
	Check() error

	// Refresh refreshes the layer connection.
	Refresh(ctx context.Context, hosts []docker.RegistryHost, refspec reference.Spec, desc ocispec.Descriptor) error

	// ReadAt reads this layer.
	ReadAt([]byte, int64, ...remote.Option) (int, error)

	// DisableXAttrs determines whether this layer should have xattrs disabled
	DisableXAttrs() bool

	// Done releases the reference to this layer. The resources related to this layer will be
	// discarded sooner or later. Queries after calling this function won't be serviced.
	Done()

	// GetCacheRefKey returns the reference key for the cache used by the layer
	GetCacheRefKey() string
}

// Info is the current status of a layer.
type Info struct {
	Digest      digest.Digest
	Size        int64     // layer size in bytes
	FetchedSize int64     // layer fetched size in bytes
	ReadTime    time.Time // last time the layer was read
}

// Resolver resolves the layer location and provieds the handler of that layer.
type Resolver struct {
	rootDir           string
	resolver          *remote.Resolver
	layerCache        *lrucache.Cache
	layerCacheMu      sync.Mutex
	blobCache         *lrucache.Cache
	blobCacheMu       sync.Mutex
	resolveLock       *namedmutex.NamedMutex
	config            config.FSConfig
	metadataStore     metadata.Store
	artifactStore     content.Storage
	overlayOpaqueType OverlayOpaqueType
	bgFetcher         *backgroundfetcher.BackgroundFetcher
	prefetchSemaphore *semaphore.Weighted
}

// NewResolver returns a new layer resolver.
func NewResolver(root string, cfg config.FSConfig, resolveHandlers map[string]remote.Handler,
	metadataStore metadata.Store, artifactStore content.Storage, overlayOpaqueType OverlayOpaqueType, bgFetcher *backgroundfetcher.BackgroundFetcher) (*Resolver, error) {
	resolveResultEntry := cfg.ResolveResultEntry
	if resolveResultEntry == 0 {
		resolveResultEntry = defaultResolveResultEntry
	}

	// layerCache caches resolved layers for future use. This is useful in a use-case where
	// the filesystem resolves and caches all layers in an image (not only queried one) in parallel,
	// before they are actually queried.
	layerCache := lrucache.New(resolveResultEntry)
	layerCache.OnEvicted = func(key string, value interface{}) {
		if err := value.(*layer).close(); err != nil {
			logrus.WithField("key", key).WithError(err).Warnf("failed to clean up layer")
			return
		}
		logrus.WithField("key", key).Debugf("cleaned up layer")
	}

	// blobCache caches resolved blobs for future use.
	blobCache := lrucache.New(resolveResultEntry)
	blobCache.OnEvicted = func(key string, value interface{}) {
		if err := value.(remote.Blob).Close(); err != nil {
			logrus.WithField("key", key).WithError(err).Warnf("failed to clean up blob")
			return
		}
		logrus.WithField("key", key).Debugf("cleaned up blob")
	}

	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}

	var prefetchSem *semaphore.Weighted
	if cfg.PrefetchConfig.Enable && cfg.PrefetchConfig.MaxConcurrency > 0 {
		prefetchSem = semaphore.NewWeighted(cfg.PrefetchConfig.MaxConcurrency)
	}

	return &Resolver{
		rootDir:           root,
		resolver:          remote.NewResolver(cfg.BlobConfig, resolveHandlers),
		layerCache:        layerCache,
		blobCache:         blobCache,
		config:            cfg,
		resolveLock:       new(namedmutex.NamedMutex),
		metadataStore:     metadataStore,
		artifactStore:     artifactStore,
		overlayOpaqueType: overlayOpaqueType,
		bgFetcher:         bgFetcher,
		prefetchSemaphore: prefetchSem,
	}, nil
}

func newCache(root string, cacheType string, cfg config.FSConfig) (cache.BlobCache, error) {
	if cacheType == memoryCacheType {
		return cache.NewMemoryCache(), nil
	}

	dcc := cfg.DirectoryCacheConfig
	maxDataEntry := dcc.MaxLRUCacheEntry
	if maxDataEntry == 0 {
		maxDataEntry = defaultMaxLRUCacheEntry
	}
	maxFdEntry := dcc.MaxCacheFds
	if maxFdEntry == 0 {
		maxFdEntry = defaultMaxCacheFds
	}

	bufPool := &sync.Pool{
		New: func() interface{} {
			return new(bytes.Buffer)
		},
	}
	dCache, fCache := lrucache.New(maxDataEntry), lrucache.New(maxFdEntry)
	dCache.OnEvicted = func(key string, value interface{}) {
		value.(*bytes.Buffer).Reset()
		bufPool.Put(value)
	}
	fCache.OnEvicted = func(key string, value interface{}) {
		value.(*os.File).Close()
	}
	// create a cache on an unique directory
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	cachePath, err := os.MkdirTemp(root, "")
	if err != nil {
		return nil, fmt.Errorf("failed to initialize directory cache: %w", err)
	}
	return cache.NewDirectoryCache(
		cachePath,
		cache.DirectoryCacheConfig{
			SyncAdd:   dcc.SyncAdd,
			DataCache: dCache,
			FdCache:   fCache,
			BufPool:   bufPool,
			Direct:    dcc.Direct,
		},
	)
}

func (r *Resolver) Evict(name string) {
	r.layerCacheMu.Lock()
	r.layerCache.Remove(name)
	r.layerCacheMu.Unlock()
}

// Resolve resolves a layer based on the passed layer blob information.
// prefetchDesc is optional - if provided, it will be used for prefetching spans
func (r *Resolver) Resolve(ctx context.Context, hosts []docker.RegistryHost, refspec reference.Spec, desc, sociDesc ocispec.Descriptor, opCounter *FuseOperationCounter, disableVerification bool, prefetchDesc *ocispec.Descriptor, bgPolicy BackgroundFetchPolicy, metadataOpts ...metadata.Option) (_ Layer, retErr error) {
	name := resolveCacheKey(refspec, desc, sociDesc, prefetchDesc, bgPolicy, disableVerification)

	// Wait if resolving this layer is already running. The result
	// can hopefully get from the LRU cache.
	r.resolveLock.Lock(name)
	defer r.resolveLock.Unlock(name)

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("src", name))

	// First, try to retrieve this layer from the underlying LRU cache.
	r.layerCacheMu.Lock()
	c, done, ok := r.layerCache.Get(name)
	r.layerCacheMu.Unlock()
	if ok {
		if l := c.(*layer); l.Check() == nil {
			log.G(ctx).Debugf("hit layer cache %q", name)
			return &layerRef{l, done}, nil
		}
		// Cached layer is invalid
		done()
		r.layerCacheMu.Lock()
		r.layerCache.Remove(name)
		r.layerCacheMu.Unlock()
	}

	log.G(ctx).Debugf("resolving")

	// Resolve the blob.
	blobR, err := r.resolveBlob(ctx, hosts, refspec, desc)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve the blob: %w", err)
	}
	defer func() {
		if retErr != nil {
			blobR.done()
		}
	}()

	spanCache, err := newCache(filepath.Join(r.rootDir, "spancache"), r.config.FSCacheType, r.config)
	if err != nil {
		return nil, fmt.Errorf("failed to create span manager cache: %w", err)
	}
	defer func() {
		if retErr != nil {
			spanCache.Close()
		}
	}()

	ztocReader, err := r.artifactStore.Fetch(ctx, sociDesc)
	if err != nil {
		return nil, err
	}
	defer ztocReader.Close()
	// Check if the ztoc exists (will be passed from fs)
	// If it exists, we decide if we want to lazily load layer, or
	// download/decompress the entire layer
	// If we decide to download/decompress the entire layer, getZtoc will not return the ztoc
	ztoc, err := ztoc.Unmarshal(ztocReader)

	if err != nil {
		// for now error out and let container runtime handle the layer download
		return nil, fmt.Errorf("cannot get ztoc; download and unpack this layer in container runtime for now: %w", err)
	}

	if ztoc == nil {
		// 1. download and unpack the layer
		// 2. return the reference to the layer
		// for now just error out, so container runtime takes care of this
		return nil, fmt.Errorf("download and unpack this layer in container runtime for now")
	}

	// log ztoc info
	log.G(ctx).WithFields(logrus.Fields{
		"layer_sha":      desc.Digest,
		"files_in_layer": len(ztoc.FileMetadata),
	}).Debugf("[Resolver.Resolve] downloaded layer ZTOC")
	// continue with resolving the layer presuming we handle ZTOC
	// ztoc will belong to a layer

	// Get a reader for the layer files
	// Each file's read operation is a prioritized task and all background tasks
	// will be stopped during the execution so this can avoid being disturbed for
	// NW traffic by background tasks.
	sr := io.NewSectionReader(readerAtFunc(func(p []byte, offset int64) (n int, err error) {
		return blobR.ReadAt(p, offset)
	}), 0, blobR.Size())
	// define telemetry hooks to measure latency metrics for the metadata store
	telemetry := metadata.Telemetry{
		InitMetadataStoreLatency: func(start time.Time) {
			commonmetrics.MeasureLatencyInMilliseconds(commonmetrics.InitMetadataStore, desc.Digest, start)
		},
	}
	fileMetadata := ztoc.TOC.FileMetadata
	meta, err := r.metadataStore(sr, ztoc.TOC, append(metadataOpts, metadata.WithTelemetry(&telemetry))...)
	if err != nil {
		return nil, err
	}
	ztoc.TOC.FileMetadata = nil
	log.G(ctx).Debugf("[Resolver.Resolve]Initialized metadata store for layer sha=%v", desc.Digest)

	spanManager, err := spanmanager.New(ztoc, sr, spanCache, r.config.BlobConfig.MaxSpanVerificationRetries, cache.Direct())
	if err != nil {
		return nil, fmt.Errorf("error creating span manager: %w", err)
	}
	startupProfile := hasStartupPrefetchProfile(prefetchDesc)
	var bgLayerResolver backgroundfetcher.Resolver
	if r.bgFetcher == nil {
		// Background fetcher disabled globally.
	} else if bgPolicy.Mode == BackgroundFetchDisabled {
		log.G(ctx).WithField("layerDigest", desc.Digest.String()).Debug("background fetch disabled for layer by SOCI index annotation")
	} else {
		bgLayerResolver = backgroundfetcher.NewSequentialResolver(desc.Digest, spanManager)
		r.bgFetcher.Add(bgLayerResolver)
	}

	materialized, err := r.materializeStartupFiles(ctx, desc.Digest, spanManager, fileMetadata, startupProfile)
	if err != nil {
		log.G(ctx).WithError(err).Warn("Failed to materialize startup files, continuing without local materialized files")
	}
	var asyncPrefetchWG *sync.WaitGroup
	if materialized == nil || len(materialized.Files) == 0 {
		asyncPrefetchWG, err = r.executePrefetch(ctx, spanManager, prefetchDesc)
		if err != nil {
			log.G(ctx).WithError(err).Warn("Failed to execute prefetch, continuing without prefetch")
		}
	}
	if shouldEnableStartupHotCache(startupProfile, materialized) {
		spanManager.EnableHotCache(startupHotSpanCacheEntries)
	}

	vr, err := reader.NewReaderWithMaterializedFiles(meta, desc.Digest, spanManager, disableVerification, materialized, startupProfile)
	if err != nil {
		return nil, fmt.Errorf("failed to read layer: %w", err)
	}
	disableXAttrs := getDisableXAttrAnnotation(sociDesc)
	// Combine layer information together and cache it.
	l := newLayer(r, desc, name, blobR, vr, bgLayerResolver, asyncPrefetchWG, opCounter, disableXAttrs)
	r.layerCacheMu.Lock()
	cachedL, done2, added := r.layerCache.Add(name, l)
	r.layerCacheMu.Unlock()
	if !added {
		l.close() // layer already exists in the cache. discard this.
	}

	log.G(ctx).Debugf("resolved layer")
	return &layerRef{cachedL.(*layer), done2}, nil
}

// resolveBlob resolves a blob based on the passed layer blob information.
func (r *Resolver) resolveBlob(ctx context.Context, hosts []docker.RegistryHost, refspec reference.Spec, desc ocispec.Descriptor) (_ *blobRef, retErr error) {
	name := refspec.String() + "/" + desc.Digest.String()

	// Try to retrieve the blob from the underlying LRU cache.
	r.blobCacheMu.Lock()
	c, done, ok := r.blobCache.Get(name)
	r.blobCacheMu.Unlock()
	if ok {
		if blob := c.(remote.Blob); blob.Check() == nil {
			return &blobRef{blob, done}, nil
		}
		// invalid blob. discard this.
		done()
		r.blobCacheMu.Lock()
		r.blobCache.Remove(name)
		r.blobCacheMu.Unlock()
	}

	// Resolve the blob and cache the result.
	b, err := r.resolver.Resolve(ctx, hosts, refspec, desc)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve the source: %w", err)
	}
	r.blobCacheMu.Lock()
	cachedB, done, added := r.blobCache.Add(name, b)
	r.blobCacheMu.Unlock()
	if !added {
		b.Close() // blob already exists in the cache. discard this.
	}
	return &blobRef{cachedB.(remote.Blob), done}, nil
}

func newLayer(
	resolver *Resolver,
	desc ocispec.Descriptor,
	cacheRefKey string,
	blob *blobRef,
	r reader.Reader,
	bgResolver backgroundfetcher.Resolver,
	asyncPrefetchWG *sync.WaitGroup,
	opCounter *FuseOperationCounter,
	disableXAttrs bool,
) *layer {
	return &layer{
		resolver:             resolver,
		desc:                 desc,
		cacheRefKey:          cacheRefKey,
		blob:                 blob,
		r:                    r,
		bgResolver:           bgResolver,
		asyncPrefetchWG:      asyncPrefetchWG,
		fuseOperationCounter: opCounter,
		disableXAttrs:        disableXAttrs,
	}
}

type layer struct {
	resolver    *Resolver
	desc        ocispec.Descriptor
	cacheRefKey string
	blob        *blobRef

	bgResolver      backgroundfetcher.Resolver
	asyncPrefetchWG *sync.WaitGroup

	r reader.Reader

	fuseOperationCounter *FuseOperationCounter
	disableXAttrs        bool

	closed   bool
	closedMu sync.Mutex
}

func (l *layer) GetCacheRefKey() string {
	return l.cacheRefKey
}

func (l *layer) Info() Info {
	return Info{
		Digest:      l.desc.Digest,
		Size:        l.blob.Size(),
		FetchedSize: l.blob.FetchedSize(),
		ReadTime:    l.r.LastOnDemandReadTime(),
	}
}

func (l *layer) Check() error {
	if l.isClosed() {
		return fmt.Errorf("layer is already closed")
	}
	return l.blob.Check()
}

func (l *layer) Refresh(ctx context.Context, hosts []docker.RegistryHost, refspec reference.Spec, desc ocispec.Descriptor) error {
	if l.isClosed() {
		return fmt.Errorf("layer is already closed")
	}
	return l.blob.Refresh(ctx, hosts, refspec, desc)
}

func (l *layerRef) Done() {
	l.done()
}

func (l *layer) RootNode(baseInode uint32, idMapper idtools.IDMap) (fusefs.InodeEmbedder, error) {
	if l.isClosed() {
		return nil, fmt.Errorf("layer is already closed")
	}
	return newNode(l, baseInode, idMapper)
}

func (l *layer) ReadAt(p []byte, offset int64, opts ...remote.Option) (int, error) {
	return l.blob.ReadAt(p, offset, opts...)
}

func (l *layer) DisableXAttrs() bool {
	return l.disableXAttrs
}

func (l *layer) close() error {
	l.closedMu.Lock()
	defer l.closedMu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if l.bgResolver != nil {
		l.bgResolver.Close()
	}
	if l.asyncPrefetchWG != nil {
		l.asyncPrefetchWG.Wait()
	}
	defer l.blob.done() // Close reader first, then close the blob
	return l.r.Close()
}

func (l *layer) isClosed() bool {
	l.closedMu.Lock()
	closed := l.closed
	l.closedMu.Unlock()
	return closed
}

func getDisableXAttrAnnotation(desc ocispec.Descriptor) bool {
	stringVal, present := desc.Annotations[soci.IndexAnnotationDisableXAttrs]
	if !present {
		return false
	}
	val, err := strconv.ParseBool(stringVal)
	if err != nil {
		return false
	}

	return val
}

// blobRef is a reference to the blob in the cache. Calling `done` decreases the reference counter
// of this blob in the underlying cache. When nobody refers to the blob in the cache, resources bound
// to this blob will be discarded.
type blobRef struct {
	remote.Blob
	done func()
}

// layerRef is a reference to the layer in the cache. Calling `Done` or `done` decreases the
// reference counter of this blob in the underlying cache. When nobody refers to the layer in the
// cache, resources bound to this layer will be discarded.
type layerRef struct {
	*layer
	done func()
}

type readerAtFunc func([]byte, int64) (int, error)

func (f readerAtFunc) ReadAt(p []byte, offset int64) (int, error) { return f(p, offset) }

func (r *Resolver) executePrefetch(ctx context.Context, spanManager *spanmanager.SpanManager, prefetchDesc *ocispec.Descriptor) (*sync.WaitGroup, error) {
	if prefetchDesc == nil {
		return nil, nil
	}

	if !r.config.PrefetchConfig.Enable && prefetchDesc.Annotations[soci.IndexAnnotationHermesPrefetchProfile] == "" {
		log.G(ctx).Debug("Prefetch is disabled in config, skipping prefetch")
		return nil, nil
	}

	prefetchArtifact, err := r.loadPrefetchArtifact(ctx, prefetchDesc)
	if err != nil {
		if errors.Is(err, soci.ErrEmptyPrefetchArtifact) {
			log.G(ctx).Debug("Prefetch artifact is empty, skipping prefetch")
			return nil, nil
		}
		return nil, fmt.Errorf("failed to load prefetch artifact: %w", err)
	}

	var syncSpans []compression.SpanID
	var asyncSpans []compression.SpanID
	startupProfile := prefetchDesc.Annotations[soci.IndexAnnotationHermesPrefetchProfile] != ""
	for _, prefetchSpan := range prefetchArtifact.PrefetchSpans {
		for spanID := prefetchSpan.StartSpan; spanID <= prefetchSpan.EndSpan; spanID++ {
			if prefetchSpan.Priority > 0 {
				if !startupProfile {
					asyncSpans = append(asyncSpans, spanID)
				}
			} else {
				syncSpans = append(syncSpans, spanID)
			}
		}
	}

	if len(syncSpans) == 0 && len(asyncSpans) == 0 {
		return nil, nil
	}
	if err := r.executePrefetchSpans(ctx, spanManager, syncSpans); err != nil {
		return nil, err
	}
	if len(asyncSpans) > 0 {
		asyncWG := &sync.WaitGroup{}
		asyncWG.Add(1)
		asyncCtx := context.WithoutCancel(ctx)
		go func() {
			defer asyncWG.Done()
			if err := r.executePrefetchSpans(asyncCtx, spanManager, asyncSpans); err != nil {
				log.G(asyncCtx).WithError(err).Debug("async prefetch failed")
			}
		}()
		return asyncWG, nil
	}
	return nil, nil
}

func (r *Resolver) executePrefetchSpans(ctx context.Context, spanManager *spanmanager.SpanManager, spansToFetch []compression.SpanID) error {
	if len(spansToFetch) == 0 {
		return nil
	}

	if r.prefetchSemaphore != nil {
		if err := r.prefetchSemaphore.Acquire(ctx, 1); err != nil {
			return err
		}
		defer r.prefetchSemaphore.Release(1)
	}

	spanChan := make(chan compression.SpanID, len(spansToFetch))
	numWorkers := runtime.GOMAXPROCS(0)
	if numWorkers > len(spansToFetch) {
		numWorkers = len(spansToFetch)
	}

	var wg sync.WaitGroup
	var errMu sync.Mutex
	var retErr error
	recordErr := func(spanID compression.SpanID, err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		retErr = errors.Join(retErr, fmt.Errorf("prefetch span %d: %w", spanID, err))
		errMu.Unlock()
	}

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for spanID := range spanChan {
				if err := spanManager.ResolveSpan(spanID); err != nil {
					recordErr(spanID, err)
				}
			}
		}()
	}

	for _, spanID := range spansToFetch {
		spanChan <- spanID
	}
	close(spanChan)

	wg.Wait()
	return retErr
}

type startupMaterializeCandidate struct {
	name     string
	offset   compression.Offset
	size     compression.Offset
	priority int
}

func (r *Resolver) materializeStartupFiles(ctx context.Context, layerDigest digest.Digest, spanManager *spanmanager.SpanManager, files []ztoc.FileMetadata, enabled bool) (*reader.MaterializedFileSet, error) {
	if !enabled || len(files) == 0 {
		return nil, nil
	}

	candidates := startupMaterializeCandidates(files)
	if len(candidates) == 0 {
		return nil, nil
	}

	rootParent := filepath.Join(r.rootDir, "materialized")
	if err := os.MkdirAll(rootParent, 0700); err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp(rootParent, "startup-*")
	if err != nil {
		return nil, err
	}

	out := &reader.MaterializedFileSet{
		Root:  root,
		Files: make(map[string]string),
	}
	var totalBytes int64
	var totalFiles int
	var retErr error
	for _, candidate := range candidates {
		if totalFiles >= startupMaterializeMaxFiles {
			break
		}
		if startupMaterializeMaxBytes <= 0 || startupMaterializeMaxFiles <= 0 {
			break
		}
		if totalBytes+int64(candidate.size) > startupMaterializeMaxBytes {
			continue
		}
		localPath, err := materializeStartupFile(ctx, spanManager, root, candidate)
		if err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("%s: %w", candidate.name, err))
			continue
		}
		out.Files[candidate.name] = localPath
		totalBytes += int64(candidate.size)
		totalFiles++
	}
	if len(out.Files) == 0 {
		_ = os.RemoveAll(root)
		out = nil
	}
	log.G(ctx).WithFields(logrus.Fields{
		"layerDigest":       layerDigest.String(),
		"materializedFiles": totalFiles,
		"materializedBytes": totalBytes,
	}).Info("materialized startup files for local reads")
	return out, retErr
}

func startupMaterializeCandidates(files []ztoc.FileMetadata) []startupMaterializeCandidate {
	candidates := make([]startupMaterializeCandidate, 0)
	seen := map[string]struct{}{}
	for _, file := range files {
		name, ok := reader.MaterializedFileKey(file.Name)
		if !ok || file.Type != "reg" || file.UncompressedSize <= 0 || !isStartupMaterializePath(name) {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		candidates = append(candidates, startupMaterializeCandidate{
			name:     name,
			offset:   file.UncompressedOffset,
			size:     file.UncompressedSize,
			priority: startupMaterializePriority(name),
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority < candidates[j].priority
		}
		if candidates[i].size != candidates[j].size {
			return candidates[i].size > candidates[j].size
		}
		return candidates[i].name < candidates[j].name
	})
	return candidates
}

func startupMaterializePriority(name string) int {
	name = normalizeStartupMaterializePath(name)
	switch {
	case name == "usr/share/opensearch/jdk/lib/modules":
		return 0
	case strings.HasPrefix(name, "usr/share/opensearch/bin/"):
		return 1
	case strings.HasPrefix(name, "usr/share/opensearch/config/"):
		return 2
	case strings.HasPrefix(name, "usr/share/opensearch/jdk/bin/"):
		return 3
	case strings.HasPrefix(name, "usr/share/opensearch/jdk/lib/") && strings.HasSuffix(name, ".so"):
		return 4
	case strings.HasPrefix(name, "usr/share/opensearch/lib/"):
		return 5
	case strings.HasPrefix(name, "usr/share/opensearch/modules/"):
		return 6
	case strings.HasPrefix(name, "usr/share/opensearch/plugins/"):
		return 7
	default:
		return 8
	}
}

func isStartupMaterializePath(name string) bool {
	name = normalizeStartupMaterializePath(name)
	if name == "" || name == "." {
		return false
	}
	if reader.IsStartupHotPath(name) {
		return true
	}
	if strings.HasPrefix(name, "usr/share/opensearch/bin/") ||
		strings.HasPrefix(name, "usr/share/opensearch/config/") ||
		strings.HasPrefix(name, "usr/share/opensearch/jdk/bin/") ||
		strings.HasPrefix(name, "usr/share/opensearch/jdk/conf/") {
		return true
	}
	if strings.HasPrefix(name, "usr/share/opensearch/jdk/lib/") {
		return strings.HasSuffix(name, ".so") ||
			strings.HasSuffix(name, ".cfg") ||
			strings.Contains(name, "/security/") ||
			strings.Contains(name, "/tzdb.dat")
	}
	if strings.HasPrefix(name, "usr/share/opensearch/modules/") ||
		strings.HasPrefix(name, "usr/share/opensearch/plugins/") {
		return strings.HasSuffix(name, ".jar") ||
			strings.HasSuffix(name, ".properties") ||
			strings.HasSuffix(name, ".policy") ||
			strings.HasSuffix(name, ".yml") ||
			strings.HasSuffix(name, ".yaml")
	}
	return false
}

func materializeStartupFile(ctx context.Context, spanManager *spanmanager.SpanManager, root string, candidate startupMaterializeCandidate) (string, error) {
	r, err := spanManager.GetContents(candidate.offset, candidate.offset+candidate.size)
	if err != nil {
		return "", err
	}
	defer r.Close()

	sum := sha256.Sum256([]byte(candidate.name))
	localPath := filepath.Join(root, hex.EncodeToString(sum[:])+".file")
	tmpPath := localPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return "", err
	}
	written, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return "", copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return "", closeErr
	}
	if written != int64(candidate.size) {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("materialized %d bytes, expected %d", written, candidate.size)
	}
	if err := os.Rename(tmpPath, localPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	log.G(ctx).WithFields(logrus.Fields{
		"path": candidate.name,
		"size": candidate.size,
	}).Debug("materialized startup file")
	return localPath, nil
}

func normalizeStartupMaterializePath(name string) string {
	key, ok := reader.MaterializedFileKey(name)
	if !ok {
		return ""
	}
	return key
}

func envInt64(name string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func hasStartupPrefetchProfile(prefetchDesc *ocispec.Descriptor) bool {
	return prefetchDesc != nil && prefetchDesc.Annotations[soci.IndexAnnotationHermesPrefetchProfile] != ""
}

func shouldEnableStartupHotCache(startupProfile bool, materialized *reader.MaterializedFileSet) bool {
	return startupProfile && (materialized == nil || len(materialized.Files) == 0)
}

func resolveCacheKey(refspec reference.Spec, desc, sociDesc ocispec.Descriptor, prefetchDesc *ocispec.Descriptor, bgPolicy BackgroundFetchPolicy, disableVerification bool) string {
	prefetchKey := ""
	prefetchProfile := ""
	if prefetchDesc != nil {
		prefetchKey = prefetchDesc.Digest.String()
		prefetchProfile = prefetchDesc.Annotations[soci.IndexAnnotationHermesPrefetchProfile]
	}
	return strings.Join([]string{
		refspec.String(),
		desc.Digest.String(),
		sociDesc.Digest.String(),
		prefetchKey,
		prefetchProfile,
		string(bgPolicy.Mode),
		strconv.FormatBool(disableVerification),
	}, "|")
}

func (r *Resolver) loadPrefetchArtifact(ctx context.Context, prefetchDesc *ocispec.Descriptor) (*soci.PrefetchArtifact, error) {
	log.G(ctx).Infof("Loading prefetch artifact %s", prefetchDesc.Digest)

	reader, err := r.artifactStore.Fetch(ctx, *prefetchDesc)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch prefetch artifact %s: %w", prefetchDesc.Digest, err)
	}
	defer reader.Close()

	artifact, err := soci.UnmarshalPrefetchArtifact(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal prefetch artifact: %w", err)
	}

	log.G(ctx).Infof("Successfully loaded prefetch artifact with %d span ranges", len(artifact.PrefetchSpans))
	return artifact, nil
}
