package options

import (
	"fmt"

	"github.com/cloudpilot-ai/hermes/pkg/common/config"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v3"
)

const (
	DefaultAddress = "/run/hermes-daemon/hermes-daemon.sock"

	envAddress  = "HERMES_DAEMON_ADDRESS"
	envConfig   = "HERMES_DAEMON_CONFIG"
	envLogLevel = "HERMES_DAEMON_LOG_LEVEL"
	envRoot     = "HERMES_DAEMON_ROOT"
)

type Options struct {
	Address    string
	ConfigPath string
	LogLevel   string
	Root       string

	ParsedLogLevel logrus.Level
}

func NewOptions() *Options {
	return &Options{
		Address:    DefaultAddress,
		ConfigPath: config.DefaultConfigPath,
		LogLevel:   logrus.InfoLevel.String(),
		Root:       config.DefaultDaemonRootPath,
	}
}

func (o *Options) Flags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:        "address",
			Usage:       "address for the snapshotter's GRPC server",
			Value:       o.Address,
			Sources:     cli.EnvVars(envAddress),
			Destination: &o.Address,
		},
		&cli.StringFlag{
			Name:        "config",
			Usage:       "path to the configuration file",
			Value:       o.ConfigPath,
			Sources:     cli.EnvVars(envConfig),
			Destination: &o.ConfigPath,
		},
		&cli.StringFlag{
			Name:        "log-level",
			Usage:       "set the logging level [trace, debug, info, warn, error, fatal, panic]",
			Value:       o.LogLevel,
			Sources:     cli.EnvVars(envLogLevel),
			Destination: &o.LogLevel,
		},
		&cli.StringFlag{
			Name:        "root",
			Usage:       "path to the root directory for this snapshotter",
			Value:       o.Root,
			Sources:     cli.EnvVars(envRoot),
			Destination: &o.Root,
		},
	}
}

func (o *Options) ApplyAndValidate() error {
	lvl, err := logrus.ParseLevel(o.LogLevel)
	if err != nil {
		return fmt.Errorf("failed to prepare logger: %w", err)
	}
	o.ParsedLogLevel = lvl
	return nil
}
