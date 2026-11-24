//go:build scenario

package scenario

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// These scenarios confirm on the test baseline what the design relies on from APISIX itself
// (Edge detailed design, sections 03, 07 and 08). They repeat whenever the baseline moves.

func TestThePinnedImageReportsItsVersionAndListensForNoAdminAPI(t *testing.T) {
	t.Parallel()
	ca := newAuthority(t)
	edge := startEdge(t, ca, setup{ca: ca}.render(t)) // an empty route file

	version := lastLine(edge.exec(t, "apisix", "version"))
	if version != "3.21.0" {
		t.Errorf("version: got %q, want 3.21.0", version)
	}
	if !atLeast(t, version, "3.20.0") {
		t.Errorf("version %s is below the floor of 3.20.0 (section 07)", version)
	}
	ports := edge.listeningPorts(t)
	want := map[int]bool{9443: true, 9444: true, 9080: true, 9090: true}
	if !mapsEqual(ports, want) {
		t.Errorf("listening ports: got %v, want only the external, internal, loopback and control listeners %v", ports, want)
	}
	if ids := edge.loadedIDs(t, "routes"); len(ids) != 0 {
		t.Errorf("the control listener reports routes %v for an empty route file", ids)
	}
}

func TestEveryRenderedRouteAndUpstreamIsLoaded(t *testing.T) {
	t.Parallel()
	ca := newAuthority(t)
	projects := startStub(t, ca, "projects", "projects")
	authoring := startStub(t, ca, "authoring", "authoring")
	out := setup{ca: ca, stubs: []*stub{projects, authoring}, fragments: map[string]string{
		"projects": `
routes:
  - {paths: [/api/v1/projects, /api/v1/projects/*], methods: [GET, POST], auth: anonymous}
  - {paths: [/api/v1/project-templates], methods: [GET], auth: anonymous, traffic: transfer}
`,
		"authoring": `
routes:
  - {paths: [/*], methods: [GET, HEAD], auth: anonymous}
`,
	}}.render(t)
	edge := startEdge(t, ca, out)

	for _, kind := range []string{"routes", "upstreams"} {
		rendered, loaded := renderedIDs(t, out, kind), edge.loadedIDs(t, kind)
		slices.Sort(rendered)
		slices.Sort(loaded)
		if len(rendered) == 0 || !slices.Equal(rendered, loaded) {
			t.Errorf("%s: rendered %v, loaded %v", kind, rendered, loaded)
		}
	}
	// A more specific route wins over Authoring's remainder.
	if got := edge.get(t, externalPort, "/api/v1/projects/7", nil).Header.Get(stubHeader); got != "projects" {
		t.Errorf("/api/v1/projects/7 reached %q", got)
	}
	if got := edge.get(t, externalPort, "/editor/index.html", nil).Header.Get(stubHeader); got != "authoring" {
		t.Errorf("/editor/index.html reached %q", got)
	}
}

// Assumption A3 of the implementation plan.
func TestARouteAnswersOnlyOnItsListenerAndARouteWithoutTheConditionOnEveryListener(t *testing.T) {
	t.Parallel()
	ca := newAuthority(t)
	projects := startStub(t, ca, "projects", "projects")
	out := setup{ca: ca, stubs: []*stub{projects}, fragments: map[string]string{"projects": projectsFragment}}.render(t)
	addRoute(t, out, map[string]any{"id": "without-condition", "uri": "/unbound", "upstream_id": "projects"})
	edge := startEdge(t, ca, out)

	for _, c := range []struct {
		port, path string
		want       int
	}{
		{externalPort, "/api/v1/projects", stubStatus},
		{internalPort, "/api/v1/projects", http.StatusNotFound},
		{externalPort, "/unbound", stubStatus},
		{internalPort, "/unbound", stubStatus},
	} {
		if got := edge.get(t, c.port, c.path, nil).StatusCode; got != c.want {
			t.Errorf("%s on %s: got %d, want %d", c.path, c.port, got, c.want)
		}
	}
}

func TestAFileRenamedIntoTheMountedDirectoryIsAppliedWithinThePollingInterval(t *testing.T) {
	t.Parallel()
	ca := newAuthority(t)
	projects := startStub(t, ca, "projects", "projects")
	before := setup{ca: ca, stubs: []*stub{projects}, fragments: map[string]string{"projects": projectsFragment}}
	out := before.render(t)
	edge := startEdge(t, ca, out)
	if got := edge.get(t, externalPort, "/api/v1/archive", nil).StatusCode; got != http.StatusNotFound {
		t.Fatalf("/api/v1/archive before the change: got %d, want 404", got)
	}

	after := before
	after.fragments = map[string]string{"projects": projectsFragment + `
  - {paths: [/api/v1/archive], methods: [GET], auth: anonymous}
`}
	changed, err := os.ReadFile(filepath.Join(after.render(t), edgeAddress, "apisix.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// APISIX compares modification times in whole seconds, so the new file must not share the
	// second of the current one (section 03).
	time.Sleep(time.Second)
	renameIntoPlace(t, filepath.Join(out, edgeAddress, "apisix.yaml"), changed)
	renamed := time.Now()

	for {
		if edge.get(t, externalPort, "/api/v1/archive", nil).StatusCode == stubStatus {
			break
		}
		if time.Since(renamed) > 3*time.Second {
			t.Fatal("the renamed file was not applied within 3 s, three polling intervals")
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("applied %v after the rename", time.Since(renamed).Round(100*time.Millisecond))
}

// addRoute appends a hand-written route to the rendered apisix.yaml.
func addRoute(t *testing.T, out string, route map[string]any) {
	t.Helper()
	path := filepath.Join(out, edgeAddress, "apisix.yaml")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]any
	if err := yaml.Unmarshal(content, &file); err != nil {
		t.Fatal(err)
	}
	file["routes"] = append(file["routes"].([]any), route)
	renameIntoPlace(t, path, append(marshal(t, file), "#END\n"...))
}

// renameIntoPlace writes content beside path and renames it into place, as a deployment does.
func renameIntoPlace(t *testing.T, path string, content []byte) {
	t.Helper()
	temporary := filepath.Join(filepath.Dir(path), ".apisix.yaml.new")
	if err := os.WriteFile(temporary, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, path); err != nil {
		t.Fatal(err)
	}
}

func lastLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// atLeast reports whether the dotted version is at least floor.
func atLeast(t *testing.T, version, floor string) bool {
	t.Helper()
	parse := func(v string) []int {
		var parts []int
		for _, part := range strings.Split(v, ".") {
			n, err := strconv.Atoi(part)
			if err != nil {
				t.Fatalf("version %q: %v", v, err)
			}
			parts = append(parts, n)
		}
		return parts
	}
	return slices.Compare(parse(version), parse(floor)) >= 0
}

func mapsEqual(a, b map[int]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
