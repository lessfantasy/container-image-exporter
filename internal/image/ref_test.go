package image

import (
	"errors"
	"testing"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestParse(t *testing.T) {
	tests := []struct {
		in         string
		registry   string
		repository string
		tag        string
		digest     string
	}{
		{"nginx", "docker.io", "library/nginx", "latest", ""},
		{"nginx:1.26.2", "docker.io", "library/nginx", "1.26.2", ""},
		{"grafana/grafana:11.0.0", "docker.io", "grafana/grafana", "11.0.0", ""},
		{"docker.io/library/nginx:1.26", "docker.io", "library/nginx", "1.26", ""},
		{"index.docker.io/library/nginx", "docker.io", "library/nginx", "latest", ""},
		{"ghcr.io/org/app:v1.2.3", "ghcr.io", "org/app", "v1.2.3", ""},
		{"quay.io/prometheus/node-exporter:v1.8.0", "quay.io", "prometheus/node-exporter", "v1.8.0", ""},
		{"gcr.io/project/app:1.0", "gcr.io", "project/app", "1.0", ""},
		{"europe-docker.pkg.dev/p/r/app:1.0", "europe-docker.pkg.dev", "p/r/app", "1.0", ""},
		{"myreg.azurecr.io/team/app:2.1", "myreg.azurecr.io", "team/app", "2.1", ""},
		{"123456789012.dkr.ecr.eu-west-1.amazonaws.com/app:3.0.0", "123456789012.dkr.ecr.eu-west-1.amazonaws.com", "app", "3.0.0", ""},
		{"registry.example.com:5000/team/app", "registry.example.com:5000", "team/app", "latest", ""},
		{"localhost:5000/app:dev", "localhost:5000", "app", "dev", ""},
		{"nginx@" + testDigest, "docker.io", "library/nginx", "", testDigest},
		{"nginx:1.26@" + testDigest, "docker.io", "library/nginx", "1.26", testDigest},
		{"registry.example.com:5000/app@" + testDigest, "registry.example.com:5000", "app", "", testDigest},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			ref, err := Parse(tt.in)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if ref.Registry != tt.registry || ref.Repository != tt.repository || ref.Tag != tt.tag || ref.Digest != tt.digest {
				t.Errorf("got %q %q %q %q, want %q %q %q %q",
					ref.Registry, ref.Repository, ref.Tag, ref.Digest,
					tt.registry, tt.repository, tt.tag, tt.digest)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{"", "   ", testDigest, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"} {
		if _, err := Parse(in); !errors.Is(err, ErrNoReference) {
			t.Errorf("Parse(%q) error = %v, want ErrNoReference", in, err)
		}
	}
	for _, in := range []string{"UPPER/case:tag", "nginx:bad tag", "nginx@sha256:short"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", in)
		}
	}
}

func TestStringAndKeys(t *testing.T) {
	ref, err := Parse("nginx:1.26@" + testDigest)
	if err != nil {
		t.Fatal(err)
	}
	if got := ref.String(); got != "docker.io/library/nginx:1.26" {
		t.Errorf("String() = %q", got)
	}
	if got := ref.RepositoryKey(); got != "docker.io/library/nginx" {
		t.Errorf("RepositoryKey() = %q", got)
	}
	if got := ref.TagRef().String(); got != "index.docker.io/library/nginx:1.26" {
		t.Errorf("TagRef() = %q", got)
	}
}

func TestDigestOf(t *testing.T) {
	tests := map[string]string{
		"docker.io/library/nginx@" + testDigest:  testDigest,
		"docker-pullable://nginx@" + testDigest:  testDigest,
		testDigest:                               "",
		"docker://" + testDigest:                 "",
		"nginx@sha256:abc":                       "",
		"":                                       "",
		"registry:5000/app@" + testDigest + "zz": "",
	}
	for in, want := range tests {
		if got := DigestOf(in); got != want {
			t.Errorf("DigestOf(%q) = %q, want %q", in, got, want)
		}
	}
}
