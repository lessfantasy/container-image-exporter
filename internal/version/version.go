// Package version selects the version-detection strategy for an image tag.
package version

import (
	"regexp"
	"strings"

	"github.com/container-image-exporter/container-image-exporter/internal/version/semver"
)

// Strategy is the way a tag is checked for updates.
type Strategy int

// Strategies.
const (
	// Unsupported means the tag format is not recognized; no version is
	// invented and the check is reported as unsuccessful.
	Unsupported Strategy = iota
	// SemVer compares the extracted version with other tags of the family.
	SemVer
	// Floating compares the running digest with the registry digest.
	Floating
)

func (s Strategy) String() string {
	switch s {
	case SemVer:
		return "semver"
	case Floating:
		return "floating"
	default:
		return "unsupported"
	}
}

// Detection is the result of Detect.
type Detection struct {
	Strategy Strategy
	// SemVer is set when Strategy is SemVer.
	SemVer semver.Tag
	// Reason explains an Unsupported result.
	Reason string
}

var (
	// Valid OCI tag grammar.
	validTagRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	// Tags made of digits and hyphens only, e.g. "2026-11-10", "20240101", "8".
	numericHyphenRE = regexp.MustCompile(`^[0-9-]+$`)
)

// floatingAliases are well-known moving tags, matched case-insensitively
// against the whole tag.
var floatingAliases = map[string]struct{}{
	"latest": {}, "stable": {}, "edge": {}, "main": {}, "master": {},
	"nightly": {}, "dev": {}, "develop": {}, "canary": {}, "rolling": {},
	"current": {}, "mainline": {}, "lts": {}, "next": {}, "beta": {},
	"alpha": {}, "rc": {}, "release": {}, "testing": {}, "unstable": {},
	"snapshot": {}, "head": {}, "trunk": {}, "tip": {}, "prod": {},
	"production": {}, "staging": {},
}

// Detect classifies tag. Precedence, first match wins:
//
//  1. empty or malformed tag: Unsupported
//  2. known moving alias (latest, stable, ...): Floating
//  3. digits and hyphens only (2026-11-10, 8): Floating
//  4. contains a two or three component version: SemVer
//  5. anything else (abc123, v1, jdk17): Unsupported
func Detect(tag string) Detection {
	if tag == "" {
		return Detection{Reason: "empty tag"}
	}
	if !validTagRE.MatchString(tag) {
		return Detection{Reason: "malformed tag"}
	}
	if _, ok := floatingAliases[strings.ToLower(tag)]; ok {
		return Detection{Strategy: Floating}
	}
	if numericHyphenRE.MatchString(tag) {
		return Detection{Strategy: Floating}
	}
	parsed, err := semver.Parse(tag)
	if err != nil {
		return Detection{Reason: err.Error()}
	}
	return Detection{Strategy: SemVer, SemVer: parsed}
}
