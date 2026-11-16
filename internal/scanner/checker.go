package scanner

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/container-image-exporter/container-image-exporter/internal/cache"
	"github.com/container-image-exporter/container-image-exporter/internal/image"
	"github.com/container-image-exporter/container-image-exporter/internal/registry"
	"github.com/container-image-exporter/container-image-exporter/internal/version"
	"github.com/container-image-exporter/container-image-exporter/internal/version/floating"
	"github.com/container-image-exporter/container-image-exporter/internal/version/semver"
)

// Registry is the registry access needed for version checks.
// *registry.Client implements it.
type Registry interface {
	ListTags(ctx context.Context, ref image.Ref, keychain authn.Keychain) ([]string, error)
	Digest(ctx context.Context, ref image.Ref, keychain authn.Keychain) (registry.Manifest, error)
	Manifest(ctx context.Context, ref image.Ref, keychain authn.Keychain) (registry.Manifest, error)
}

// target is one deduplicated registry lookup.
type target struct {
	key       string
	ref       image.Ref
	detection version.Detection
	digests   []string // floating only
	keychain  authn.Keychain
}

// targetKey identifies a registry lookup. SemVer lookups depend only on
// the reference; floating lookups also depend on the running digests.
func targetKey(ref image.Ref, det version.Detection, digests []string) string {
	if det.Strategy == version.Floating {
		sorted := append([]string(nil), digests...)
		sort.Strings(sorted)
		return "floating|" + ref.String() + "|" + strings.Join(sorted, ",")
	}
	return "semver|" + ref.String()
}

// tagLists memoizes tag listings for one scan cycle so that several tags
// of one repository cause a single listing.
type tagLists struct {
	reg Registry
	mu  sync.Mutex
	m   map[string]*tagList
}

type tagList struct {
	once sync.Once
	tags []string
	err  error
}

func newTagLists(reg Registry) *tagLists {
	return &tagLists{reg: reg, m: map[string]*tagList{}}
}

func (t *tagLists) get(ctx context.Context, ref image.Ref, keychain authn.Keychain) ([]string, error) {
	t.mu.Lock()
	entry, ok := t.m[ref.RepositoryKey()]
	if !ok {
		entry = &tagList{}
		t.m[ref.RepositoryKey()] = entry
	}
	t.mu.Unlock()
	entry.once.Do(func() {
		entry.tags, entry.err = t.reg.ListTags(ctx, ref, keychain)
	})
	return entry.tags, entry.err
}

// check performs the registry lookup for one target. The returned error is
// the underlying registry error, if any.
func check(ctx context.Context, reg Registry, tags *tagLists, t target) (cache.Result, error) {
	switch t.detection.Strategy {
	case version.SemVer:
		return checkSemVer(ctx, tags, t)
	case version.Floating:
		return checkFloating(ctx, reg, t)
	default:
		return cache.Result{Reason: "unsupported tag: " + t.detection.Reason}, nil
	}
}

func checkSemVer(ctx context.Context, tags *tagLists, t target) (cache.Result, error) {
	all, err := tags.get(ctx, t.ref, t.keychain)
	if err != nil {
		return cache.Result{Reason: registry.Reason(err)}, err
	}
	latest, newer := semver.Latest(t.detection.SemVer, all)
	return cache.Result{OK: true, LatestTag: latest, UpdateAvailable: newer}, nil
}

func checkFloating(ctx context.Context, reg Registry, t target) (cache.Result, error) {
	// Without a running digest no registry request can help.
	if _, err := floating.Compare(t.digests, floating.Remote{Digest: "unknown"}); err != nil {
		return cache.Result{Reason: err.Error()}, nil
	}

	// HEAD first: cheap and exempt from Docker Hub's pull rate limit.
	manifest, err := reg.Digest(ctx, t.ref, t.keychain)
	if err != nil {
		return cache.Result{Reason: registry.Reason(err)}, err
	}
	update, err := floating.Compare(t.digests, floating.Remote{Digest: manifest.Digest})
	if err != nil {
		return cache.Result{Reason: err.Error()}, nil
	}

	// The tag digest differs. For a multi-platform image the runtime may
	// have reported the platform manifest digest instead of the index
	// digest, so compare against the platform manifests as well.
	if update && manifest.IsIndex {
		if manifest.Platforms == nil {
			if manifest, err = reg.Manifest(ctx, t.ref, t.keychain); err != nil {
				return cache.Result{Reason: registry.Reason(err)}, err
			}
		}
		update, err = floating.Compare(t.digests, floating.Remote{Digest: manifest.Digest, Platforms: manifest.Platforms})
		if err != nil {
			return cache.Result{Reason: err.Error()}, nil
		}
	}
	return cache.Result{OK: true, LatestTag: t.ref.Tag, UpdateAvailable: update}, nil
}
