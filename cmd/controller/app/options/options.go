package options

import (
	"strings"
	"time"

	"github.com/cloudpilot-ai/hermes/pkg/controller"
	"github.com/urfave/cli/v3"
)

type Options struct {
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
	Optimizations     string
	PullImage         bool
}

func NewOptions() *Options {
	return &Options{
		ListenAddr:        ":39091",
		DBPath:            "/data/hermes/hermes-cache.db",
		Platform:          "linux/amd64",
		WatchKubernetes:   true,
		MaxConcurrency:    1,
		QueueSize:         128,
		BuildTimeout:      60 * time.Minute,
		ContainerdAddress: "/run/containerd/containerd.sock",
		ContainerdNS:      "k8s.io",
		HermesRoot:        "/var/lib/hermes",
		SpanSize:          1 << 22,
		MinLayerSize:      10 << 20,
		Optimizations:     "xattr",
		PullImage:         true,
	}
}

func (o *Options) Flags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "listen", Usage: "HTTP listen address for artifact gateway", Value: o.ListenAddr, Destination: &o.ListenAddr},
		&cli.StringFlag{Name: "db", Usage: "SQLite database path", Value: o.DBPath, Destination: &o.DBPath},
		&cli.StringFlag{Name: "kubeconfig", Usage: "kubeconfig path; defaults to in-cluster config or ~/.kube/config", Destination: &o.Kubeconfig},
		&cli.BoolFlag{Name: "watch-kubernetes", Usage: "watch Kubernetes Pods and enqueue their images", Value: o.WatchKubernetes, Destination: &o.WatchKubernetes},
		&cli.StringFlag{Name: "namespace", Usage: "namespace to watch; empty means all namespaces", Destination: &o.WatchNamespace},
		&cli.StringFlag{Name: "platform", Usage: "platform to build index artifacts for", Value: o.Platform, Destination: &o.Platform},
		&cli.IntFlag{Name: "max-concurrency", Usage: "max concurrent index builds", Value: o.MaxConcurrency, Destination: &o.MaxConcurrency},
		&cli.IntFlag{Name: "queue-size", Usage: "build queue size", Value: o.QueueSize, Destination: &o.QueueSize},
		&cli.DurationFlag{Name: "build-timeout", Usage: "timeout per build", Value: o.BuildTimeout, Destination: &o.BuildTimeout},
		&cli.StringFlag{Name: "containerd-address", Usage: "containerd socket path", Value: o.ContainerdAddress, Destination: &o.ContainerdAddress},
		&cli.StringFlag{Name: "containerd-namespace", Usage: "containerd namespace", Value: o.ContainerdNS, Destination: &o.ContainerdNS},
		&cli.StringFlag{Name: "hermes-root", Usage: "Hermes artifact root for generated index artifacts", Value: o.HermesRoot, Destination: &o.HermesRoot},
		&cli.Int64Flag{Name: "span-size", Usage: "index span size", Value: o.SpanSize, Destination: &o.SpanSize},
		&cli.Int64Flag{Name: "min-layer-size", Usage: "minimum layer size to build zTOC", Value: o.MinLayerSize, Destination: &o.MinLayerSize},
		&cli.StringFlag{Name: "optimizations", Usage: "comma-separated index create optimizations; empty disables", Value: o.Optimizations, Destination: &o.Optimizations},
		&cli.BoolFlag{Name: "pull-image", Usage: "pull image into containerd before building index artifacts", Value: o.PullImage, Destination: &o.PullImage},
	}
}

func (o *Options) ApplyAndValidate() error {
	if o.MaxConcurrency < 1 {
		o.MaxConcurrency = 1
	}
	if o.QueueSize < 1 {
		o.QueueSize = 1
	}
	return nil
}

func (o *Options) Config() controller.Config {
	return controller.Config{
		ListenAddr:        o.ListenAddr,
		DBPath:            o.DBPath,
		Kubeconfig:        o.Kubeconfig,
		WatchKubernetes:   o.WatchKubernetes,
		WatchNamespace:    o.WatchNamespace,
		Platform:          o.Platform,
		MaxConcurrency:    o.MaxConcurrency,
		QueueSize:         o.QueueSize,
		BuildTimeout:      o.BuildTimeout,
		ContainerdAddress: o.ContainerdAddress,
		ContainerdNS:      o.ContainerdNS,
		HermesRoot:        o.HermesRoot,
		SpanSize:          o.SpanSize,
		MinLayerSize:      o.MinLayerSize,
		Optimizations:     splitCSV(o.Optimizations),
		PullImage:         o.PullImage,
	}
}

func splitCSV(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
