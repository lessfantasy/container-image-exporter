package config

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	cfg, err := Parse(nil, env(nil), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Mode:                ModeDocker,
		ListenAddress:       ":9877",
		ScanInterval:        time.Minute,
		RegistryInterval:    6 * time.Hour,
		RegistryTimeout:     30 * time.Second,
		RegistryConcurrency: 4,
		LogLevel:            slog.LevelInfo,
	}
	if cfg != want {
		t.Errorf("got %+v, want %+v", cfg, want)
	}
}

func TestPrecedence(t *testing.T) {
	environment := env(map[string]string{
		"EXPORTER_MODE":                          "kubernetes",
		"EXPORTER_LISTEN_ADDRESS":                "127.0.0.1:9000",
		"EXPORTER_DOCKER_HOST":                   "tcp://proxy:2375",
		"EXPORTER_KUBECONFIG":                    "/tmp/kubeconfig",
		"EXPORTER_SCAN_INTERVAL":                 "2m",
		"EXPORTER_REGISTRY_INTERVAL":             "1h",
		"EXPORTER_REGISTRY_TIMEOUT":              "10s",
		"EXPORTER_REGISTRY_CONCURRENCY":          "8",
		"EXPORTER_KUBERNETES_IMAGE_PULL_SECRETS": "true",
		"EXPORTER_LOG_LEVEL":                     "debug",
	})

	// Environment beats defaults.
	cfg, err := Parse(nil, environment, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Mode:                ModeKubernetes,
		ListenAddress:       "127.0.0.1:9000",
		DockerHost:          "tcp://proxy:2375",
		Kubeconfig:          "/tmp/kubeconfig",
		ScanInterval:        2 * time.Minute,
		RegistryInterval:    time.Hour,
		RegistryTimeout:     10 * time.Second,
		RegistryConcurrency: 8,
		ImagePullSecrets:    true,
		LogLevel:            slog.LevelDebug,
	}
	if cfg != want {
		t.Errorf("env: got %+v, want %+v", cfg, want)
	}

	// Flags beat environment.
	cfg, err = Parse([]string{
		"--mode=docker", "--scan-interval=30s", "--registry-concurrency", "2",
		"--kubernetes-image-pull-secrets=false", "--log-level=warn",
	}, environment, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModeDocker || cfg.ScanInterval != 30*time.Second || cfg.RegistryConcurrency != 2 ||
		cfg.ImagePullSecrets || cfg.LogLevel != slog.LevelWarn {
		t.Errorf("flags: got %+v", cfg)
	}
	// Untouched options keep the environment value.
	if cfg.RegistryInterval != time.Hour {
		t.Errorf("RegistryInterval = %s", cfg.RegistryInterval)
	}
}

func TestInvalid(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"mode", []string{"--mode=podman"}, nil, "--mode"},
		{"listen address", []string{"--listen-address=9877"}, nil, "--listen-address"},
		{"scan interval", []string{"--scan-interval=0s"}, nil, "--scan-interval"},
		{"registry interval", []string{"--registry-interval=10ms"}, nil, "--registry-interval"},
		{"registry timeout", []string{"--registry-timeout=-1s"}, nil, "--registry-timeout"},
		{"concurrency low", []string{"--registry-concurrency=0"}, nil, "--registry-concurrency"},
		{"concurrency high", []string{"--registry-concurrency=1000"}, nil, "--registry-concurrency"},
		{"log level", []string{"--log-level=loud"}, nil, "--log-level"},
		{"positional argument", []string{"extra"}, nil, "unexpected argument"},
		{"env duration", nil, map[string]string{"EXPORTER_SCAN_INTERVAL": "soon"}, "EXPORTER_SCAN_INTERVAL"},
		{"env integer", nil, map[string]string{"EXPORTER_REGISTRY_CONCURRENCY": "many"}, "EXPORTER_REGISTRY_CONCURRENCY"},
		{"env boolean", nil, map[string]string{"EXPORTER_KUBERNETES_IMAGE_PULL_SECRETS": "maybe"}, "EXPORTER_KUBERNETES_IMAGE_PULL_SECRETS"},
		{"env mode", nil, map[string]string{"EXPORTER_MODE": "nomad"}, "--mode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.args, env(tt.env), &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want mention of %q", err, tt.want)
			}
		})
	}
}

func TestUnknownFlag(t *testing.T) {
	var out bytes.Buffer
	if _, err := Parse([]string{"--config=/etc/exporter.yaml"}, env(nil), &out); err == nil {
		t.Error("configuration file flag accepted")
	}
}

func TestInvalidEnvOverriddenByFlag(t *testing.T) {
	cfg, err := Parse([]string{"--scan-interval=45s"}, env(map[string]string{"EXPORTER_SCAN_INTERVAL": "soon"}), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ScanInterval != 45*time.Second {
		t.Errorf("ScanInterval = %s", cfg.ScanInterval)
	}
}

func TestHelpAndVersion(t *testing.T) {
	var out bytes.Buffer
	if _, err := Parse([]string{"--help"}, env(nil), &out); !errors.Is(err, ErrHelp) {
		t.Errorf("--help error = %v", err)
	}
	for _, want := range []string{"--mode", "-scan-interval", "EXPORTER_REGISTRY_TIMEOUT", "Precedence"} {
		if !strings.Contains(out.String(), strings.TrimPrefix(want, "-")) {
			t.Errorf("help output lacks %q", want)
		}
	}

	// --version works even when other settings are invalid.
	cfg, err := Parse([]string{"--version"}, env(map[string]string{"EXPORTER_MODE": "bogus"}), &bytes.Buffer{})
	if err != nil || !cfg.ShowVersion {
		t.Errorf("--version: cfg=%+v err=%v", cfg, err)
	}
}
