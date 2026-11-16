// Package config parses CLI flags and environment variables.
//
// Precedence: CLI flags > environment variables > defaults.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"
)

// Modes.
const (
	ModeDocker     = "docker"
	ModeKubernetes = "kubernetes"
)

// ErrHelp is returned when --help was requested.
var ErrHelp = flag.ErrHelp

// Config is the validated exporter configuration.
type Config struct {
	Mode                string
	ListenAddress       string
	DockerHost          string
	Kubeconfig          string
	ScanInterval        time.Duration
	RegistryInterval    time.Duration
	RegistryTimeout     time.Duration
	RegistryConcurrency int
	ImagePullSecrets    bool
	LogLevel            slog.Level
	ShowVersion         bool
}

// Parse builds the configuration from args (without the program name) and
// the environment lookup function. Usage and errors are written to out.
func Parse(args []string, getenv func(string) string, out io.Writer) (Config, error) {
	var (
		cfg      Config
		logLevel string
		envErrs  []error
	)

	env := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	envDuration := func(key string, def time.Duration) time.Duration {
		v := getenv(key)
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			envErrs = append(envErrs, fmt.Errorf("%s: invalid duration %q", key, v))
			return def
		}
		return d
	}
	envInt := func(key string, def int) int {
		v := getenv(key)
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			envErrs = append(envErrs, fmt.Errorf("%s: invalid integer %q", key, v))
			return def
		}
		return n
	}
	envBool := func(key string, def bool) bool {
		v := getenv(key)
		if v == "" {
			return def
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			envErrs = append(envErrs, fmt.Errorf("%s: invalid boolean %q", key, v))
			return def
		}
		return b
	}

	fs := flag.NewFlagSet("container-image-exporter", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&cfg.Mode, "mode", env("EXPORTER_MODE", ModeDocker),
		"runtime to inspect: docker or kubernetes [EXPORTER_MODE]")
	fs.StringVar(&cfg.ListenAddress, "listen-address", env("EXPORTER_LISTEN_ADDRESS", ":9877"),
		"HTTP listen address [EXPORTER_LISTEN_ADDRESS]")
	fs.StringVar(&cfg.DockerHost, "docker-host", env("EXPORTER_DOCKER_HOST", ""),
		"Docker Engine endpoint, e.g. unix:///var/run/docker.sock or tcp://socket-proxy:2375; empty uses DOCKER_HOST or the default socket [EXPORTER_DOCKER_HOST]")
	fs.StringVar(&cfg.Kubeconfig, "kubeconfig", env("EXPORTER_KUBECONFIG", ""),
		"kubeconfig path for local development; empty uses in-cluster authentication [EXPORTER_KUBECONFIG]")
	fs.DurationVar(&cfg.ScanInterval, "scan-interval", envDuration("EXPORTER_SCAN_INTERVAL", time.Minute),
		"interval between runtime discovery scans [EXPORTER_SCAN_INTERVAL]")
	fs.DurationVar(&cfg.RegistryInterval, "registry-interval", envDuration("EXPORTER_REGISTRY_INTERVAL", 6*time.Hour),
		"how long a successful registry check stays valid [EXPORTER_REGISTRY_INTERVAL]")
	fs.DurationVar(&cfg.RegistryTimeout, "registry-timeout", envDuration("EXPORTER_REGISTRY_TIMEOUT", 30*time.Second),
		"deadline for the registry requests of one image check [EXPORTER_REGISTRY_TIMEOUT]")
	fs.IntVar(&cfg.RegistryConcurrency, "registry-concurrency", envInt("EXPORTER_REGISTRY_CONCURRENCY", 4),
		"maximum parallel registry checks [EXPORTER_REGISTRY_CONCURRENCY]")
	fs.BoolVar(&cfg.ImagePullSecrets, "kubernetes-image-pull-secrets", envBool("EXPORTER_KUBERNETES_IMAGE_PULL_SECRETS", false),
		"read imagePullSecrets of discovered pods for registry authentication; needs RBAC get on secrets [EXPORTER_KUBERNETES_IMAGE_PULL_SECRETS]")
	fs.StringVar(&logLevel, "log-level", env("EXPORTER_LOG_LEVEL", "info"),
		"log level: debug, info, warn or error [EXPORTER_LOG_LEVEL]")
	fs.BoolVar(&cfg.ShowVersion, "version", false, "print version and exit")
	fs.Usage = func() {
		fmt.Fprintf(out, "Usage: container-image-exporter [flags]\n\n"+
			"Prometheus exporter that reports whether running container images have newer versions.\n"+
			"Precedence: CLI flags > environment variables [in brackets] > defaults.\n\nFlags:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if cfg.ShowVersion {
		return cfg, nil
	}

	// An invalid environment value only matters when no flag overrides it.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var errs []error
	for _, e := range envErrs {
		key, _, _ := strings.Cut(e.Error(), ":")
		if !set[envToFlag(key)] {
			errs = append(errs, e)
		}
	}

	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))
	if cfg.Mode != ModeDocker && cfg.Mode != ModeKubernetes {
		errs = append(errs, fmt.Errorf("--mode must be %q or %q, got %q", ModeDocker, ModeKubernetes, cfg.Mode))
	}
	if _, _, err := net.SplitHostPort(cfg.ListenAddress); err != nil {
		errs = append(errs, fmt.Errorf("--listen-address %q is not host:port", cfg.ListenAddress))
	}
	if cfg.ScanInterval < time.Second {
		errs = append(errs, fmt.Errorf("--scan-interval must be at least 1s, got %s", cfg.ScanInterval))
	}
	if cfg.RegistryInterval < time.Second {
		errs = append(errs, fmt.Errorf("--registry-interval must be at least 1s, got %s", cfg.RegistryInterval))
	}
	if cfg.RegistryTimeout < time.Second {
		errs = append(errs, fmt.Errorf("--registry-timeout must be at least 1s, got %s", cfg.RegistryTimeout))
	}
	if cfg.RegistryConcurrency < 1 || cfg.RegistryConcurrency > 64 {
		errs = append(errs, fmt.Errorf("--registry-concurrency must be between 1 and 64, got %d", cfg.RegistryConcurrency))
	}
	if err := cfg.LogLevel.UnmarshalText([]byte(logLevel)); err != nil {
		errs = append(errs, fmt.Errorf("--log-level must be debug, info, warn or error, got %q", logLevel))
	}
	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// envToFlag maps EXPORTER_SCAN_INTERVAL to scan-interval.
func envToFlag(key string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(key, "EXPORTER_")), "_", "-")
}
