// Package tests holds end-to-end tests. They run the real Docker SDK
// client, the real registry client, the scanner and the HTTP handler
// against local fakes: an HTTP Docker Engine API, the Kubernetes fake
// clientset and an in-memory OCI registry. No live registry or cloud
// credentials are needed.
package tests

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"

	"github.com/container-image-exporter/container-image-exporter/internal/discovery"
	"github.com/container-image-exporter/container-image-exporter/internal/discovery/docker"
	"github.com/container-image-exporter/container-image-exporter/internal/discovery/kubernetes"
	"github.com/container-image-exporter/container-image-exporter/internal/registry"
	"github.com/container-image-exporter/container-image-exporter/internal/scanner"
	"github.com/container-image-exporter/container-image-exporter/internal/server"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// testRegistry is an in-memory OCI registry that records request methods.
type testRegistry struct {
	host string
	mu   sync.Mutex
	// methods seen after recording was enabled
	methods map[string]int
	record  bool
}

func newTestRegistry(t *testing.T) *testRegistry {
	t.Helper()
	r := &testRegistry{methods: map[string]int{}}
	inner := ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		if r.record {
			r.methods[req.Method]++
		}
		r.mu.Unlock()
		inner.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	r.host = strings.TrimPrefix(srv.URL, "http://")
	return r
}

func (r *testRegistry) startRecording() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record = true
}

func (r *testRegistry) assertReadOnly(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.methods) == 0 {
		t.Error("no registry requests recorded")
	}
	for method := range r.methods {
		if method != http.MethodGet && method != http.MethodHead {
			t.Errorf("exporter sent %s to the registry", method)
		}
	}
}

// pushImage publishes a single-platform image and returns its digest.
func (r *testRegistry) pushImage(t *testing.T, repoTag string) string {
	t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	tag, err := name.NewTag(r.host + "/" + repoTag)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()
	return d.String()
}

// pushIndex publishes a multi-platform image and returns the index digest
// and the platform manifest digests.
func (r *testRegistry) pushIndex(t *testing.T, repoTag string) (string, []string) {
	t.Helper()
	idx, err := random.Index(64, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	tag, err := name.NewTag(r.host + "/" + repoTag)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(tag, idx); err != nil {
		t.Fatal(err)
	}
	d, _ := idx.Digest()
	m, _ := idx.IndexManifest()
	var children []string
	for _, c := range m.Manifests {
		children = append(children, c.Digest.String())
	}
	return d.String(), children
}

// fakeDockerEngine serves the read-only Docker Engine API endpoints.
type fakeDockerEngine struct {
	mu         sync.Mutex
	containers []map[string]any
	images     map[string]map[string]any
	down       bool
	requests   []string
}

var dockerPath = regexp.MustCompile(`^(?:/v[0-9.]+)?(/.*)$`)

func (f *fakeDockerEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := dockerPath.FindStringSubmatch(r.URL.Path)[1]
	f.requests = append(f.requests, r.Method+" "+path)
	w.Header().Set("Api-Version", "1.47")
	w.Header().Set("Content-Type", "application/json")
	if f.down {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"message":"daemon unavailable"}`)
		return
	}
	switch {
	case path == "/_ping":
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "OK")
	case path == "/containers/json":
		if r.URL.Query().Get("all") == "1" || r.URL.Query().Get("all") == "true" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(f.containers)
	case strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
		img, ok := f.images[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"no such image"}`)
			return
		}
		json.NewEncoder(w).Encode(img)
	default:
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"not found"}`)
	}
}

func (f *fakeDockerEngine) add(id, containerName, image, imageID, state string, repoDigests ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers = append(f.containers, map[string]any{
		"Id": id, "Names": []string{"/" + containerName}, "Image": image, "ImageID": imageID, "State": state,
	})
	if f.images == nil {
		f.images = map[string]map[string]any{}
	}
	f.images[imageID] = map[string]any{"Id": imageID, "RepoDigests": repoDigests, "Os": "linux", "Architecture": "amd64"}
}

func scrape(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// samples returns the non-comment lines of an exposition.
func samples(body string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

func assertSamples(t *testing.T, body string, want []string) {
	t.Helper()
	got := samples(body)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("samples:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func newScanner(disc discovery.Discoverer) *scanner.Scanner {
	return scanner.New(disc, registry.New(registry.Options{UserAgent: "integration-test"}), scanner.Options{
		ScanInterval:     time.Minute,
		RegistryInterval: time.Hour,
		RegistryTimeout:  10 * time.Second,
		Concurrency:      4,
		Logger:           quiet,
	})
}

func TestDockerEndToEnd(t *testing.T) {
	reg := newTestRegistry(t)
	for _, tag := range []string{"1.26.2", "1.26.3", "1.27.0", "1.27.0-alpine", "1.28.0-rc.1"} {
		reg.pushImage(t, "library/nginx:"+tag)
	}
	currentLatest := reg.pushImage(t, "team/api:latest")
	oldStable := reg.pushImage(t, "team/worker:stable")
	reg.pushImage(t, "team/worker:stable") // the tag moves to a new image
	indexDigest, platforms := reg.pushIndex(t, "team/multi:latest")
	_ = indexDigest
	reg.pushImage(t, "team/tool:main-mongod8.3")
	reg.pushImage(t, "team/tool:main-mongod8.4")
	reg.pushImage(t, "team/tool:main-operator9.0")

	engine := &fakeDockerEngine{}
	h := reg.host
	engine.add("c1", "web", h+"/library/nginx:1.26.2", "sha256:img1", "running")
	engine.add("c2", "shop-api-1", h+"/team/api", "sha256:img2", "running", h+"/team/api@"+currentLatest)
	engine.add("c3", "worker", h+"/team/worker:stable", "sha256:img3", "running", h+"/team/worker@"+oldStable)
	// The runtime recorded the platform manifest digest, the tag points to an index.
	engine.add("c4", "multi", h+"/team/multi:latest", "sha256:img4", "running", h+"/team/multi@"+platforms[1])
	engine.add("c5", "tool", h+"/team/tool:main-mongod8.3", "sha256:img5", "running")
	engine.add("c6", "hash", h+"/team/api:abc123", "sha256:img6", "running")
	engine.add("c7", "gone", h+"/team/removed:1.0.0", "sha256:img7", "running")
	engine.add("c8", "local", h+"/team/api:latest", "sha256:img8", "running") // no RepoDigests
	engine.add("c9", "stopped", h+"/library/nginx:1.0.0", "sha256:img9", "exited")

	engineSrv := httptest.NewServer(engine)
	defer engineSrv.Close()
	cli, err := docker.NewClient("tcp://" + strings.TrimPrefix(engineSrv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	scan := newScanner(docker.New(cli, quiet))
	web := httptest.NewServer(server.NewHandler(scan.Snapshot))
	defer web.Close()

	// Startup: no scan yet.
	if code, _ := scrape(t, web.URL+"/metrics"); code != http.StatusServiceUnavailable {
		t.Fatalf("/metrics before the first scan = %d, want 503", code)
	}

	reg.startRecording()
	scan.Cycle(context.Background())
	code, body := scrape(t, web.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics = %d: %s", code, body)
	}

	l := func(metric, container, repo, tags string) string {
		return metric + `{container="` + container + `",` + tags + `namespace="",platform="docker",registry="` + h + `",repository="` + repo + `",workload_kind=""}`
	}
	assertSamples(t, body, []string{
		l("container_image_info", "gone", "team/removed", `current_tag="1.0.0",latest_tag="",`) + " 0",
		l("container_image_info", "hash", "team/api", `current_tag="abc123",latest_tag="",`) + " 0",
		l("container_image_info", "local", "team/api", `current_tag="latest",latest_tag="",`) + " 0",
		l("container_image_info", "multi", "team/multi", `current_tag="latest",latest_tag="latest",`) + " 1",
		l("container_image_info", "shop-api-1", "team/api", `current_tag="latest",latest_tag="latest",`) + " 1",
		l("container_image_info", "tool", "team/tool", `current_tag="main-mongod8.3",latest_tag="main-mongod8.4",`) + " 1",
		l("container_image_info", "web", "library/nginx", `current_tag="1.26.2",latest_tag="1.27.0",`) + " 1",
		l("container_image_info", "worker", "team/worker", `current_tag="stable",latest_tag="stable",`) + " 1",
		l("container_image_update_available", "multi", "team/multi", "") + " 0",
		l("container_image_update_available", "shop-api-1", "team/api", "") + " 0",
		l("container_image_update_available", "tool", "team/tool", "") + " 1",
		l("container_image_update_available", "web", "library/nginx", "") + " 1",
		l("container_image_update_available", "worker", "team/worker", "") + " 1",
	})
	for _, line := range strings.Split(body, "\n") {
		for _, prefix := range []string{"go_", "process_", "promhttp_", "prometheus_"} {
			if strings.HasPrefix(line, prefix) {
				t.Errorf("default metric exposed: %s", line)
			}
		}
	}

	// Read-only towards Docker and the registry.
	engine.mu.Lock()
	for _, req := range engine.requests {
		if !strings.HasPrefix(req, "GET ") && !strings.HasPrefix(req, "HEAD ") {
			t.Errorf("exporter sent a non read-only Docker request: %s", req)
		}
	}
	engine.mu.Unlock()
	reg.assertReadOnly(t)

	// The daemon goes away: the scrape keeps working from the snapshot.
	engine.mu.Lock()
	engine.down = true
	engine.mu.Unlock()
	scan.Cycle(context.Background())
	if code, body2 := scrape(t, web.URL+"/metrics"); code != http.StatusOK || len(samples(body2)) != len(samples(body)) {
		t.Errorf("short daemon outage changed the metrics: %d\n%s", code, body2)
	}
}

func TestKubernetesEndToEnd(t *testing.T) {
	reg := newTestRegistry(t)
	h := reg.host
	for _, tag := range []string{"v1.4.2", "v1.5.0", "v1.5.0-debug"} {
		reg.pushImage(t, "org/api:"+tag)
	}
	_, platforms := reg.pushIndex(t, "org/proxy:stable")
	reg.pushImage(t, "addons/coredns:v1.14.6-eksbuild.4")
	reg.pushImage(t, "addons/coredns:v1.14.6-eksbuild.9")
	reg.pushImage(t, "addons/coredns:v1.14.7")

	isController := true
	owned := func(apiVersion, kind, ownerName string) []metav1.OwnerReference {
		return []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: ownerName, Controller: &isController}}
	}
	newPod := func(ns, podName string, owners []metav1.OwnerReference, containers ...[3]string) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: podName, OwnerReferences: owners},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}
		for _, c := range containers {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: c[0], Image: c[1]})
			p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{Name: c[0], Image: c[1], ImageID: c[2]})
		}
		return p
	}
	someDigest := "sha256:" + strings.Repeat("ab", 32)

	core := fake.NewClientset(
		// Deployment mid-rollout: two old replicas, one new.
		newPod("prod", "api-old-1", owned("apps/v1", "ReplicaSet", "api-old"), [3]string{"api", h + "/org/api:v1.4.2", h + "/org/api@" + someDigest}),
		newPod("prod", "api-old-2", owned("apps/v1", "ReplicaSet", "api-old"), [3]string{"api", h + "/org/api:v1.4.2", h + "/org/api@" + someDigest}),
		newPod("prod", "api-new-1", owned("apps/v1", "ReplicaSet", "api-new"), [3]string{"api", h + "/org/api:v1.5.0", h + "/org/api@" + someDigest}),
		// DaemonSet on a floating tag; the kubelet reports the platform manifest digest.
		newPod("prod", "proxy-a", owned("apps/v1", "DaemonSet", "proxy"), [3]string{"proxy", h + "/org/proxy:stable", h + "/org/proxy@" + platforms[0]}),
		newPod("prod", "proxy-b", owned("apps/v1", "DaemonSet", "proxy"), [3]string{"proxy", h + "/org/proxy:stable", h + "/org/proxy@" + platforms[0]}),
		newPod("kube-system", "coredns-1", owned("apps/v1", "ReplicaSet", "coredns-abc"), [3]string{"coredns", h + "/addons/coredns:v1.14.6-eksbuild.4", h + "/addons/coredns@" + someDigest}),
	)
	scheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	rs := func(ns, rsName, deployment string) *metav1.PartialObjectMetadata {
		return &metav1.PartialObjectMetadata{
			TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "ReplicaSet"},
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: rsName, OwnerReferences: owned("apps/v1", "Deployment", deployment)},
		}
	}
	meta := metadatafake.NewSimpleMetadataClient(scheme,
		rs("prod", "api-old", "api"), rs("prod", "api-new", "api"), rs("kube-system", "coredns-abc", "coredns"))

	scan := newScanner(kubernetes.New(core, meta, false, quiet))
	web := httptest.NewServer(server.NewHandler(scan.Snapshot))
	defer web.Close()

	reg.startRecording()
	scan.Cycle(context.Background())
	code, body := scrape(t, web.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics = %d: %s", code, body)
	}

	l := func(metric, ns, kind, workload, repo, tags string) string {
		return metric + `{container="` + workload + `",` + tags + `namespace="` + ns + `",platform="kubernetes",registry="` + h + `",repository="` + repo + `",workload_kind="` + kind + `"}`
	}
	assertSamples(t, body, []string{
		// One series per distinct running tag of the Deployment, not per pod.
		l("container_image_info", "prod", "Deployment", "api", "org/api", `current_tag="v1.4.2",latest_tag="v1.5.0",`) + " 1",
		l("container_image_info", "prod", "Deployment", "api", "org/api", `current_tag="v1.5.0",latest_tag="v1.5.0",`) + " 1",
		l("container_image_info", "kube-system", "Deployment", "coredns", "addons/coredns", `current_tag="v1.14.6-eksbuild.4",latest_tag="v1.14.6-eksbuild.9",`) + " 1",
		l("container_image_info", "prod", "DaemonSet", "proxy", "org/proxy", `current_tag="stable",latest_tag="stable",`) + " 1",
		// One update series per workload and repository.
		l("container_image_update_available", "prod", "Deployment", "api", "org/api", "") + " 1",
		l("container_image_update_available", "kube-system", "Deployment", "coredns", "addons/coredns", "") + " 1",
		l("container_image_update_available", "prod", "DaemonSet", "proxy", "org/proxy", "") + " 0",
	})
	reg.assertReadOnly(t)

	for _, a := range append(core.Actions(), meta.Actions()...) {
		if a.GetVerb() != "list" {
			t.Errorf("non read-only Kubernetes request: %s %s", a.GetVerb(), a.GetResource().Resource)
		}
	}
}
