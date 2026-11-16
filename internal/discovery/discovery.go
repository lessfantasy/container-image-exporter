// Package discovery defines the runtime-independent inventory model.
package discovery

import (
	"context"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/container-image-exporter/container-image-exporter/internal/image"
)

// Platform label values.
const (
	PlatformDocker     = "docker"
	PlatformKubernetes = "kubernetes"
)

// Instance is one logical running image: a Docker container, or a
// Kubernetes workload together with one distinct running image.
type Instance struct {
	Platform     string
	Container    string // Docker container name or Kubernetes workload name
	Namespace    string // empty for Docker
	WorkloadKind string // empty for Docker

	// RawImage is the image string as reported by the runtime.
	RawImage string
	// Image is the normalized reference. It is the zero value when
	// ImageErr is set.
	Image image.Ref
	// ImageErr explains why RawImage could not be normalized.
	ImageErr error

	// Digests are the registry manifest digests known for the running
	// image. Empty when the runtime does not expose one.
	Digests []string

	// Keychain holds workload-specific registry credentials (Kubernetes
	// imagePullSecrets). It may be nil.
	Keychain authn.Keychain
}

// Discoverer lists the images that are running right now. An error means
// the inventory is unknown; it must never be reported as an empty inventory.
type Discoverer interface {
	Discover(ctx context.Context) ([]Instance, error)
}
