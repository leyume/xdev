package runtime

import (
	"reflect"
	"testing"
)

// TestParsePlatformArches covers the output shapes the engines actually
// produce. Getting any of these wrong yields an empty list, which callers would
// read as "publishes for nothing" — so each shape is pinned.
func TestParsePlatformArches(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{
			// docker manifest inspect on a multi-arch tag.
			name: "manifest list",
			raw: `{"manifests":[
				{"platform":{"architecture":"amd64","os":"linux"}},
				{"platform":{"architecture":"arm64","os":"linux","variant":"v8"}}
			]}`,
			want: []string{"amd64", "arm64"},
		},
		{
			// docker manifest inspect -v on a single-arch tag: the platform hangs
			// off Descriptor, not a manifests array. This is the shape that made
			// leyume/swoole look platform-less.
			name: "single arch verbose",
			raw:  `{"Descriptor":{"platform":{"architecture":"arm64","os":"linux","variant":"v8"}}}`,
			want: []string{"arm64"},
		},
		{
			// -v on a manifest list is an array of those objects.
			name: "verbose list",
			raw: `[{"Descriptor":{"platform":{"architecture":"amd64","os":"linux"}}},
			       {"Descriptor":{"platform":{"architecture":"s390x","os":"linux"}}}]`,
			want: []string{"amd64", "s390x"},
		},
		{
			// buildx attaches attestation manifests whose platform is unknown;
			// counting them would claim support for an architecture that has no
			// runnable image.
			name: "attestations ignored",
			raw: `{"manifests":[
				{"platform":{"architecture":"amd64","os":"linux"}},
				{"platform":{"architecture":"unknown","os":"unknown"}}
			]}`,
			want: []string{"amd64"},
		},
		{
			// Windows images share a registry with linux ones; a linux host can't
			// run them, so they must not count as support.
			name: "non-linux ignored",
			raw: `{"manifests":[
				{"platform":{"architecture":"amd64","os":"windows"}},
				{"platform":{"architecture":"arm64","os":"linux"}}
			]}`,
			want: []string{"arm64"},
		},
		{name: "not json", raw: `unauthorized: authentication required`, want: nil},
		{name: "empty", raw: `{}`, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePlatformArches([]byte(tc.raw))
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
