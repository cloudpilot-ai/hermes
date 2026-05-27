package controller

import (
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	statusBuilding = "Building"
	statusReady    = "Ready"
	statusFailed   = "Failed"
)

type Config struct {
	ListenAddr        string
	DBPath            string
	Kubeconfig        string
	WatchKubernetes   bool
	WatchNamespace    string
	Platform          string
	MaxConcurrency    int
	QueueSize         int
	BuildTimeout      time.Duration
	ContainerdAddress string
	ContainerdNS      string
	HermesRoot        string
	SpanSize          int64
	MinLayerSize      int64
	Optimizations     []string
	PullImage         bool
}

type BuildTask struct {
	SourceImageRef string
	Platform       string
	Reason         string
	PolicyNames    []string
	RegistryAuths  []RegistryAuth
}

type RegistryAuth struct {
	Host     string
	Username string
	Secret   string
}

type Artifact struct {
	ID                  int64  `json:"id"`
	SourceImageRef      string `json:"sourceImageRef"`
	ImageDigestRef      string `json:"imageDigestRef"`
	ImageManifestDigest string `json:"imageManifestDigest"`
	ImageConfigDigest   string `json:"imageConfigDigest"`
	Platform            string `json:"platform"`
	IndexDigest         string `json:"indexDigest"`
	IndexMediaType      string `json:"indexMediaType"`
	IndexSize           int64  `json:"indexSize"`
	Status              string `json:"status"`
	Error               string `json:"error"`
}

type LayerArtifact struct {
	LayerDigest string
	ZtocDigest  string
	ZtocSize    int64
}

type ResolveResponse struct {
	Image     string             `json:"image"`
	Platform  string             `json:"platform"`
	SOCIIndex ocispec.Descriptor `json:"sociIndex"`
	Ztocs     []ZtocResponse     `json:"ztocs"`
}

type ZtocResponse struct {
	LayerDigest string `json:"layerDigest"`
	ZtocDigest  string `json:"ztocDigest"`
	Size        int64  `json:"size"`
}
