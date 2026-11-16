package semver

import (
	"errors"
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		tag        string
		prefix     string
		version    string
		components int
		pre        string
		family     string
		counters   []uint64
	}{
		{"1.26.2", "", "1.26.2", 3, "", "", nil},
		{"v1.14.6", "v", "1.14.6", 3, "", "", nil},
		{"main-mongod8.3", "main-mongod", "8.3", 2, "", "", nil},
		{"v1.14.6-eksbuild.4", "v", "1.14.6", 3, "", "-eksbuild.#", []uint64{4}},
		{"1.9.0", "", "1.9.0", 3, "", "", nil},
		{"8.3", "", "8.3", 2, "", "", nil},
		{"1.26.2-alpine", "", "1.26.2", 3, "", "-alpine", nil},
		{"1.26.2-alpine3.20", "", "1.26.2", 3, "", "-alpine3.#", []uint64{20}},
		{"1.2.3-arm64v8", "", "1.2.3", 3, "", "-arm64v8", nil},
		{"1.2.3-rc.1", "", "1.2.3", 3, "rc", "", nil},
		{"1.2.3-rc1", "", "1.2.3", 3, "rc", "", nil},
		{"1.2.3-beta2", "", "1.2.3", 3, "beta", "", nil},
		{"1.2.3-alpha", "", "1.2.3", 3, "alpha", "", nil},
		{"1.27.0-rc.1-alpine3.20", "", "1.27.0", 3, "rc", "-alpine3.#", []uint64{20}},
		{"8.0.35-debian-12-r5", "", "8.0.35", 3, "", "-debian-#-r#", []uint64{12, 5}},
		{"17.0.9_9-jre-alpine", "", "17.0.9", 3, "", "_#-jre-alpine", []uint64{9}},
		{"release-2.4.1", "release-", "2.4.1", 3, "", "", nil},
		{"24.04", "", "24.04", 2, "", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			got, err := Parse(tt.tag)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got.Raw != tt.tag || got.Prefix != tt.prefix || got.Version != tt.version ||
				got.Components != tt.components || got.PreChannel != tt.pre || got.Family != tt.family {
				t.Errorf("got %+v", got)
			}
			if !reflect.DeepEqual(got.Counters, tt.counters) {
				t.Errorf("counters = %v, want %v", got.Counters, tt.counters)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := map[string]error{
		"":                               ErrNoVersion,
		"latest":                         ErrNoVersion,
		"abc123":                         ErrNoVersion,
		"v1":                             ErrNoVersion,
		"2026-11-10":                     ErrNoVersion,
		"1..2":                           ErrNoVersion,
		"1.2.3.4":                        ErrTooManyComponents,
		"v1.2.3.4-alpine":                ErrTooManyComponents,
		"1.2.3-abc1234":                  ErrCommitHash,
		"1.2.3-g0a1b2c3d":                ErrCommitHash,
		"99999999999999999999999.1":      ErrNumberTooLarge,
		"1.2-99999999999999999999999":    ErrNumberTooLarge,
		"1.2-rc99999999999999999999999":  ErrNumberTooLarge,
		"1.2-r99999999999999999999999":   ErrNumberTooLarge,
		"1.2-rc.99999999999999999999999": ErrNumberTooLarge,
	}
	for tag, want := range tests {
		if _, err := Parse(tag); !errors.Is(err, want) {
			t.Errorf("Parse(%q) error = %v, want %v", tag, err, want)
		}
	}
}

func TestCompareIsNumeric(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.9.0", "1.10.0", -1},
		{"2.9", "2.10", -1},
		{"1.10.0", "1.9.0", 1},
		{"1.2.3", "1.2.3", 0},
		{"8.3", "8.4", -1},
		{"1.2.3-rc.1", "1.2.3", -1},
		{"1.2.3-rc.2", "1.2.3-rc.10", -1},
		{"v1.14.6-eksbuild.4", "v1.14.6-eksbuild.10", -1},
		{"24.04", "24.10", -1},
	}
	for _, tt := range tests {
		a, err := Parse(tt.a)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Parse(tt.b)
		if err != nil {
			t.Fatal(err)
		}
		if got := sign(a.Compare(b)); got != tt.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

func TestLatest(t *testing.T) {
	tests := []struct {
		name       string
		current    string
		candidates []string
		latest     string
		newer      bool
	}{
		{
			name:       "major upgrade",
			current:    "1.26.2",
			candidates: []string{"1.26.2", "1.27.0", "2.0.1", "latest", "stable"},
			latest:     "2.0.1", newer: true,
		},
		{
			name:       "minor upgrade, numeric not lexicographic",
			current:    "1.9.0",
			candidates: []string{"1.9.0", "1.9.5", "1.10.0"},
			latest:     "1.10.0", newer: true,
		},
		{
			name:       "patch upgrade",
			current:    "1.26.2",
			candidates: []string{"1.26.1", "1.26.2", "1.26.3"},
			latest:     "1.26.3", newer: true,
		},
		{
			name:       "two component versions",
			current:    "2.9",
			candidates: []string{"2.9", "2.10", "2.10.1", "3"},
			latest:     "2.10", newer: true,
		},
		{
			name:       "two components never switch to three",
			current:    "8.3",
			candidates: []string{"8.3", "8.4.1", "9.0.0"},
			latest:     "8.3", newer: false,
		},
		{
			name:       "different tag prefixes",
			current:    "main-mongod8.3",
			candidates: []string{"main-mongod8.3", "main-operator9.0", "mongod9.9", "9.9", "main-mongod8.4"},
			latest:     "main-mongod8.4", newer: true,
		},
		{
			name:       "v prefix is part of the family",
			current:    "v1.2.3",
			candidates: []string{"1.3.0", "V1.3.0"},
			latest:     "v1.2.3", newer: false,
		},
		{
			name:       "different suffix families",
			current:    "v1.14.6-eksbuild.4",
			candidates: []string{"v1.15.0", "v1.16.0-fargate.1", "v1.14.6-eksbuild.5", "v1.15.0-eksbuild.1", "v1.15.0-eksbuild.2"},
			latest:     "v1.15.0-eksbuild.2", newer: true,
		},
		{
			name:       "build counter alone is an update",
			current:    "v1.14.6-eksbuild.4",
			candidates: []string{"v1.14.6-eksbuild.4", "v1.14.6-eksbuild.10"},
			latest:     "v1.14.6-eksbuild.10", newer: true,
		},
		{
			name:       "alpine stays alpine",
			current:    "1.26.2-alpine",
			candidates: []string{"1.27.0", "1.27.0-bookworm", "1.27.0-alpine", "1.28.0-alpine3.20", "1.28.0-alpine-slim"},
			latest:     "1.27.0-alpine", newer: true,
		},
		{
			name:       "architecture variants",
			current:    "1.2.3-arm64",
			candidates: []string{"1.3.0-amd64", "1.2.4-arm64", "1.3.0-arm32"},
			latest:     "1.2.4-arm64", newer: true,
		},
		{
			name:       "java flavours are different families",
			current:    "3.9.6-jdk17",
			candidates: []string{"3.9.9-jdk21", "3.9.7-jdk17"},
			latest:     "3.9.7-jdk17", newer: true,
		},
		{
			name:       "pre-releases excluded for a stable tag",
			current:    "1.2.3",
			candidates: []string{"1.3.0-rc.1", "1.2.4-beta1", "2.0.0-alpha"},
			latest:     "1.2.3", newer: false,
		},
		{
			name:       "pre-release follows its own channel",
			current:    "1.3.0-rc.1",
			candidates: []string{"1.3.0-rc.2", "1.3.0-beta.9", "1.4.0-alpha.1"},
			latest:     "1.3.0-rc.2", newer: true,
		},
		{
			name:       "stable release supersedes a pre-release",
			current:    "1.3.0-rc.1",
			candidates: []string{"1.3.0-rc.2", "1.3.0"},
			latest:     "1.3.0", newer: true,
		},
		{
			name:       "multiple candidate tags",
			current:    "1.0.0",
			candidates: []string{"1.0.1", "3.2.1", "2.9.9", "3.2.0", "3.10.0", "3.9.12"},
			latest:     "3.10.0", newer: true,
		},
		{
			name:       "no newer version",
			current:    "1.2.3",
			candidates: []string{"1.2.3", "1.2.2", "1.0.0", "0.9"},
			latest:     "1.2.3", newer: false,
		},
		{
			name:       "nothing in the family",
			current:    "main-mongod8.3",
			candidates: []string{"latest", "9.0", "abc123"},
			latest:     "main-mongod8.3", newer: false,
		},
		{
			name:       "current tag missing from the registry",
			current:    "1.2.3",
			candidates: []string{"1.2.2"},
			latest:     "1.2.3", newer: false,
		},
		{
			name:       "empty registry listing",
			current:    "1.2.3",
			candidates: nil,
			latest:     "1.2.3", newer: false,
		},
		{
			name:       "commit hash candidates are ignored",
			current:    "1.2.3",
			candidates: []string{"1.2.4-abc1234", "1.2.3.1"},
			latest:     "1.2.3", newer: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current, err := Parse(tt.current)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.current, err)
			}
			latest, newer := Latest(current, tt.candidates)
			if latest != tt.latest || newer != tt.newer {
				t.Errorf("Latest = %q, %v; want %q, %v", latest, newer, tt.latest, tt.newer)
			}
		})
	}
}

func TestLatestIsOrderIndependent(t *testing.T) {
	current, _ := Parse("1.0.0")
	a, _ := Latest(current, []string{"1.2.0", "1.10.0", "1.9.0"})
	b, _ := Latest(current, []string{"1.9.0", "1.10.0", "1.2.0"})
	if a != b || a != "1.10.0" {
		t.Errorf("got %q and %q", a, b)
	}
}
