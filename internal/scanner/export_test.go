package scanner

import (
	"github.com/container-image-exporter/container-image-exporter/internal/discovery"
	"github.com/container-image-exporter/container-image-exporter/internal/version"
)

func targetKeyFor(inst discovery.Instance) string {
	return targetKey(inst.Image, version.Detect(inst.Image.Tag), inst.Digests)
}
