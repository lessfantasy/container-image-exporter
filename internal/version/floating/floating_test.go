package floating

import (
	"errors"
	"testing"
)

const (
	index1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	index2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	amd64  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	arm64  = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	other  = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func TestCompare(t *testing.T) {
	tests := []struct {
		name    string
		running []string
		remote  Remote
		update  bool
		err     error
	}{
		{name: "equal digest", running: []string{index1}, remote: Remote{Digest: index1}},
		{name: "different digest", running: []string{index1}, remote: Remote{Digest: index2}, update: true},
		{name: "missing running digest", running: nil, remote: Remote{Digest: index1}, err: ErrNoRunningDigest},
		{name: "empty running digest", running: []string{""}, remote: Remote{Digest: index1}, err: ErrNoRunningDigest},
		{name: "missing registry digest", running: []string{index1}, remote: Remote{}, err: ErrNoRemoteDigest},
		{
			name:    "runtime reports the index digest",
			running: []string{index1},
			remote:  Remote{Digest: index1, Platforms: []string{amd64, arm64}},
		},
		{
			name:    "runtime reports the platform manifest digest",
			running: []string{arm64},
			remote:  Remote{Digest: index1, Platforms: []string{amd64, arm64}},
		},
		{
			name:    "platform manifest replaced",
			running: []string{other},
			remote:  Remote{Digest: index2, Platforms: []string{amd64, arm64}},
			update:  true,
		},
		{
			name:    "index republished, running platform unchanged",
			running: []string{arm64},
			remote:  Remote{Digest: index2, Platforms: []string{other, arm64}},
		},
		{
			name:    "one of several running digests matches",
			running: []string{other, index1},
			remote:  Remote{Digest: index1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			update, err := Compare(tt.running, tt.remote)
			if !errors.Is(err, tt.err) {
				t.Fatalf("error = %v, want %v", err, tt.err)
			}
			if update != tt.update {
				t.Errorf("update = %v, want %v", update, tt.update)
			}
		})
	}
}
