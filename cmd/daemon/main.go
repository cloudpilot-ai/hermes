package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/cloudpilot-ai/hermes/cmd/daemon/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cmd := app.NewDaemonCommand(ctx)
	if err := cmd.Run(ctx, os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "hermes-daemon: %v\n", err)
		os.Exit(1)
	}
}
