package main

import (
	"bytes"
	"strings"
	"testing"
)

func noEnv(string) string { return "" }

func TestVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--version"}, noEnv, &stdout, &stderr); code != 0 {
		t.Errorf("exit code = %d", code)
	}
	if !strings.HasPrefix(stdout.String(), "container-image-exporter ") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--help"}, noEnv, &stdout, &stderr); code != 0 {
		t.Errorf("exit code = %d", code)
	}
	if !strings.Contains(stderr.String(), "--mode") && !strings.Contains(stderr.String(), "-mode") {
		t.Errorf("usage missing: %q", stderr.String())
	}
}

func TestInvalidConfigurationExitsNonZero(t *testing.T) {
	tests := [][]string{
		{"--mode=podman"},
		{"--scan-interval=never"},
		{"--registry-concurrency=0"},
		{"--no-such-flag"},
		{"--config=/etc/exporter.yaml"},
	}
	for _, args := range tests {
		var stdout, stderr bytes.Buffer
		if code := run(args, noEnv, &stdout, &stderr); code == 0 {
			t.Errorf("%v: exit code 0", args)
		}
		if stderr.Len() == 0 {
			t.Errorf("%v: no error message", args)
		}
	}
}

func TestKubernetesModeWithoutClusterFailsClearly(t *testing.T) {
	var stdout, stderr bytes.Buffer
	env := func(k string) string {
		if k == "EXPORTER_MODE" {
			return "kubernetes"
		}
		return ""
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	if code := run([]string{"--listen-address=127.0.0.1:0"}, env, &stdout, &stderr); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "kubernetes") {
		t.Errorf("stderr = %q", stderr.String())
	}
}
