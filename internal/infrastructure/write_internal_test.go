package infrastructure

import (
	"strings"
	"testing"
)

// The encoder always ends apisix.yaml with the #END marker, so no input can produce a file without
// it. The fixtures are such files. APISIX looks for the marker, followed by nothing but white space,
// in the last 10 bytes of the file.
func TestARouteFileWithoutTheEndMarkerIsRefused(t *testing.T) {
	const want = "the apisix.yaml of 10.0.0.5 does not end with the #END marker, without which APISIX never applies it"
	for _, content := range []string{
		"routes: []\n",
		"routes: []\n#END\nupstreams: []\n",
		"routes: []\n#END\n\n\n\n\n\n\n\n",
	} {
		if err := checkRouteFile("the apisix.yaml of 10.0.0.5", []byte(content)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", content, err, want)
		}
	}
	for _, content := range []string{"routes: []\n#END\n", "routes: []\n#END"} {
		if err := checkRouteFile("the apisix.yaml of 10.0.0.5", []byte(content)); err != nil {
			t.Errorf("%q: %v", content, err)
		}
	}
}
