package docker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/client"

	"github.com/container-image-exporter/container-image-exporter/internal/discovery"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	imageID = "sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
)

// fakeAPI is a mocked Docker Engine API.
type fakeAPI struct {
	containers   []container.Summary
	configImages map[string]string   // container ID -> Config.Image
	repoDigests  map[string][]string // image ID -> RepoDigests
	listErr      error
	listOptions  []client.ContainerListOptions
	imageCalls   map[string]int
}

func (f *fakeAPI) ContainerList(_ context.Context, o client.ContainerListOptions) (client.ContainerListResult, error) {
	f.listOptions = append(f.listOptions, o)
	if f.listErr != nil {
		return client.ContainerListResult{}, f.listErr
	}
	return client.ContainerListResult{Items: f.containers}, nil
}

func (f *fakeAPI) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	img, ok := f.configImages[id]
	if !ok {
		return client.ContainerInspectResult{}, errors.New("no such container")
	}
	return client.ContainerInspectResult{Container: container.InspectResponse{Config: &container.Config{Image: img}}}, nil
}

func (f *fakeAPI) ImageInspect(_ context.Context, id string, _ ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	if f.imageCalls == nil {
		f.imageCalls = map[string]int{}
	}
	f.imageCalls[id]++
	digests, ok := f.repoDigests[id]
	if !ok {
		return client.ImageInspectResult{}, errors.New("no such image")
	}
	return client.ImageInspectResult{InspectResponse: image.InspectResponse{RepoDigests: digests}}, nil
}

func discover(t *testing.T, api *fakeAPI) []discovery.Instance {
	t.Helper()
	got, err := New(api, slog.New(slog.NewTextHandler(io.Discard, nil))).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func running(id, name, img, imgID string) container.Summary {
	return container.Summary{ID: id, Names: []string{"/" + name}, Image: img, ImageID: imgID, State: container.StateRunning}
}

func TestDiscover(t *testing.T) {
	tests := []struct {
		name string
		api  *fakeAPI
		want []discovery.Instance // Image compared through registry/repository/tag
		refs [][3]string
	}{
		{
			name: "docker run container",
			api: &fakeAPI{
				containers:  []container.Summary{running("c1", "web", "nginx:1.26.2", "sha256:i1")},
				repoDigests: map[string][]string{"sha256:i1": {"nginx@" + digestA}},
			},
			want: []discovery.Instance{{Platform: "docker", Container: "web", RawImage: "nginx:1.26.2", Digests: []string{digestA}}},
			refs: [][3]string{{"docker.io", "library/nginx", "1.26.2"}},
		},
		{
			name: "docker compose container needs no labels",
			api: &fakeAPI{
				containers: []container.Summary{{
					ID: "c1", Names: []string{"/shop-db-1"}, Image: "postgres:16.4-alpine", ImageID: "sha256:i1",
					State:  container.StateRunning,
					Labels: map[string]string{"com.docker.compose.project": "shop", "com.docker.compose.service": "db"},
				}},
				repoDigests: map[string][]string{"sha256:i1": {"postgres@" + digestA}},
			},
			want: []discovery.Instance{{Platform: "docker", Container: "shop-db-1", RawImage: "postgres:16.4-alpine", Digests: []string{digestA}}},
			refs: [][3]string{{"docker.io", "library/postgres", "16.4-alpine"}},
		},
		{
			name: "container without an explicit tag",
			api: &fakeAPI{
				containers:  []container.Summary{running("c1", "cache", "redis", "sha256:i1")},
				repoDigests: map[string][]string{"sha256:i1": {"redis@" + digestA}},
			},
			want: []discovery.Instance{{Platform: "docker", Container: "cache", RawImage: "redis", Digests: []string{digestA}}},
			refs: [][3]string{{"docker.io", "library/redis", "latest"}},
		},
		{
			name: "private registry image",
			api: &fakeAPI{
				containers:  []container.Summary{running("c1", "api", "registry.example.com:5000/team/api:v2.3.1", "sha256:i1")},
				repoDigests: map[string][]string{"sha256:i1": {"registry.example.com:5000/team/api@" + digestA}},
			},
			want: []discovery.Instance{{Platform: "docker", Container: "api", RawImage: "registry.example.com:5000/team/api:v2.3.1", Digests: []string{digestA}}},
			refs: [][3]string{{"registry.example.com:5000", "team/api", "v2.3.1"}},
		},
		{
			name: "stopped containers are ignored",
			api: &fakeAPI{
				containers: []container.Summary{
					running("c1", "web", "nginx:1.26.2", "sha256:i1"),
					{ID: "c2", Names: []string{"/old"}, Image: "nginx:1.20.0", ImageID: "sha256:i2", State: container.StateExited},
					{ID: "c3", Names: []string{"/new"}, Image: "nginx:1.27.0", ImageID: "sha256:i3", State: container.StateCreated},
					{ID: "c4", Names: []string{"/frozen"}, Image: "nginx:1.27.0", ImageID: "sha256:i3", State: container.StatePaused},
				},
				repoDigests: map[string][]string{"sha256:i1": {"nginx@" + digestA}},
			},
			want: []discovery.Instance{{Platform: "docker", Container: "web", RawImage: "nginx:1.26.2", Digests: []string{digestA}}},
			refs: [][3]string{{"docker.io", "library/nginx", "1.26.2"}},
		},
		{
			name: "digests of other repositories are not used",
			api: &fakeAPI{
				containers:  []container.Summary{running("c1", "web", "ghcr.io/org/app:latest", "sha256:i1")},
				repoDigests: map[string][]string{"sha256:i1": {"mirror.example.com/org/app@" + digestB, "ghcr.io/org/app@" + digestA}},
			},
			want: []discovery.Instance{{Platform: "docker", Container: "web", RawImage: "ghcr.io/org/app:latest", Digests: []string{digestA}}},
			refs: [][3]string{{"ghcr.io", "org/app", "latest"}},
		},
		{
			name: "locally built image has no registry digest",
			api: &fakeAPI{
				containers:  []container.Summary{running("c1", "dev", "myapp:latest", "sha256:i1")},
				repoDigests: map[string][]string{"sha256:i1": nil},
			},
			want: []discovery.Instance{{Platform: "docker", Container: "dev", RawImage: "myapp:latest"}},
			refs: [][3]string{{"docker.io", "library/myapp", "latest"}},
		},
		{
			name: "image inspect failure keeps the container",
			api: &fakeAPI{
				containers: []container.Summary{running("c1", "web", "nginx:1.26.2", "sha256:gone")},
			},
			want: []discovery.Instance{{Platform: "docker", Container: "web", RawImage: "nginx:1.26.2"}},
			refs: [][3]string{{"docker.io", "library/nginx", "1.26.2"}},
		},
		{
			name: "pinned digest is a running digest",
			api: &fakeAPI{
				containers:  []container.Summary{running("c1", "web", "nginx:stable@"+digestB, "sha256:i1")},
				repoDigests: map[string][]string{"sha256:i1": {"nginx@" + digestB}},
			},
			want: []discovery.Instance{{Platform: "docker", Container: "web", RawImage: "nginx:stable@" + digestB, Digests: []string{digestB}}},
			refs: [][3]string{{"docker.io", "library/nginx", "stable"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := discover(t, tt.api)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d instances, want %d: %+v", len(got), len(tt.want), got)
			}
			for i, inst := range got {
				ref := [3]string{inst.Image.Registry, inst.Image.Repository, inst.Image.Tag}
				if ref != tt.refs[i] {
					t.Errorf("instance %d ref = %v, want %v", i, ref, tt.refs[i])
				}
				if inst.Namespace != "" || inst.WorkloadKind != "" {
					t.Errorf("docker instance has kubernetes labels: %+v", inst)
				}
				inst.Image = tt.want[i].Image
				if !reflect.DeepEqual(inst, tt.want[i]) {
					t.Errorf("instance %d = %+v, want %+v", i, inst, tt.want[i])
				}
			}
			for _, o := range tt.api.listOptions {
				if o.All {
					t.Error("ContainerList called with All=true")
				}
			}
		})
	}
}

func TestMultipleContainersShareOneImageInspect(t *testing.T) {
	api := &fakeAPI{
		containers: []container.Summary{
			running("c1", "web-2", "nginx:1.26.2", "sha256:i1"),
			running("c2", "web-1", "nginx:1.26.2", "sha256:i1"),
			running("c3", "web-3", "nginx:1.26.2", "sha256:i1"),
		},
		repoDigests: map[string][]string{"sha256:i1": {"nginx@" + digestA}},
	}
	got := discover(t, api)
	if len(got) != 3 {
		t.Fatalf("got %d instances, want one per container", len(got))
	}
	for i, want := range []string{"web-1", "web-2", "web-3"} {
		if got[i].Container != want {
			t.Errorf("instance %d = %q, want %q (sorted)", i, got[i].Container, want)
		}
	}
	if api.imageCalls["sha256:i1"] != 1 {
		t.Errorf("image inspected %d times, want 1", api.imageCalls["sha256:i1"])
	}
}

func TestUntaggedImageUsesContainerConfig(t *testing.T) {
	// After "docker pull nginx:1.26.2" replaced the tag, the list shows
	// the image ID. The original reference comes from the container config.
	for _, shown := range []string{imageID, "1234567890ab"} {
		api := &fakeAPI{
			containers:   []container.Summary{running("c1", "web", shown, imageID)},
			configImages: map[string]string{"c1": "nginx:1.26.2"},
			repoDigests:  map[string][]string{imageID: {"nginx@" + digestA}},
		}
		got := discover(t, api)
		if len(got) != 1 || got[0].Image.Tag != "1.26.2" || got[0].Image.Repository != "library/nginx" {
			t.Errorf("shown=%q: got %+v", shown, got)
		}
	}
}

func TestUnresolvableImage(t *testing.T) {
	api := &fakeAPI{
		containers:   []container.Summary{running("c1", "mystery", imageID, imageID)},
		configImages: map[string]string{"c1": imageID}, // started by ID
	}
	got := discover(t, api)
	if len(got) != 1 || got[0].ImageErr == nil || got[0].RawImage != imageID {
		t.Errorf("got %+v, want an instance with ImageErr", got)
	}
}

func TestContainerWithoutName(t *testing.T) {
	api := &fakeAPI{containers: []container.Summary{{ID: "0123456789abcdef0123", Image: "nginx", State: container.StateRunning}}}
	got := discover(t, api)
	if got[0].Container != "0123456789ab" {
		t.Errorf("Container = %q", got[0].Container)
	}
}

func TestDaemonUnavailableIsAnError(t *testing.T) {
	api := &fakeAPI{listErr: errors.New("cannot connect to the Docker daemon")}
	got, err := New(api, slog.New(slog.NewTextHandler(io.Discard, nil))).Discover(context.Background())
	if err == nil || got != nil {
		t.Errorf("got %v, %v; a failed listing must not look like an empty inventory", got, err)
	}
}

// The real SDK client must satisfy the read-only interface.
var _ API = (*client.Client)(nil)
