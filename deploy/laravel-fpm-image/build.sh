#!/bin/sh
# Build and (optionally) push the Laravel php-fpm image.
#
#   ./build.sh                       build locally for this machine, no push
#   ./build.sh --push                build for amd64+arm64 and push
#   ./build.sh --push --this-arch    push for THIS machine's architecture only
#   ./build.sh --push --tag 1.1.0    same, with an explicit version
#
# Multi-arch by default when pushing. That is deliberate: the existing
# leyume/swoole tags are single-arch and, as it happens, for opposite
# architectures (dev arm64, prod amd64), which xdev carries a whole
# image-detection layer to work around. Publishing this one for both means an
# fpm app just runs, wherever it lands.
set -e
cd "$(dirname "$0")"

REPO="${REPO:-leyume/php-fpm-alphine}"
TAG="${TAG:-1.0.0}"
PHP_VERSION="${PHP_VERSION:-8.5}"
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"
PUSH=0

while [ $# -gt 0 ]; do
  case "$1" in
    --push)  PUSH=1 ;;
    --this-arch) PLATFORMS="" ;;  # resolved to the host's arch below
    --tag)   shift; TAG="$1" ;;
    --repo)  shift; REPO="$1" ;;
    --php)   shift; PHP_VERSION="$1" ;;
    -h|--help)
      sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'
      exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
  shift
done

# Two tags: the exact version, and the PHP minor as a moving pointer. xdev pins
# the exact one; the floating tag is for humans doing a quick `docker run`.
T_VERSION="$REPO:$TAG"
T_PHP="$REPO:$PHP_VERSION"

echo "repo      $REPO"
echo "tags      $TAG, $PHP_VERSION"
echo "php       $PHP_VERSION"

if [ "$PUSH" = "0" ]; then
  echo "platform  (this machine)"
  echo
  docker build --build-arg "PHP_VERSION=$PHP_VERSION" -t "$T_VERSION" -t "$T_PHP" .
  echo
  echo "Built locally. Verify before pushing:"
  echo "  ./verify.sh $T_VERSION"
  echo "Then push with:  ./build.sh --push --tag $TAG"
  exit 0
fi

# An architecture this machine cannot execute natively is built under QEMU, and
# install-php-extensions compiles every extension from source -- each ./configure
# alone runs hundreds of tiny conftest compiles through the emulator. On a small
# host that turns a 4-minute build into the better part of an hour, with no
# output to suggest anything is happening. Say so before starting, not after.
# Squeeze whitespace out: a docker that cannot reach its daemon still prints an
# empty line to stdout before failing, and an arch of "\namd64" silently matches
# no platform at all -- which would report every architecture as foreign.
HOST_ARCH=$(docker version --format '{{.Server.Arch}}' 2>/dev/null | tr -d '[:space:]')
if [ -z "$HOST_ARCH" ]; then
  case "$(uname -m)" in
    x86_64|amd64)  HOST_ARCH=amd64 ;;
    aarch64|arm64) HOST_ARCH=arm64 ;;
    *)             HOST_ARCH=$(uname -m) ;;
  esac
fi
if [ -z "$PLATFORMS" ]; then
  PLATFORMS="linux/$HOST_ARCH"
fi
FOREIGN=""
for p in $(echo "$PLATFORMS" | tr ',' ' '); do
  case "$p" in
    */"$HOST_ARCH") ;;
    *) FOREIGN="$FOREIGN ${p#*/}" ;;
  esac
done

echo "platform  $PLATFORMS"
if [ -n "$FOREIGN" ]; then
  CORES=$(nproc 2>/dev/null || echo 2)
  echo
  echo "  NOTE:$FOREIGN is emulated on this $HOST_ARCH host, and every PHP"
  echo "  extension is compiled from source. Expect 30-60 min on $CORES cores,"
  echo "  most of it with no visible progress."
  echo
  echo "  For this machine only (a few minutes):"
  echo "    ./build.sh --push --this-arch"
  echo
  echo "  Multi-arch is still the better artifact -- the existing leyume/swoole"
  echo "  tags are single-arch for opposite architectures, which is the problem"
  echo "  this avoids. Worth doing once from a faster machine, or with the time."
  echo
fi

if ! docker buildx version >/dev/null 2>&1; then
  echo "!! docker buildx is required for a multi-arch push." >&2
  echo "   Single-arch fallback (this machine only):" >&2
  echo "     docker build -t $T_VERSION . && docker push $T_VERSION" >&2
  exit 1
fi

# A named builder, because the default "docker" driver cannot do multi-platform.
# Reused across runs; created once.
if ! docker buildx inspect xdev-builder >/dev/null 2>&1; then
  echo "creating buildx builder 'xdev-builder'"
  docker buildx create --name xdev-builder --driver docker-container --bootstrap >/dev/null
fi

docker buildx build \
  --builder xdev-builder \
  --platform "$PLATFORMS" \
  --build-arg "PHP_VERSION=$PHP_VERSION" \
  -t "$T_VERSION" \
  -t "$T_PHP" \
  --push \
  .

echo
echo "pushed $T_VERSION and $T_PHP for $PLATFORMS"
echo
echo "Point xdev at it (if the tag differs from its built-in default):"
echo "  XDEV_LARAVEL_FPM_IMAGE=docker.io/$T_VERSION"
