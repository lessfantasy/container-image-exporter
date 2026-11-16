package version

import (
	"strings"
	"testing"
)

func TestDetect(t *testing.T) {
	tests := []struct {
		tag      string
		strategy Strategy
		version  string // extracted version for SemVer
	}{
		// SemVer
		{"1.26.2", SemVer, "1.26.2"},
		{"v1.14.6", SemVer, "1.14.6"},
		{"main-mongod8.3", SemVer, "8.3"},
		{"v1.14.6-eksbuild.4", SemVer, "1.14.6"},
		{"1.9.0", SemVer, "1.9.0"},
		{"1.10.0", SemVer, "1.10.0"},
		{"8.3", SemVer, "8.3"},
		{"8.4", SemVer, "8.4"},
		{"1.27-alpine", SemVer, "1.27"},
		{"release-2.4.1", SemVer, "2.4.1"},
		{"17.0.9_9-jre-alpine", SemVer, "17.0.9"},
		{"latest-1.2", SemVer, "1.2"}, // not an exact alias: the version wins

		// Floating: known aliases
		{"latest", Floating, ""},
		{"stable", Floating, ""},
		{"LATEST", Floating, ""},
		{"edge", Floating, ""},
		{"nightly", Floating, ""},
		{"main", Floating, ""},

		// Floating: digits and hyphens only, even when it looks like a version
		{"2026-11-10", Floating, ""},
		{"20261110", Floating, ""},
		{"8", Floating, ""},
		{"1-2-3", Floating, ""},

		// Unsupported
		{"", Unsupported, ""},
		{"abc123", Unsupported, ""},
		{"v1", Unsupported, ""},
		{"jdk17", Unsupported, ""},
		{"1-alpine", Unsupported, ""},
		{"1.2.3.4", Unsupported, ""},
		{"1.2.3-abc1234", Unsupported, ""},
		{"sha-0a1b2c3", Unsupported, ""},
		{"1..2", Unsupported, ""},

		// Malformed
		{"-leading-hyphen", Unsupported, ""},
		{".leading-dot", Unsupported, ""},
		{"has space", Unsupported, ""},
		{"1.2.3+build", Unsupported, ""},
		{"tag:colon", Unsupported, ""},
		{strings.Repeat("a", 129), Unsupported, ""},
	}
	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			got := Detect(tt.tag)
			if got.Strategy != tt.strategy {
				t.Fatalf("Detect(%q) = %s (%s), want %s", tt.tag, got.Strategy, got.Reason, tt.strategy)
			}
			if tt.strategy == SemVer && got.SemVer.Version != tt.version {
				t.Errorf("version = %q, want %q", got.SemVer.Version, tt.version)
			}
			if tt.strategy == Unsupported && got.Reason == "" {
				t.Error("unsupported result without a reason")
			}
		})
	}
}

// TestPrecedence pins the documented order: alias, then digits-and-hyphens,
// then SemVer.
func TestPrecedence(t *testing.T) {
	if got := Detect("stable").Strategy; got != Floating {
		t.Errorf("alias: got %s", got)
	}
	// "2026-11" has two numeric components but no dot and only digits/hyphens.
	if got := Detect("2026-11").Strategy; got != Floating {
		t.Errorf("numeric-hyphen: got %s", got)
	}
	// With dots it is a version.
	if got := Detect("2026.11").Strategy; got != SemVer {
		t.Errorf("dotted: got %s", got)
	}
}

func TestStrategyString(t *testing.T) {
	for s, want := range map[Strategy]string{SemVer: "semver", Floating: "floating", Unsupported: "unsupported"} {
		if s.String() != want {
			t.Errorf("%d.String() = %q, want %q", s, s.String(), want)
		}
	}
}
