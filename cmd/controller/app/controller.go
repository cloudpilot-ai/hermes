package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/cloudpilot-ai/hermes/cmd/controller/app/options"
	"github.com/cloudpilot-ai/hermes/pkg/controller"
	"github.com/urfave/cli/v3"
)

func NewControllerCommand(ctx context.Context) *cli.Command {
	opts := options.NewOptions()

	return &cli.Command{
		Name:  "hermes-controller",
		Usage: "Run the Hermes controller and artifact gateway",
		Flags: opts.Flags(),
		Action: func(_ context.Context, cmd *cli.Command) error {
			if err := opts.ApplyAndValidate(); err != nil {
				return err
			}
			return run(ctx, opts)
		},
	}
}

func run(ctx context.Context, opts *options.Options) error {
	cfg := opts.Config()

	if err := os.MkdirAll(cfg.HermesRoot, 0o755); err != nil {
		return fmt.Errorf("create hermes root: %w", err)
	}
	if err := os.MkdirAll(dirOf(cfg.DBPath), 0o755); err != nil {
		return fmt.Errorf("create db dir: %w", err)
	}

	store, err := controller.OpenStore(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	builder := controller.NewBuilder(cfg, store)
	builder.Start(ctx)

	if cfg.WatchKubernetes {
		go func() {
			if err := controller.StartPodWatcher(ctx, cfg, builder); err != nil && ctx.Err() == nil {
				log.Printf("pod watcher stopped: %v", err)
			}
		}()
	}

	server := controller.NewServer(cfg, store)
	if err := server.ListenAndServe(ctx); err != nil {
		return fmt.Errorf("server failed: %w", err)
	}
	return nil
}

func dirOf(path string) string {
	i := strings.LastIndex(path, "/")
	if i <= 0 {
		return "."
	}
	return path[:i]
}
