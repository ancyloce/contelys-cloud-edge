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

	"github.com/contelys/contelys-cloud-edge/internal/application"
	"github.com/contelys/contelys-cloud-edge/test/fixture"
)

// These scenarios confirm on the test baseline what the design relies on from APISIX itself
// (Edge detailed design, sections 03, 07 and 08). They repeat whenever the baseline moves.

func TestThePinnedImageReportsItsVersionAndOpensNoAdminAPI(t *testing.T) {
	t.Parallel()
	ca := fixture.NewAuthority(t)
	edge := startEdge(t, ca, setup{ca: ca}.render(t)) // an empty route file

	// APISIX names its version in the Server header of its answers.
	server := edge.get(t, externalListener, "/no-route", nil).Header.Get("Server")
	version, found := strings.CutPrefix(server, "APISIX/")
	if !found || version != "3.21.0" {
		t.Errorf("Server: got %q, want APISIX/3.21.0", server)
	}
	if found && !atLeast(t, version, "3.20.0") {
		t.Errorf("version %s is below the floor of 3.20.0 (section 07)", version)
	}
	if ids := edge.loadedIDs(t, "routes"); len(ids) != 0 {
		t.Errorf("the control listener reports routes %v for an empty route file", ids)
	}

	// Nothing answers where the Admin API would listen, and no listener serves its paths.
	if response, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + edge.address(t, adminAPI) + "/apisix/admin/routes"); err == nil {
		response.Body.Close()
		t.Errorf("something answered %d where the Admin API would listen", response.StatusCode)
	}
	for _, listener := range []string{externalListener, internalListener} {
		if got := edge.get(t, listener, "/apisix/admin/routes", nil).StatusCode; got != http.StatusNotFound {
			t.Errorf("/apisix/admin/routes on %s: got %d, want 404", listener, got)
		}
	}
	if got := edge.control(t, "/apisix/admin/routes").StatusCode; got != http.StatusNotFound {
		t.Errorf("/apisix/admin/routes on the control listener: got %d, want 404", got)
	}
}

func TestEveryRenderedRouteAndUpstreamIsLoaded(t *testing.T) {
	t.Parallel()
	ca := fixture.NewAuthority(t)
	projects := startStub(t, ca, "projects", "projects")
	authoring := startStub(t, ca, "authoring", "authoring")
	out := setup{ca: ca, stubs: []*stub{projects, authoring}, fragments: map[application.Slug]string{
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
	if got := edge.get(t, externalListener, "/api/v1/projects/7", nil).Header.Get(stubHeader); got != "projects" {
		t.Errorf("/api/v1/projects/7 reached %q", got)
	}
	if got := edge.get(t, externalListener, "/editor/index.html", nil).Header.Get(stubHeader); got != "authoring" {
		t.Errorf("/editor/index.html reached %q", got)
	}
}

// Assumption A3 of the implementation plan.
func TestARouteAnswersOnlyOnItsListenerAndARouteWithoutTheConditionOnEveryListener(t *testing.T) {
	t.Parallel()
	ca := fixture.NewAuthority(t)
	projects := startStub(t, ca, "projects", "projects")
	out := setup{ca: ca, stubs: []*stub{projects}, fragments: map[application.Slug]string{"projects": projectsFragment}}.render(t)
	addRoute(t, out, map[string]any{"id": "without-condition", "uri": "/unbound", "upstream_id": "projects"})
	edge := startEdge(t, ca, out)

	for _, c := range []struct {
		listener, path string
		want           int
	}{
		{externalListener, "/api/v1/projects", stubStatus},
		{internalListener, "/api/v1/projects", http.StatusNotFound},
		{externalListener, "/unbound", stubStatus},
		{internalListener, "/unbound", stubStatus},
	} {
		if got := edge.get(t, c.listener, c.path, nil).StatusCode; got != c.want {
			t.Errorf("%s on %s: got %d, want %d", c.path, c.listener, got, c.want)
		}
	}
}

func TestAFileRenamedIntoTheMountedDirectoryIsAppliedWithinThePollingInterval(t *testing.T) {
	t.Parallel()
	ca := fixture.NewAuthority(t)
	projects := startStub(t, ca, "projects", "projects")
	before := setup{ca: ca, stubs: []*stub{projects}, fragments: map[application.Slug]string{"projects": projectsFragment}}
	out := before.render(t)
	edge := startEdge(t, ca, out)
	if got := edge.get(t, externalListener, "/api/v1/archive", nil).StatusCode; got != http.StatusNotFound {
		t.Fatalf("/api/v1/archive before the change: got %d, want 404", got)
	}

	after := before
	after.fragments = map[application.Slug]string{"projects": projectsFragment + `
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
		if edge.get(t, externalListener, "/api/v1/archive", nil).StatusCode == stubStatus {
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
