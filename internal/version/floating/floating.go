// Package floating decides whether a moving tag such as "latest" points to
// a different image than the one that is running, by comparing digests.
package floating

import "errors"

// Errors returned by Compare.
var (
	ErrNoRunningDigest = errors.New("runtime does not expose a registry digest for the running image")
	ErrNoRemoteDigest  = errors.New("registry returned no digest for the tag")
)

// Remote describes what the registry currently publishes under a tag.
type Remote struct {
	// Digest is the digest of the manifest the tag points to. For a
	// multi-platform image this is the image index digest.
	Digest string
	// Platforms holds the digests of the platform-specific manifests of an
	// image index. Empty for single-platform images.
	Platforms []string
}

// Compare reports whether an update is available.
//
// Runtimes report either the image index digest or the digest of the
// platform-specific manifest, depending on the runtime and how the image
// was pulled. Both representations are accepted: the running image is up
// to date when one of its digests equals the tag's digest or the digest
// of one platform manifest inside the tag's image index. Digests of
// different representations can never be equal by accident, so a match is
// always a true match.
func Compare(running []string, remote Remote) (bool, error) {
	if remote.Digest == "" {
		return false, ErrNoRemoteDigest
	}
	known := false
	for _, d := range running {
		if d == "" {
			continue
		}
		known = true
		if d == remote.Digest {
			return false, nil
		}
		for _, p := range remote.Platforms {
			if d == p {
				return false, nil
			}
		}
	}
	if !known {
		return false, ErrNoRunningDigest
	}
	return true, nil
}
