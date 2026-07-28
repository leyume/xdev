package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// platformCache memoises ImagePlatforms for the life of the process. Inspecting
// a manifest is a network round trip, and the answer only changes when a tag is
// republished — not something worth paying for on every app create. A restart
// picks up a re-pushed tag, which is the same granularity as everything else
// xdev caches about images.
var platformCache sync.Map // image string -> platformResult

type platformCacheEntry struct {
	arches []string
	err    error
}

// ImagePlatforms reports the architectures an image is published for, by asking
// the engine to inspect the registry manifest. The image need not be pulled.
//
// Returns an error when the manifest can't be read at all — no network, a
// private registry needing auth, an engine whose manifest subcommand behaves
// differently. Callers are expected to degrade to a static assumption rather
// than treat that as "no architectures".
func ImagePlatforms(ctx context.Context, engine Engine, image string) ([]string, error) {
	if v, ok := platformCache.Load(image); ok {
		e := v.(platformCacheEntry)
		return e.arches, e.err
	}
	arches, err := imagePlatformsUncached(ctx, engine, image)
	platformCache.Store(image, platformCacheEntry{arches: arches, err: err})
	return arches, err
}

func imagePlatformsUncached(ctx context.Context, engine Engine, image string) ([]string, error) {
	// -v makes docker report the platform for a single-arch tag too; without it
	// the output is a bare manifest with nothing to match on. podman ignores the
	// flag on some versions, so a failure here retries without it rather than
	// giving up on the whole lookup.
	out, err := Exec(ctx, engine, "manifest", "inspect", "-v", image)
	if err != nil {
		out, err = Exec(ctx, engine, "manifest", "inspect", image)
		if err != nil {
			return nil, fmt.Errorf("inspect manifest of %s: %w", image, err)
		}
	}
	arches := parsePlatformArches([]byte(out))
	if len(arches) == 0 {
		return nil, fmt.Errorf("inspect manifest of %s: no platforms in engine output", image)
	}
	return arches, nil
}

// parsePlatformArches pulls every linux architecture out of a manifest
// inspection.
//
// It walks the JSON rather than unmarshalling a fixed shape on purpose: the
// output differs between a single-arch tag and a manifest list, between docker
// and podman, and between versions of each — a struct that fits one of them
// silently yields nothing for the rest, which would read as "no architectures"
// and quietly disable detection. Walking for platform objects is stable across
// all of those shapes.
func parsePlatformArches(raw []byte) []string {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	seen := map[string]bool{}
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			// A platform object: {"architecture": "amd64", "os": "linux", ...}.
			// Ignore attestation manifests, whose architecture is "unknown".
			if arch, ok := v["architecture"].(string); ok && arch != "" && arch != "unknown" {
				if os, ok := v["os"].(string); !ok || os == "linux" {
					seen[arch] = true
				}
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)

	arches := make([]string, 0, len(seen))
	for a := range seen {
		arches = append(arches, a)
	}
	sort.Strings(arches) // deterministic, for logs and tests
	return arches
}
