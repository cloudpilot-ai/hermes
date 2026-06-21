package controller

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	hermesv1 "github.com/cloudpilot-ai/hermes/pkg/apis/v1alpha1"
	sociapi "github.com/cloudpilot-ai/hermes/pkg/common/soci"
	socistore "github.com/cloudpilot-ai/hermes/pkg/common/soci/store"
	"github.com/containerd/containerd"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/reference"
	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const buildToolIdentifier = "Hermes Builder v0.1"

type Builder struct {
	cfg            Config
	store          *Store
	queue          chan BuildTask
	seen           *StringSet
	building       *StringSet
	statusMu       sync.RWMutex
	recorder       PolicyStatusRecorder
	policyMu       sync.Mutex
	rawPolicies    map[string][]string
	digestPolicies map[string][]string
}

func NewBuilder(cfg Config, store *Store, recorders ...PolicyStatusRecorder) *Builder {
	b := &Builder{
		cfg:            cfg,
		store:          store,
		queue:          make(chan BuildTask, cfg.QueueSize),
		seen:           NewStringSet(),
		building:       NewStringSet(),
		rawPolicies:    map[string][]string{},
		digestPolicies: map[string][]string{},
	}
	if len(recorders) > 0 {
		b.recorder = recorders[0]
	}
	return b
}

func (b *Builder) SetPolicyStatusRecorder(recorder PolicyStatusRecorder) {
	b.statusMu.Lock()
	defer b.statusMu.Unlock()
	b.recorder = recorder
}

func (b *Builder) Start(ctx context.Context) {
	for i := 0; i < b.cfg.MaxConcurrency; i++ {
		go b.worker(ctx, i)
	}
}

func (b *Builder) Enqueue(task BuildTask) bool {
	if task.Platform == "" {
		task.Platform = b.cfg.Platform
	}
	key := taskKey(task.SourceImageRef, task.Platform, task.Acceleration.Key())
	b.rememberRawPolicies(key, task.PolicyNames)
	if !b.seen.Add(key) {
		return true
	}
	select {
	case b.queue <- task:
		log.Printf("enqueued image=%s platform=%s reason=%s", task.SourceImageRef, task.Platform, task.Reason)
		return true
	default:
		b.seen.Delete(key)
		b.forgetRawPolicies(key)
		return false
	}
}

func (b *Builder) worker(ctx context.Context, id int) {
	for {
		select {
		case <-ctx.Done():
			return
		case task := <-b.queue:
			key := taskKey(task.SourceImageRef, task.Platform, task.Acceleration.Key())
			task.PolicyNames = b.policyNamesForRawKey(key, task.PolicyNames)
			err := b.Build(ctx, task)
			b.forgetRawPolicies(key)
			b.seen.Delete(key)
			if err != nil {
				log.Printf("builder-%d failed image=%s platform=%s: %v", id, task.SourceImageRef, task.Platform, err)
			}
		}
	}
}

func (b *Builder) Build(ctx context.Context, task BuildTask) error {
	if task.Platform == "" {
		task.Platform = b.cfg.Platform
	}
	accelerationKey := task.Acceleration.Key()
	buildConfig := b.effectiveBuildConfig(task)
	buildCtx, cancel := context.WithTimeout(ctx, b.cfg.BuildTimeout)
	defer cancel()

	if imageDigestRef, ok := canonicalDigestRef(task.SourceImageRef); ok {
		ready, err := b.store.HasReady(ctx, imageDigestRef, task.Platform, accelerationKey)
		if err != nil {
			return err
		}
		if ready {
			log.Printf("skip cached index image=%s platform=%s reason=%s", imageDigestRef, task.Platform, task.Reason)
			b.recordPolicyBuild(ctx, task, imageDigestRef, task.Platform, hermesv1.HermesImagePhaseReady, nil)
			return nil
		}
	}

	client, err := containerd.New(b.cfg.ContainerdAddress)
	if err != nil {
		return err
	}
	defer client.Close()
	buildCtx = namespaces.WithNamespace(buildCtx, b.cfg.ContainerdNS)

	var img containerd.Image
	if b.cfg.PullImage {
		img, err = b.pullImage(buildCtx, client, task.SourceImageRef, task.Platform, task.RegistryAuths)
		if err != nil {
			_ = b.store.MarkFailed(ctx, task.SourceImageRef, task.SourceImageRef, task.Platform, accelerationKey, err)
			return err
		}
	} else {
		img, err = client.GetImage(buildCtx, task.SourceImageRef)
		if err != nil {
			_ = b.store.MarkFailed(ctx, task.SourceImageRef, task.SourceImageRef, task.Platform, accelerationKey, err)
			return err
		}
	}

	imageDigestRef, manifestDigest, configDigest, err := b.resolveImage(buildCtx, client, img, task.SourceImageRef, task.Platform)
	if err != nil {
		_ = b.store.MarkFailed(ctx, task.SourceImageRef, task.SourceImageRef, task.Platform, accelerationKey, err)
		return err
	}

	ready, err := b.store.HasReady(ctx, imageDigestRef, task.Platform, accelerationKey)
	if err != nil {
		return err
	}
	if ready {
		log.Printf("skip cached index image=%s source=%s platform=%s reason=%s", imageDigestRef, task.SourceImageRef, task.Platform, task.Reason)
		b.recordPolicyBuild(ctx, task, imageDigestRef, task.Platform, hermesv1.HermesImagePhaseReady, nil)
		b.cleanupSourceImage(ctx, client, img, imageDigestRef, task.Platform, "cached")
		return nil
	}

	buildKey := taskKey(imageDigestRef, task.Platform, accelerationKey)
	task.PolicyNames = b.rememberDigestPolicies(buildKey, b.policyNamesForRawKey(taskKey(task.SourceImageRef, task.Platform, accelerationKey), task.PolicyNames))
	if !b.building.Add(buildKey) {
		log.Printf("skip in-flight index build image=%s source=%s platform=%s reason=%s", imageDigestRef, task.SourceImageRef, task.Platform, task.Reason)
		return nil
	}
	defer b.building.Delete(buildKey)
	defer b.forgetDigestPolicies(buildKey)

	marked, err := b.store.MarkBuilding(ctx, task, imageDigestRef, manifestDigest, buildConfig, accelerationKey)
	if err != nil {
		return err
	}
	if !marked {
		log.Printf("skip cached index image=%s source=%s platform=%s reason=%s", imageDigestRef, task.SourceImageRef, task.Platform, task.Reason)
		b.recordPolicyBuild(ctx, task, imageDigestRef, task.Platform, hermesv1.HermesImagePhaseReady, nil)
		b.cleanupSourceImage(ctx, client, img, imageDigestRef, task.Platform, "cached")
		return nil
	}
	b.recordPolicyBuild(ctx, task, imageDigestRef, task.Platform, hermesv1.HermesImagePhaseBuilding, nil)

	indexDesc, err := b.buildIndex(buildCtx, client, img, task, buildConfig)
	if err != nil {
		_ = b.store.MarkFailed(ctx, task.SourceImageRef, imageDigestRef, task.Platform, accelerationKey, err)
		b.recordPolicyBuild(ctx, task, imageDigestRef, task.Platform, hermesv1.HermesImagePhaseFailed, err)
		return err
	}

	indexDesc, indexBytes, ztocs, ztocBytes, layers, err := b.readIndexArtifacts(buildCtx, indexDesc)
	if err != nil {
		_ = b.store.MarkFailed(ctx, task.SourceImageRef, imageDigestRef, task.Platform, accelerationKey, err)
		b.recordPolicyBuild(ctx, task, imageDigestRef, task.Platform, hermesv1.HermesImagePhaseFailed, err)
		return err
	}

	artifact := Artifact{
		SourceImageRef:      task.SourceImageRef,
		ImageDigestRef:      imageDigestRef,
		ImageManifestDigest: manifestDigest,
		ImageConfigDigest:   configDigest,
		Platform:            task.Platform,
	}
	if err := b.store.PutReady(ctx, artifact, indexDesc, indexBytes, ztocs, ztocBytes, layers, buildConfig, accelerationKey); err != nil {
		b.recordPolicyBuild(ctx, task, imageDigestRef, task.Platform, hermesv1.HermesImagePhaseFailed, err)
		return err
	}
	b.recordPolicyBuild(ctx, task, imageDigestRef, task.Platform, hermesv1.HermesImagePhaseReady, nil)
	b.cleanupSourceImage(ctx, client, img, imageDigestRef, task.Platform, "built")

	log.Printf("ready image=%s platform=%s index=%s ztocs=%d", imageDigestRef, task.Platform, indexDesc.Digest, len(ztocs))
	return nil
}

func (b *Builder) cleanupSourceImage(ctx context.Context, client *containerd.Client, img containerd.Image, imageDigestRef, platform, reason string) {
	if !b.cfg.PullImage || client == nil || img == nil {
		return
	}
	name := img.Name()
	if name == "" {
		return
	}

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	cleanupCtx = namespaces.WithNamespace(cleanupCtx, b.cfg.ContainerdNS)

	start := time.Now()
	if err := client.ImageService().Delete(cleanupCtx, name, images.SynchronousDelete()); err != nil {
		log.Printf("cleanup source image failed image=%s digest=%s platform=%s reason=%s: %v", name, imageDigestRef, platform, reason, err)
		return
	}
	log.Printf("cleaned source image image=%s digest=%s platform=%s reason=%s elapsed=%s", name, imageDigestRef, platform, reason, time.Since(start).Round(time.Millisecond))
}

func (b *Builder) recordPolicyBuild(ctx context.Context, task BuildTask, imageDigestRef, platform string, phase hermesv1.HermesImagePhase, buildErr error) {
	policyNames := b.policyNamesForStatus(task, imageDigestRef, platform)
	if len(policyNames) == 0 {
		return
	}
	b.statusMu.RLock()
	recorder := b.recorder
	b.statusMu.RUnlock()
	if recorder == nil {
		return
	}
	recorder.RecordBuild(ctx, policyNames, imageDigestRef, platform, phase, buildErr)
}

func taskKey(imageRef, platform string, accelerationKeys ...string) string {
	key := imageRef + "|" + platform
	if len(accelerationKeys) > 0 && accelerationKeys[0] != "" {
		key += "|" + accelerationKeys[0]
	}
	return key
}

func (b *Builder) rememberRawPolicies(key string, policyNames []string) []string {
	return b.rememberPolicies(b.rawPolicies, key, policyNames)
}

func (b *Builder) forgetRawPolicies(key string) {
	b.forgetPolicies(b.rawPolicies, key)
}

func (b *Builder) rememberDigestPolicies(key string, policyNames []string) []string {
	return b.rememberPolicies(b.digestPolicies, key, policyNames)
}

func (b *Builder) forgetDigestPolicies(key string) {
	b.forgetPolicies(b.digestPolicies, key)
}

func (b *Builder) rememberPolicies(items map[string][]string, key string, policyNames []string) []string {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	items[key] = uniqueStrings(append(items[key], policyNames...))
	return append([]string(nil), items[key]...)
}

func (b *Builder) forgetPolicies(items map[string][]string, key string) {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	delete(items, key)
}

func (b *Builder) policyNamesForRawKey(key string, policyNames []string) []string {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	return uniqueStrings(append(append([]string(nil), policyNames...), b.rawPolicies[key]...))
}

func (b *Builder) policyNamesForStatus(task BuildTask, imageDigestRef, platform string) []string {
	accelerationKey := task.Acceleration.Key()
	rawKey := taskKey(task.SourceImageRef, platform, accelerationKey)
	digestKey := taskKey(imageDigestRef, platform, accelerationKey)
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	names := append([]string(nil), task.PolicyNames...)
	names = append(names, b.rawPolicies[rawKey]...)
	names = append(names, b.digestPolicies[digestKey]...)
	return uniqueStrings(names)
}

func canonicalDigestRef(imageRef string) (string, bool) {
	refspec, err := reference.Parse(imageRef)
	if err != nil {
		return "", false
	}
	dgst := refspec.Digest()
	if dgst == "" || dgst.Validate() != nil {
		return "", false
	}
	return fmt.Sprintf("%s@%s", refspec.Locator, dgst.String()), true
}

func (b *Builder) pullImage(ctx context.Context, client *containerd.Client, imageRef, platform string, auths []RegistryAuth) (containerd.Image, error) {
	start := time.Now()
	log.Printf("pulling image=%s platform=%s through containerd API", imageRef, platform)
	opts := []containerd.RemoteOpt{containerd.WithPlatform(platform)}
	if resolver := registryResolver(auths); resolver != nil {
		opts = append(opts, containerd.WithResolver(resolver))
	}
	img, err := client.Pull(ctx, imageRef, opts...)
	if err != nil {
		return nil, fmt.Errorf("pull image %s platform=%s: %w", imageRef, platform, err)
	}
	log.Printf("pulled image=%s platform=%s elapsed=%s", imageRef, platform, time.Since(start).Round(time.Millisecond))
	return img, nil
}

func (b *Builder) resolveImage(ctx context.Context, client *containerd.Client, img containerd.Image, imageRef, platform string) (imageDigestRef, manifestDigest, configDigest string, err error) {
	plat, err := platforms.Parse(platform)
	if err != nil {
		return "", "", "", err
	}
	matcher := platforms.OnlyStrict(plat)
	manifestDesc, err := sociapi.GetImageManifestDescriptor(ctx, client.ContentStore(), img.Target(), matcher)
	if err != nil {
		return "", "", "", err
	}
	manifest, err := images.Manifest(ctx, client.ContentStore(), img.Target(), matcher)
	if err != nil {
		return "", "", "", err
	}
	refspec, err := reference.Parse(imageRef)
	if err != nil {
		return "", "", "", err
	}
	return fmt.Sprintf("%s@%s", refspec.Locator, manifestDesc.Digest.String()), manifestDesc.Digest.String(), manifest.Config.Digest.String(), nil
}

func (b *Builder) effectiveBuildConfig(task BuildTask) Config {
	cfg := b.cfg
	cfg.MinLayerSize = task.Acceleration.MinLayerSize(cfg.MinLayerSize)
	return cfg
}

func (b *Builder) buildIndex(ctx context.Context, client *containerd.Client, img containerd.Image, task BuildTask, cfg Config) (ocispec.Descriptor, error) {
	optimizations, err := parseOptimizations(b.cfg.Optimizations)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	blobStore, err := socistore.NewContentStore(
		socistore.WithType(socistore.SociContentStoreType),
		socistore.WithSnapshotterRoot(b.cfg.HermesRoot),
	)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	artifactsDb, err := sociapi.NewDB(sociapi.ArtifactsDbPath(b.cfg.HermesRoot))
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	builder, err := sociapi.NewIndexBuilder(
		client.ContentStore(),
		blobStore,
		sociapi.WithMinLayerSize(cfg.MinLayerSize),
		sociapi.WithSpanSize(cfg.SpanSize),
		sociapi.WithBuildToolIdentifier(buildToolIdentifier),
		sociapi.WithOptimizations(optimizations),
		sociapi.WithArtifactsDb(artifactsDb),
		sociapi.WithIndexAnnotations(task.Acceleration.IndexAnnotations()),
		sociapi.WithPrefetchPaths(task.Acceleration.PrefetchPaths()),
		sociapi.WithPrefetchMaxSpans(task.Acceleration.PrefetchMaxSpans()),
		sociapi.WithPrefetchMaxSpansPerFile(task.Acceleration.prefetchMaxSpansPerFile()),
		sociapi.WithPrefetchArchiveEdgeSpans(task.Acceleration.prefetchArchiveEdges()),
	)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	plat, err := platforms.Parse(task.Platform)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	start := time.Now()
	log.Printf("building index in-process image=%s platform=%s span=%d minLayer=%d prefetchProfile=%s prefetchMaxSpans=%d", img.Name(), task.Platform, cfg.SpanSize, cfg.MinLayerSize, task.Acceleration.PrefetchProfile(), task.Acceleration.PrefetchMaxSpans())
	indexWithMetadata, err := builder.Build(ctx, img.Metadata(), sociapi.WithPlatform(plat), sociapi.WithNoGarbageCollectionLabel())
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	log.Printf("built index in-process image=%s platform=%s index=%s elapsed=%s", img.Name(), task.Platform, indexWithMetadata.Desc.Digest, time.Since(start).Round(time.Millisecond))
	return indexWithMetadata.Desc, nil
}

func parseOptimizations(values []string) ([]sociapi.Optimization, error) {
	out := make([]sociapi.Optimization, 0, len(values))
	for _, value := range values {
		optimization, err := sociapi.ParseOptimization(value)
		if err != nil {
			return nil, err
		}
		out = append(out, optimization)
	}
	return out, nil
}

func (b *Builder) readIndexArtifacts(ctx context.Context, indexDesc ocispec.Descriptor) (ocispec.Descriptor, []byte, []ocispec.Descriptor, map[string][]byte, []LayerArtifact, error) {
	blobStore, err := socistore.NewContentStore(
		socistore.WithType(socistore.SociContentStoreType),
		socistore.WithSnapshotterRoot(b.cfg.HermesRoot),
	)
	if err != nil {
		return ocispec.Descriptor{}, nil, nil, nil, nil, err
	}

	indexBytes, err := fetchAll(ctx, blobStore, indexDesc)
	if err != nil {
		return ocispec.Descriptor{}, nil, nil, nil, nil, err
	}
	indexDesc.Size = int64(len(indexBytes))

	var idx sociapi.Index
	if err := sociapi.DecodeIndex(bytes.NewReader(indexBytes), &idx); err != nil {
		return ocispec.Descriptor{}, nil, nil, nil, nil, err
	}

	var ztocs []ocispec.Descriptor
	ztocBytes := make(map[string][]byte, len(idx.Blobs))
	var layers []LayerArtifact
	for _, blob := range idx.Blobs {
		content, err := fetchAll(ctx, blobStore, blob)
		if err != nil {
			return ocispec.Descriptor{}, nil, nil, nil, nil, err
		}
		blob.Size = int64(len(content))
		ztocs = append(ztocs, blob)
		ztocBytes[blob.Digest.String()] = content
		if blob.MediaType == sociapi.SociLayerMediaType {
			layers = append(layers, LayerArtifact{
				LayerDigest: blob.Annotations[sociapi.IndexAnnotationImageLayerDigest],
				ZtocDigest:  blob.Digest.String(),
				ZtocSize:    blob.Size,
			})
		}
	}
	return indexDesc, indexBytes, ztocs, ztocBytes, layers, nil
}

func fetchAll(ctx context.Context, store socistore.Store, desc ocispec.Descriptor) ([]byte, error) {
	rc, err := store.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}
