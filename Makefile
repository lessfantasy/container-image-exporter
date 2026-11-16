VERSION ?= dev
IMAGE   ?= container-image-exporter:$(VERSION)
DOCKER_BUILD_ARGS ?=

.PHONY: build test lint image deb

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/container-image-exporter ./cmd/exporter

test:
	go test ./...
	go test -race ./...

lint:
	test -z "$$(gofmt -l .)"
	go vet ./...

image:
	docker buildx build --platform linux/amd64,linux/arm64 $(DOCKER_BUILD_ARGS) --build-arg VERSION=$(VERSION) -t $(IMAGE) .

# Debian packages for amd64 and arm64 in dist/. Needs a numeric VERSION.
deb:
	mkdir -p bin dist
	for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/container-image-exporter-linux-$$arch ./cmd/exporter && \
		packaging/deb/build.sh bin/container-image-exporter-linux-$$arch $(VERSION) $$arch dist || exit 1; \
	done
