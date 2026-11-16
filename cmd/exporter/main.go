// Command container-image-exporter reports through Prometheus metrics
// whether running container images have newer versions available.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/container-image-exporter/container-image-exporter/internal/config"
	"github.com/container-image-exporter/container-image-exporter/internal/discovery"
	"github.com/container-image-exporter/container-image-exporter/internal/discovery/docker"
	"github.com/container-image-exporter/container-image-exporter/internal/discovery/kubernetes"
	"github.com/container-image-exporter/container-image-exporter/internal/registry"
	"github.com/container-image-exporter/container-image-exporter/internal/scanner"
	"github.com/container-image-exporter/container-image-exporter/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cfg, err := config.Parse(args, getenv, stderr)
	if errors.Is(err, config.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "invalid configuration:\n%v\n\nRun with --help for usage.\n", err)
		return 2
	}
	if cfg.ShowVersion {
		fmt.Fprintf(stdout, "container-image-exporter %s\n", version)
		return 0
	}

	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	disc, closeDisc, err := newDiscoverer(cfg, logger)
	if err != nil {
		logger.Error("startup failed", "error", err)
		return 1
	}
	defer closeDisc()

	reg := registry.New(registry.Options{
		Keychain:  registry.DefaultKeychain(logger),
		UserAgent: "container-image-exporter/" + version,
	})
	scan := scanner.New(disc, reg, scanner.Options{
		ScanInterval:     cfg.ScanInterval,
		RegistryInterval: cfg.RegistryInterval,
		RegistryTimeout:  cfg.RegistryTimeout,
		Concurrency:      cfg.RegistryConcurrency,
		Logger:           logger,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("starting", "version", version, "mode", cfg.Mode,
		"scan_interval", cfg.ScanInterval, "registry_interval", cfg.RegistryInterval)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		scan.Run(ctx)
	}()

	err = server.Run(ctx, cfg.ListenAddress, server.NewHandler(scan.Snapshot), logger)
	stop()
	wg.Wait()
	if err != nil {
		logger.Error("exiting", "error", err)
		return 1
	}
	logger.Info("shut down")
	return 0
}

func newDiscoverer(cfg config.Config, logger *slog.Logger) (discovery.Discoverer, func(), error) {
	switch cfg.Mode {
	case config.ModeKubernetes:
		core, meta, err := kubernetes.NewClients(cfg.Kubeconfig)
		if err != nil {
			return nil, nil, err
		}
		return kubernetes.New(core, meta, cfg.ImagePullSecrets, logger), func() {}, nil
	default:
		cli, err := docker.NewClient(cfg.DockerHost)
		if err != nil {
			return nil, nil, fmt.Errorf("create docker client: %w", err)
		}
		return docker.New(cli, logger), func() { _ = cli.Close() }, nil
	}
}
