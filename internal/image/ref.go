// Package image parses and normalizes container image references.
package image

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

// DockerHubRegistry is the label value used for Docker Hub images.
const DockerHubRegistry = "docker.io"

// ErrNoReference is returned when a string is an image ID rather than a
// pullable reference.
var ErrNoReference = errors.New("not a pullable image reference")

// Ref is a normalized image reference.
type Ref struct {
	// Registry is the registry host as exposed in metric labels
	// (Docker Hub is always "docker.io").
	Registry string
	// Repository is the repository path without the registry host.
	Repository string
	// Tag is the image tag. It is empty when the image is referenced by
	// digest only.
	Tag string
	// Digest is the digest the reference is pinned to, if any.
	Digest string

	repo name.Repository
}

// Parse normalizes s using the Docker reference rules: a missing registry
// means Docker Hub, single-segment Docker Hub repositories live under
// "library/", and a missing tag means "latest" unless the reference is
// pinned by digest only.
func Parse(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Ref{}, fmt.Errorf("empty image reference: %w", ErrNoReference)
	}
	if isImageID(s) {
		return Ref{}, fmt.Errorf("%q is an image ID: %w", s, ErrNoReference)
	}

	rest, digest, pinned := strings.Cut(s, "@")
	if pinned {
		if _, err := name.NewDigest(s); err != nil {
			return Ref{}, fmt.Errorf("parse image reference %q: %w", s, err)
		}
	}

	lastSegment := rest[strings.LastIndex(rest, "/")+1:]
	hasTag := strings.Contains(lastSegment, ":")

	tag, err := name.NewTag(rest)
	if err != nil {
		return Ref{}, fmt.Errorf("parse image reference %q: %w", s, err)
	}

	ref := Ref{
		Registry:   tag.RegistryStr(),
		Repository: tag.RepositoryStr(),
		Digest:     digest,
		repo:       tag.Repository,
	}
	if ref.Registry == name.DefaultRegistry {
		ref.Registry = DockerHubRegistry
	}
	if hasTag || !pinned {
		ref.Tag = tag.TagStr()
	}
	return ref, nil
}

// Name returns the go-containerregistry repository for registry calls.
func (r Ref) Name() name.Repository { return r.repo }

// TagRef returns the registry reference for the tag.
func (r Ref) TagRef() name.Tag { return r.repo.Tag(r.Tag) }

// RepositoryKey identifies the repository across registries.
func (r Ref) RepositoryKey() string { return r.Registry + "/" + r.Repository }

// String returns the normalized reference without the digest.
func (r Ref) String() string {
	if r.Tag == "" {
		return r.RepositoryKey()
	}
	return r.RepositoryKey() + ":" + r.Tag
}

// DigestOf extracts the digest from strings such as
// "docker.io/library/nginx@sha256:..." or
// "docker-pullable://nginx@sha256:...". It returns "" when s carries no
// registry manifest digest (for example a bare image ID).
func DigestOf(s string) string {
	_, digest, ok := strings.Cut(s, "@")
	if !ok || !validDigest(digest) {
		return ""
	}
	return digest
}

func validDigest(d string) bool {
	hex, ok := strings.CutPrefix(d, "sha256:")
	return ok && len(hex) == 64 && isHex(hex)
}

func isImageID(s string) bool {
	if hex, ok := strings.CutPrefix(s, "sha256:"); ok {
		return isHex(hex)
	}
	// A full 64 character hex string is an image ID, never a repository.
	return len(s) == 64 && isHex(s)
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
