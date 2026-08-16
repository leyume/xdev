#!/bin/sh
# Build and (optionally) push the Laravel php-fpm image.
#
#   ./build.sh                     build locally for this machine, no push
#   ./build.sh --push              build for amd64+arm64 and push
#   ./build.sh --push --tag 1.1.0  same, with an explicit version
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

echo "platform  $PLATFORMS"
echo

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
