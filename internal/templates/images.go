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

// laravelImageArches is the offline fallback for what each tag is published
// for, used only when the registry can't be asked (no network, private
// registry, an engine whose manifest subcommand misbehaves). The live answer
// comes from the engine — see LaravelImageDetected — so republishing a tag as
// multi-arch takes effect without touching this file.
//
// It records what was true when written: these tags are single-arch and, as it
// happens, for opposite architectures. That matters because naming the "right"
// variant for the environment is not enough — on the wrong host it fails at
// exec with "exec format error", which looks nothing like an image-availability
// problem.
var laravelImageArches = map[string][]string{
	LaravelImageDev:  {"arm64"},
	LaravelImageProd: {"amd64"},
}

// ArchLookup reports the architectures an image is published for. ok is false
// when that couldn't be determined, which is different from "none" and must
// not be read as "this image runs nowhere".
type ArchLookup func(image string) (arches []string, ok bool)

// LaravelImageDetected picks the laravel image using live platform data.
//
// The policy is the same as LaravelImage — dev for local, prod for prod, fall
// back to whichever variant this host can actually run — but "can actually run"
// is answered by inspecting the registry instead of a table that has to be kept
// current by hand. When the lookup can't answer for an image, that image's
// entry in laravelImageArches is used, and an image absent from both is assumed
// to run anywhere (an operator's override is not ours to second-guess).
func LaravelImageDetected(env, goarch, override string, lookup ArchLookup) (image, fallbackReason string) {
	if override != "" {
		return override, ""
	}
	runsOn := func(img string) bool {
		if lookup != nil {
			if arches, ok := lookup(img); ok {
				return containsArch(arches, goarch)
			}
		}
		return imageRunsOn(img, goarch)
	}
	preferred, alternate := LaravelImageDev, LaravelImageProd
	if env == "prod" {
		preferred, alternate = LaravelImageProd, LaravelImageDev
	}
	if runsOn(preferred) {
		return preferred, ""
	}
	if runsOn(alternate) {
		return alternate, fmt.Sprintf(
			"%s is not published for %s; using %s instead", preferred, goarch, alternate)
	}
	return preferred, fmt.Sprintf(
		"no laravel image is published for %s; trying %s, which may not start",
		goarch, preferred)
}

// ResolveLaravelImage picks the laravel image for env on this host, honouring
// the XDEV_LARAVEL_IMAGE override. RenderCompose and the apps service both go
// through it, so what a compose file says and what everything derived from it
// assumes — the user to chown the scaffold to, what doctor reports — can't
// drift apart.
func ResolveLaravelImage(env string) (image, fallbackReason string) {
	return LaravelImage(env, goarch, os.Getenv(laravelImageEnvVar))
}

// LaravelImage picks the image a laravel app serves from without consulting the
// registry, using only what laravelImageArches recorded. It is the answer for
// callers that have no engine to ask through — template rendering in a test,
// and the offline path of the detected version.
func LaravelImage(env, goarch, override string) (image, fallbackReason string) {
	return LaravelImageDetected(env, goarch, override, nil)
}

// imageRunsOn reports whether an image is published for an architecture
// according to the offline table. Unknown images are assumed to run anywhere:
// the table only covers our own tags, and an operator's override or a future
// image should not be second guessed.
func imageRunsOn(image, goarch string) bool {
	arches, known := laravelImageArches[image]
	if !known {
		return true
	}
	return containsArch(arches, goarch)
}

func containsArch(arches []string, goarch string) bool {
	for _, a := range arches {
		if a == goarch {
			return true
		}
	}
	return false
}
