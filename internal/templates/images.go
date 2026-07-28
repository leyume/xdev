package templates

import (
	"fmt"
	"os"
	stdruntime "runtime"
)

// goarch is this build's architecture, split out so tests can reason about
// hosts they aren't running on.
var goarch = stdruntime.GOARCH

// laravelImageEnvVar pins the laravel app image, bypassing the selection below.
// It is the escape hatch for a private registry, a pinned digest, or an image
// built for an architecture this table doesn't know about.
const laravelImageEnvVar = "XDEV_LARAVEL_IMAGE"

// The Swoole images the laravel stack can serve from. The dev variant carries a
// full toolchain (composer, dev dependencies); the prod variant is hardened —
// no build tooling, and it runs as an unprivileged user.
const (
	LaravelImageDev  = "docker.io/leyume/swoole:2.0.0-dev"
	LaravelImageProd = "docker.io/leyume/swoole:2.0.0-prod"
)

// laravelImageArches records the architectures each tag is actually published
// for. These tags are single-arch and, as it happens, for opposite
// architectures — so naming the "right" variant for the environment is not
// enough; on the wrong host it fails at exec with "exec format error", which
// looks nothing like an image-availability problem. Keep this table in step
// with what is pushed to the registry; publishing multi-arch tags is the real
// fix, after which the entries here become {"amd64", "arm64"} and every
// fallback below stops triggering on its own.
var laravelImageArches = map[string][]string{
	LaravelImageDev:  {"arm64"},
	LaravelImageProd: {"amd64"},
}

// ResolveLaravelImage picks the laravel image for env on this host, honouring
// the XDEV_LARAVEL_IMAGE override. RenderCompose and the apps service both go
// through it, so what a compose file says and what everything derived from it
// assumes — the user to chown the scaffold to, what doctor reports — can't
// drift apart.
func ResolveLaravelImage(env string) (image, fallbackReason string) {
	return LaravelImage(env, goarch, os.Getenv(laravelImageEnvVar))
}

// LaravelImage picks the image a laravel app serves from.
//
// A prod project gets the hardened variant and a local one gets the dev
// variant — that is the choice the environment is meant to express. When the
// preferred variant isn't published for this machine's architecture, it falls
// back to one that is and returns a reason explaining the substitution, so the
// surprise surfaces in the log instead of as a crash-looping container.
// A non-empty override wins over everything and is returned as-is: an operator
// naming their own image knows better than this table.
func LaravelImage(env, goarch, override string) (image, fallbackReason string) {
	if override != "" {
		return override, ""
	}
	preferred, alternate := LaravelImageDev, LaravelImageProd
	if env == "prod" {
		preferred, alternate = LaravelImageProd, LaravelImageDev
	}
	if imageRunsOn(preferred, goarch) {
		return preferred, ""
	}
	if imageRunsOn(alternate, goarch) {
		return alternate, fmt.Sprintf(
			"%s is not published for %s; using %s instead", preferred, goarch, alternate)
	}
	// Neither is known to run here. Prefer the intended one and say so rather
	// than silently substituting an image that is no better.
	return preferred, fmt.Sprintf(
		"no laravel image is published for %s; trying %s, which may not start",
		goarch, preferred)
}

// imageRunsOn reports whether an image is published for an architecture.
// Unknown images are assumed to run anywhere: the table only covers our own
// tags, and an operator's override or a future image should not be second
// guessed.
func imageRunsOn(image, goarch string) bool {
	arches, known := laravelImageArches[image]
	if !known {
		return true
	}
	for _, a := range arches {
		if a == goarch {
			return true
		}
	}
	return false
}
