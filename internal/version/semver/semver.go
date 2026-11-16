// Package semver extracts a semantic version and a tag family from an
// image tag and selects the newest compatible tag from a registry listing.
//
// A tag is split into three parts:
//
//	<prefix><version><suffix>
//	main-mongod 8.3
//	v           1.14.6  -eksbuild.4
//	            1.27.0  -rc.1-alpine3.20
//
// The version is the leftmost run of two or three dot-separated numbers.
// Prefix and suffix describe the tag family. Two tags are compatible only
// when they belong to the same family; see Tag.SameFamily.
package semver

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	mmsemver "github.com/Masterminds/semver/v3"
)

// Errors returned by Parse.
var (
	ErrNoVersion         = errors.New("no version with two or three numeric components")
	ErrTooManyComponents = errors.New("version has more than three numeric components")
	ErrNumberTooLarge    = errors.New("numeric component is too large")
	ErrCommitHash        = errors.New("tag carries a commit hash, newer tags cannot be ordered")
)

var (
	coreRE = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)
	// A suffix is tokenized into delimiters and words.
	tokenRE = regexp.MustCompile(`[-._+]|[^-._+]+`)
	// Pre-release channel, optionally with a glued number: rc, rc1, beta2.
	preRE = regexp.MustCompile(`^(alpha|beta|rc|pre|preview|dev|snapshot|nightly|canary)(\d*)$`)
	// Build counters with a fixed word, e.g. Bitnami "r5" or "build3".
	counterRE = regexp.MustCompile(`^(r|rev|build|b)(\d+)$`)
	digitsRE  = regexp.MustCompile(`^\d+$`)
	hashRE    = regexp.MustCompile(`^g?[0-9a-f]{7,40}$`)
)

// Tag is a parsed SemVer image tag.
type Tag struct {
	// Raw is the tag as published.
	Raw string
	// Prefix is everything before the version, e.g. "v" or "main-mongod".
	Prefix string
	// Version is the extracted version as written, e.g. "1.14.6" or "8.3".
	Version string
	// Components is 2 or 3.
	Components int
	// PreChannel is the pre-release channel ("rc", "beta", ...) or "".
	PreChannel string
	// Family is the suffix with the pre-release part removed and build
	// counters replaced by "#", e.g. "-eksbuild.#" or "-alpine3.#".
	Family string
	// Counters are the build counters found in the suffix, in order.
	Counters []uint64

	core *mmsemver.Version
}

// Parse extracts the version and family from tag.
func Parse(tag string) (Tag, error) {
	loc := coreRE.FindStringSubmatchIndex(tag)
	if loc == nil {
		return Tag{}, ErrNoVersion
	}
	start, end := loc[0], loc[1]
	// A fourth numeric component is outside this project's SemVer policy.
	if rest := tag[end:]; len(rest) >= 2 && rest[0] == '.' && isDigit(rest[1]) {
		return Tag{}, ErrTooManyComponents
	}

	var nums [3]uint64
	components := 2
	for i := 0; i < 3; i++ {
		if loc[2+2*i] < 0 {
			continue
		}
		n, err := strconv.ParseUint(tag[loc[2+2*i]:loc[3+2*i]], 10, 64)
		if err != nil {
			return Tag{}, ErrNumberTooLarge
		}
		nums[i] = n
		if i == 2 {
			components = 3
		}
	}

	t := Tag{
		Raw:        tag,
		Prefix:     tag[:start],
		Version:    tag[start:end],
		Components: components,
	}
	pre, err := t.parseSuffix(tag[end:])
	if err != nil {
		return Tag{}, err
	}
	t.core = mmsemver.New(nums[0], nums[1], nums[2], pre, "")
	return t, nil
}

// parseSuffix fills PreChannel, Family and Counters and returns the
// pre-release string for the SemVer comparison.
func (t *Tag) parseSuffix(suffix string) (string, error) {
	tokens := tokenRE.FindAllString(suffix, -1)
	var (
		family strings.Builder
		pre    string
		i      int
	)

	// Pre-release: the first word of the suffix, e.g. "-rc.1", "-beta2", "rc1".
	first := 0
	if len(tokens) > 0 && isDelimiter(tokens[0]) {
		first = 1
	}
	if first < len(tokens) {
		if m := preRE.FindStringSubmatch(strings.ToLower(tokens[first])); m != nil {
			t.PreChannel = m[1]
			number := m[2]
			i = first + 1
			// "-rc.1": the number is a separate token.
			if number == "" && i+1 < len(tokens) && isDelimiter(tokens[i]) && digitsRE.MatchString(tokens[i+1]) {
				number = tokens[i+1]
				i += 2
			}
			n := uint64(0)
			if number != "" {
				var err error
				if n, err = strconv.ParseUint(number, 10, 64); err != nil {
					return "", ErrNumberTooLarge
				}
			}
			pre = fmt.Sprintf("%s.%d", t.PreChannel, n)
		}
	}

	for ; i < len(tokens); i++ {
		tok := tokens[i]
		lower := strings.ToLower(tok)
		switch {
		case isDelimiter(tok):
			family.WriteString(tok)
		case digitsRE.MatchString(tok):
			n, err := strconv.ParseUint(tok, 10, 64)
			if err != nil {
				return "", ErrNumberTooLarge
			}
			t.Counters = append(t.Counters, n)
			family.WriteString("#")
		case counterRE.MatchString(lower):
			m := counterRE.FindStringSubmatch(lower)
			n, err := strconv.ParseUint(m[2], 10, 64)
			if err != nil {
				return "", ErrNumberTooLarge
			}
			t.Counters = append(t.Counters, n)
			family.WriteString(m[1] + "#")
		case isCommitHash(lower):
			return "", ErrCommitHash
		default:
			family.WriteString(tok)
		}
	}
	t.Family = family.String()
	return pre, nil
}

// SameFamily reports whether candidate may replace t.
//
// Rules:
//   - the prefix is identical (case-sensitive);
//   - the number of version components is identical;
//   - the suffix family is identical once build counters are ignored;
//   - a stable tag only accepts stable candidates;
//   - a pre-release tag accepts its own channel and stable candidates.
func (t Tag) SameFamily(candidate Tag) bool {
	if t.Prefix != candidate.Prefix || t.Components != candidate.Components || t.Family != candidate.Family {
		return false
	}
	if candidate.PreChannel == "" {
		return true
	}
	return candidate.PreChannel == t.PreChannel
}

// Compare orders two tags of the same family: by version, then pre-release
// (a stable version is greater than its pre-releases), then build counters.
// All comparisons are numeric.
func (t Tag) Compare(o Tag) int {
	if c := t.core.Compare(o.core); c != 0 {
		return c
	}
	for i := 0; i < len(t.Counters) && i < len(o.Counters); i++ {
		if t.Counters[i] != o.Counters[i] {
			if t.Counters[i] < o.Counters[i] {
				return -1
			}
			return 1
		}
	}
	return len(t.Counters) - len(o.Counters)
}

// Latest returns the newest tag in candidates that belongs to the family
// of current, and whether it is newer than current. When nothing newer
// exists it returns current.Raw and false. Candidates that do not parse or
// belong to another family are ignored.
func Latest(current Tag, candidates []string) (string, bool) {
	best := current
	for _, raw := range candidates {
		cand, err := Parse(raw)
		if err != nil || !current.SameFamily(cand) {
			continue
		}
		c := cand.Compare(best)
		// Equal ordering with a different spelling: keep the running tag,
		// otherwise the lexicographically smaller one, to stay deterministic.
		if c > 0 || (c == 0 && best.Raw != current.Raw && cand.Raw < best.Raw) {
			best = cand
		}
	}
	return best.Raw, best.Compare(current) > 0
}

func isDelimiter(tok string) bool {
	return len(tok) == 1 && strings.ContainsAny(tok, "-._+")
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isCommitHash reports whether tok looks like an abbreviated or full
// commit hash: 7 to 40 hex characters with at least one digit and one
// letter, optionally prefixed with git-describe's "g".
func isCommitHash(tok string) bool {
	if !hashRE.MatchString(tok) {
		return false
	}
	return strings.ContainsAny(tok, "0123456789") && strings.ContainsAny(tok, "abcdef")
}
