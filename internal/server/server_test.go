package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/container-image-exporter/container-image-exporter/internal/metrics"
)

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func TestEndpointsBeforeFirstScan(t *testing.T) {
	h := NewHandler(func() *metrics.Snapshot { return nil })
	if code, _ := get(t, h, "/metrics"); code != http.StatusServiceUnavailable {
		t.Errorf("/metrics = %d, want 503 before the first scan", code)
	}
	if code, _ := get(t, h, "/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d, want 503", code)
	}
	if code, _ := get(t, h, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", code)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	var snap atomic.Pointer[metrics.Snapshot]
	snap.Store(metrics.Build([]metrics.Observation{
		{Platform: "docker", Container: "web", Registry: "docker.io", Repository: "library/nginx", CurrentTag: "1.26.2", LatestTag: "1.27.3", OK: true, UpdateAvailable: true},
		{Platform: "kubernetes", Container: "api", Namespace: "prod", WorkloadKind: "Deployment", Registry: "ghcr.io", Repository: "org/api", CurrentTag: "abc123"},
	}))
	h := NewHandler(snap.Load)

	code, body := get(t, h, "/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics = %d", code)
	}
	for _, want := range []string{
		`container_image_info{container="web",current_tag="1.26.2",latest_tag="1.27.3",namespace="",platform="docker",registry="docker.io",repository="library/nginx",workload_kind=""} 1`,
		`container_image_update_available{container="web",namespace="",platform="docker",registry="docker.io",repository="library/nginx",workload_kind=""} 1`,
		`container_image_info{container="api",current_tag="abc123",latest_tag="",namespace="prod",platform="kubernetes",registry="ghcr.io",repository="org/api",workload_kind="Deployment"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing line:\n%s\nin:\n%s", want, body)
		}
	}
	if strings.Contains(body, `container_image_update_available{container="api"`) {
		t.Error("update series exposed for an unknown status")
	}

	// Only the two families, no runtime, process or handler metrics.
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		name := strings.TrimPrefix(strings.TrimPrefix(line, "# HELP "), "# TYPE ")
		if !strings.HasPrefix(name, "container_image_info") && !strings.HasPrefix(name, "container_image_update_available") {
			t.Errorf("unexpected line: %s", line)
		}
	}
	for _, forbidden := range []string{"go_", "process_", "promhttp_", "prometheus_"} {
		if strings.Contains(body, "\n"+forbidden) || strings.Contains(body, " "+forbidden) {
			t.Errorf("default metrics with prefix %q exposed", forbidden)
		}
	}

	if code, _ := get(t, h, "/readyz"); code != http.StatusOK {
		t.Errorf("/readyz = %d", code)
	}
	if code, _ := get(t, h, "/"); code != http.StatusOK {
		t.Errorf("/ = %d", code)
	}
	if code, _ := get(t, h, "/unknown"); code != http.StatusNotFound {
		t.Errorf("/unknown = %d", code)
	}

	// The inventory becomes unknown again: scrapes fail instead of
	// returning an empty result.
	snap.Store(nil)
	if code, _ := get(t, h, "/metrics"); code != http.StatusServiceUnavailable {
		t.Errorf("/metrics = %d after the snapshot was withdrawn", code)
	}
}

func TestRunShutsDownGracefully(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- Run(ctx, addr, NewHandler(func() *metrics.Snapshot { return &metrics.Snapshot{} }), logger)
	}()

	var resp *http.Response
	for range 200 {
		if resp, err = http.Get("http://" + addr + "/healthz"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server not reachable: %v", err)
	}
	resp.Body.Close()

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	// The address is taken: a clear error, no hang.
	l, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := Run(context.Background(), l.Addr().String(), http.NotFoundHandler(), logger); err == nil {
		t.Error("Run succeeded on an occupied address")
	}
}
