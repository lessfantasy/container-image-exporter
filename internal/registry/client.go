// Package registry talks to OCI and Docker v2 registries.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/container-image-exporter/container-image-exporter/internal/image"
)

// Manifest describes what a registry publishes under a tag.
type Manifest struct {
	// Digest of the manifest or image index the tag points to.
	Digest string
	// IsIndex is true for multi-platform images.
	IsIndex bool
	// Platforms lists the digests of the manifests inside an image index.
	// It is only filled by Client.Manifest.
	Platforms []string
}

// Client is a read-only registry client. It is safe for concurrent use.
type Client struct {
	keychain authn.Keychain
	options  []remote.Option
}

// Options configure a Client.
type Options struct {
	// Keychain resolves credentials. Nil means anonymous access.
	Keychain authn.Keychain
	// UserAgent is sent with every request.
	UserAgent string
	// PageSize is the tag list page size. Zero uses the library default.
	PageSize int
	// Transport overrides the HTTP transport (tests).
	Transport http.RoundTripper
}

// New creates a Client.
func New(o Options) *Client {
	if o.Keychain == nil {
		o.Keychain = authn.NewMultiKeychain()
	}
	opts := []remote.Option{
		// Bounded exponential backoff with jitter for transient failures.
		remote.WithRetryBackoff(remote.Backoff{
			Duration: 500 * time.Millisecond,
			Factor:   2,
			Jitter:   0.3,
			Steps:    3,
		}),
		// 429 is deliberately not retried here: hammering a rate-limited
		// registry makes it worse. The scanner backs off per registry.
		remote.WithRetryStatusCodes(
			http.StatusRequestTimeout,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout,
		),
		remote.WithRetryPredicate(retryable),
	}
	if o.UserAgent != "" {
		opts = append(opts, remote.WithUserAgent(o.UserAgent))
	}
	if o.PageSize > 0 {
		opts = append(opts, remote.WithPageSize(o.PageSize))
	}
	if o.Transport != nil {
		opts = append(opts, remote.WithTransport(o.Transport))
	}
	return &Client{keychain: o.Keychain, options: opts}
}

func (c *Client) opts(ctx context.Context, extra authn.Keychain) []remote.Option {
	kc := c.keychain
	if extra != nil {
		kc = authn.NewMultiKeychain(extra, c.keychain)
	}
	return append([]remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(kc)}, c.options...)
}

// ListTags returns all tags of the repository, following pagination.
func (c *Client) ListTags(ctx context.Context, ref image.Ref, extra authn.Keychain) ([]string, error) {
	tags, err := remote.List(ref.Name(), c.opts(ctx, extra)...)
	if err != nil {
		return nil, fmt.Errorf("list tags of %s: %w", ref.RepositoryKey(), err)
	}
	return tags, nil
}

// Digest returns the digest the tag points to using a HEAD request, which
// does not count against Docker Hub's pull rate limit. It falls back to
// GET for registries without HEAD support.
func (c *Client) Digest(ctx context.Context, ref image.Ref, extra authn.Keychain) (Manifest, error) {
	desc, err := remote.Head(ref.TagRef(), c.opts(ctx, extra)...)
	if err != nil {
		// Only a registry that rejects HEAD itself (400, 405, 501, ...) is
		// worth a second attempt with GET. Network errors, 5xx, 401/403,
		// 404 and 429 would fail the same way again.
		code := statusCode(err)
		headUnsupported := code >= 400 && code < 500 && !IsRateLimited(err) && !IsUnauthorized(err) && !IsNotFound(err)
		if code == http.StatusNotImplemented {
			headUnsupported = true
		}
		if !headUnsupported || ctx.Err() != nil {
			return Manifest{}, fmt.Errorf("resolve digest of %s: %w", ref, err)
		}
		return c.Manifest(ctx, ref, extra)
	}
	return Manifest{Digest: desc.Digest.String(), IsIndex: desc.MediaType.IsIndex()}, nil
}

// Manifest fetches the manifest the tag points to and, for an image index,
// the digests of its platform manifests.
func (c *Client) Manifest(ctx context.Context, ref image.Ref, extra authn.Keychain) (Manifest, error) {
	desc, err := remote.Get(ref.TagRef(), c.opts(ctx, extra)...)
	if err != nil {
		return Manifest{}, fmt.Errorf("fetch manifest of %s: %w", ref, err)
	}
	m := Manifest{Digest: desc.Digest.String(), IsIndex: desc.MediaType.IsIndex()}
	if !m.IsIndex {
		return m, nil
	}
	var index struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(desc.Manifest, &index); err != nil {
		return Manifest{}, fmt.Errorf("decode image index of %s: %w", ref, err)
	}
	for _, child := range index.Manifests {
		if child.Digest != "" {
			m.Platforms = append(m.Platforms, child.Digest)
		}
	}
	return m, nil
}

// retryable reports whether an operation should be retried.
func retryable(err error) bool {
	if IsRateLimited(err) {
		return false
	}
	var terr *transport.Error
	if errors.As(err, &terr) {
		return terr.Temporary()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED)
}

func statusCode(err error) int {
	var terr *transport.Error
	if errors.As(err, &terr) {
		return terr.StatusCode
	}
	return 0
}

// IsRateLimited reports whether the registry answered 429.
func IsRateLimited(err error) bool { return statusCode(err) == http.StatusTooManyRequests }

// IsUnauthorized reports whether the registry rejected the credentials.
func IsUnauthorized(err error) bool {
	code := statusCode(err)
	return code == http.StatusUnauthorized || code == http.StatusForbidden
}

// IsNotFound reports whether the repository or tag does not exist.
func IsNotFound(err error) bool { return statusCode(err) == http.StatusNotFound }

// Reason maps an error to a short, credential-free description for logs.
func Reason(err error) string {
	switch {
	case err == nil:
		return ""
	case IsRateLimited(err):
		return "rate limited"
	case IsUnauthorized(err):
		return "authentication failed"
	case IsNotFound(err):
		return "repository or tag not found"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case statusCode(err) >= 500:
		return "registry unavailable"
	default:
		return "registry request failed"
	}
}
