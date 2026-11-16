// Package docker discovers running containers through the Docker Engine
// API. Containers started by "docker run" and by Docker Compose are
// handled identically. Only read (GET) endpoints are used.
package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/container-image-exporter/container-image-exporter/internal/discovery"
	"github.com/container-image-exporter/container-image-exporter/internal/image"
)

// API is the read-only subset of the Docker Engine API that is used.
// *client.Client implements it.
type API interface {
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ImageInspect(ctx context.Context, imageID string, options ...client.ImageInspectOption) (client.ImageInspectResult, error)
}

// NewClient connects to the Docker Engine. An empty host uses DOCKER_HOST
// or the default Unix socket.
func NewClient(host string) (*client.Client, error) {
	opts := []client.Opt{client.FromEnv}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	return client.New(opts...)
}

// Discoverer lists running Docker containers.
type Discoverer struct {
	api    API
	logger *slog.Logger
}

// New creates a Discoverer.
func New(api API, logger *slog.Logger) *Discoverer {
	return &Discoverer{api: api, logger: logger}
}

// Discover implements discovery.Discoverer.
func (d *Discoverer) Discover(ctx context.Context) ([]discovery.Instance, error) {
	// All=false: the daemon returns only containers that are not stopped.
	list, err := d.api.ContainerList(ctx, client.ContainerListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list docker containers: %w", err)
	}

	repoDigests := map[string][]string{} // image ID -> RepoDigests
	instances := make([]discovery.Instance, 0, len(list.Items))
	for _, c := range list.Items {
		if c.State != container.StateRunning {
			continue
		}
		inst := discovery.Instance{
			Platform:  discovery.PlatformDocker,
			Container: containerName(c),
			RawImage:  c.Image,
		}

		// The list shows an image ID instead of a name when the tag was
		// moved to another image. The container config keeps the name.
		if isImageID(c.Image, c.ImageID) {
			if name, err := d.configuredImage(ctx, c.ID); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				d.logger.Warn("inspect container failed", "container", inst.Container, "error", err)
			} else if name != "" {
				inst.RawImage = name
			}
		}

		ref, err := image.Parse(inst.RawImage)
		if err != nil {
			inst.ImageErr = err
			instances = append(instances, inst)
			continue
		}
		inst.Image = ref

		digests, ok := repoDigests[c.ImageID]
		if !ok && c.ImageID != "" {
			res, err := d.api.ImageInspect(ctx, c.ImageID)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				d.logger.Warn("inspect image failed", "container", inst.Container, "image", ref.String(), "error", err)
			} else {
				digests = res.RepoDigests
			}
			repoDigests[c.ImageID] = digests
		}
		inst.Digests = digestsFor(ref, digests)
		instances = append(instances, inst)
	}

	sort.SliceStable(instances, func(i, j int) bool { return instances[i].Container < instances[j].Container })
	return instances, nil
}

func (d *Discoverer) configuredImage(ctx context.Context, id string) (string, error) {
	res, err := d.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return "", err
	}
	if res.Container.Config == nil {
		return "", errors.New("container has no config")
	}
	return res.Container.Config.Image, nil
}

// digestsFor returns the registry digests recorded for ref's repository,
// plus the digest ref is pinned to.
func digestsFor(ref image.Ref, repoDigests []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(d string) {
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	add(ref.Digest)
	for _, rd := range repoDigests {
		parsed, err := image.Parse(rd)
		if err != nil || parsed.RepositoryKey() != ref.RepositoryKey() {
			continue
		}
		add(parsed.Digest)
	}
	sort.Strings(out)
	return out
}

func containerName(c container.Summary) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	if len(c.ID) > 12 {
		return c.ID[:12]
	}
	return c.ID
}

// isImageID reports whether img is an image ID (full, or an abbreviation
// of imageID) rather than an image name.
func isImageID(img, imageID string) bool {
	if strings.HasPrefix(img, "sha256:") {
		return true
	}
	hex := strings.TrimPrefix(imageID, "sha256:")
	return len(img) >= 12 && hex != "" && strings.HasPrefix(hex, img)
}
