#!/bin/sh
# Builds a .deb from an already compiled binary.
# Usage: build.sh <binary> <version> <deb-arch> <output-dir>
set -eu

BIN=$1
VERSION=$2
ARCH=$3
OUT=$4

NAME=container-image-exporter
MAINTAINER=${MAINTAINER:-Container Image Exporter Maintainers <maintainers@example.invalid>}
SRC=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$SRC/../.." && pwd)

case "$VERSION" in
    [0-9]*) ;;
    *) echo "deb version must start with a digit, got '$VERSION' (set VERSION=x.y.z)" >&2; exit 2 ;;
esac

STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT
chmod 0755 "$STAGE"

install -D -m 0755 "$BIN" "$STAGE/usr/bin/$NAME"
install -D -m 0644 "$SRC/$NAME.service" "$STAGE/usr/lib/systemd/system/$NAME.service"
install -D -m 0640 "$SRC/$NAME.default" "$STAGE/etc/default/$NAME"
install -D -m 0644 "$ROOT/README.md" "$STAGE/usr/share/doc/$NAME/README.md"
install -D -m 0644 "$ROOT/docs/metrics-design.md" "$STAGE/usr/share/doc/$NAME/metrics-design.md"

install -d -m 0755 "$STAGE/DEBIAN"
for script in postinst prerm postrm; do
    install -m 0755 "$SRC/$script" "$STAGE/DEBIAN/$script"
done
echo "/etc/default/$NAME" >"$STAGE/DEBIAN/conffiles"

SIZE=$(du -sk --exclude=DEBIAN "$STAGE" | cut -f1)
cat >"$STAGE/DEBIAN/control" <<EOF
Package: $NAME
Version: $VERSION
Architecture: $ARCH
Maintainer: $MAINTAINER
Installed-Size: $SIZE
Depends: adduser
Recommends: ca-certificates
Section: net
Priority: optional
Description: Prometheus exporter for container image updates
 Discovers the images running in Docker or Kubernetes, asks their
 registries for newer versions and exposes the result as two Prometheus
 metrics. Read-only towards the runtime and the registries.
EOF

mkdir -p "$OUT"
dpkg-deb --root-owner-group -Zxz --build "$STAGE" "$OUT/${NAME}_${VERSION}_${ARCH}.deb"
