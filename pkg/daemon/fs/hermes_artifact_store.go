package fs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cloudpilot-ai/hermes/pkg/common/config"
	"github.com/cloudpilot-ai/hermes/pkg/common/soci"
	"github.com/cloudpilot-ai/hermes/pkg/common/soci/store"
	"github.com/cloudpilot-ai/hermes/pkg/common/util/ioutils"
	"github.com/containerd/containerd/reference"
	"github.com/containerd/log"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
)

type hermesResolveResponse struct {
	Image     string             `json:"image"`
	Platform  string             `json:"platform"`
	SOCIIndex ocispec.Descriptor `json:"sociIndex"`
}

// FetchHermesArtifacts fetches the index and zTOCs from the Hermes controller,
// stores them in the local content store, then lets the existing lazy mount
// path continue unchanged for real image layer reads.
func FetchHermesArtifacts(ctx context.Context, cfg config.ExternalArtifactStoreConfig, imageRef, imageManifestDigest string, localStore store.Store) (*soci.Index, error) {
	refspec, err := reference.Parse(imageRef)
	if err != nil {
		return nil, err
	}
	imageDigestRef := fmt.Sprintf("%s@%s", refspec.Locator, imageManifestDigest)

	client := &http.Client{Timeout: time.Duration(cfg.TimeoutSec) * time.Second}
	resolveResp, err := hermesResolve(ctx, client, cfg, imageDigestRef)
	if err != nil {
		return nil, err
	}

	log.G(ctx).WithField("image", imageDigestRef).
		WithField("digest", resolveResp.SOCIIndex.Digest.String()).
		Info("fetching index from Hermes artifact store")

	indexBytes, err := hermesFetchBlob(ctx, client, cfg.Endpoint, resolveResp.SOCIIndex.Digest.String())
	if err != nil {
		return nil, fmt.Errorf("unable to fetch Hermes index: %w", err)
	}
	tr := ioutils.NewPositionTrackerReader(bytes.NewReader(indexBytes))

	var index soci.Index
	if err := soci.DecodeIndex(tr, &index); err != nil {
		return nil, fmt.Errorf("cannot deserialize Hermes index: %w", err)
	}

	indexDesc := resolveResp.SOCIIndex
	indexDesc.Size = tr.CurrentPos()

	ctx, batchDone, err := localStore.BatchOpen(ctx)
	if err != nil {
		return nil, err
	}
	defer batchDone(ctx)

	if err := localStore.Push(ctx, indexDesc, bytes.NewReader(indexBytes)); err != nil && !store.IsErrAlreadyExists(err) {
		return nil, fmt.Errorf("unable to store Hermes index locally: %w", err)
	}
	if err := store.LabelGCRoot(ctx, localStore, indexDesc); err != nil {
		return nil, fmt.Errorf("unable to label Hermes index: %w", err)
	}

	eg, ctx := errgroup.WithContext(ctx)
	for i, blob := range index.Blobs {
		i, blob := i, blob
		eg.Go(func() error {
			content, err := hermesFetchBlob(ctx, client, cfg.Endpoint, blob.Digest.String())
			if err != nil {
				return fmt.Errorf("cannot fetch Hermes artifact %s: %w", blob.Digest, err)
			}
			if blob.Size == 0 {
				blob.Size = int64(len(content))
			}
			if err := localStore.Push(ctx, blob, bytes.NewReader(content)); err != nil && !store.IsErrAlreadyExists(err) {
				return fmt.Errorf("unable to store Hermes zTOC %s locally: %w", blob.Digest, err)
			}
			return store.LabelGCRefContent(ctx, localStore, indexDesc, "ztoc."+strconv.Itoa(i), blob.Digest.String())
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	return &index, nil
}

func hermesResolve(ctx context.Context, client *http.Client, cfg config.ExternalArtifactStoreConfig, imageDigestRef string) (*hermesResolveResponse, error) {
	q := url.Values{}
	q.Set("image", imageDigestRef)
	if cfg.Platform != "" {
		q.Set("platform", cfg.Platform)
	}
	endpoint := strings.TrimRight(cfg.Endpoint, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/indexes/resolve?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("Hermes resolve returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out hermesResolveResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.SOCIIndex.Digest == "" {
		return nil, fmt.Errorf("Hermes resolve returned empty index digest")
	}
	return &out, nil
}

func hermesFetchBlob(ctx context.Context, client *http.Client, endpoint, dgst string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/v1/blobs/"+dgst, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("Hermes blob fetch returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return io.ReadAll(resp.Body)
}
