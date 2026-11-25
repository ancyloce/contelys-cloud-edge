//go:build scenario

package scenario

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	edge := startEdge(t, ca, setup{ca: ca}.render(t)) // a route file without fragments

	// APISIX names its version in the Server header of its answers.
	server := edge.get(t, externalListener, "/no-route", nil).Header.Get("Server")
	version, found := strings.CutPrefix(server, "APISIX/")
	if !found || version != "3.21.0" {
		t.Errorf("Server: got %q, want APISIX/3.21.0", server)
	}
	if found && !atLeast(t, version, "3.20.0") {
		t.Errorf("version %s is below the floor of 3.20.0 (section 07)", version)
	}
	// Without fragments the fixed-response route is the only route.
	if ids := edge.loadedIDs(t, "routes"); !slices.Equal(ids, []string{application.RevisionRouteID}) {
		t.Errorf("the control listener reports routes %v for a route file without fragments", ids)
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

// The loopback listener binds 127.0.0.1 inside the container, so from the network an instance
// accepts connections on its external, internal and control listeners and on no other port.
func TestFromTheNetworkAnInstanceAcceptsConnectionsOnlyOnItsListeners(t *testing.T) {
	t.Parallel()
	ca := fixture.NewAuthority(t)
	edge := startEdge(t, ca, setup{ca: ca}.render(t))
	// The scan dials the container's own address, which a Docker engine on Linux routes from the host.
	address := edge.containerAddress(t)
	if !accepts(address, portOf(externalListener)) {
		t.Fatalf("the host cannot reach the container's address %s", address)
	}

	want := []int{portOf(controlListener), portOf(externalListener), portOf(internalListener)}
	if open := openPorts(address); !slices.Equal(open, want) {
		t.Errorf("ports that accept connections from the network: got %v, want %v", open, want)
	}
}

// accepts reports whether port of address accepts a TCP connection within a second.
func accepts(address string, port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(address, strconv.Itoa(port)), time.Second)
	if err == nil {
		c.Close()
	}
	return err == nil
}

// openPorts dials every TCP port of address and returns, in order, those that accept a connection.
func openPorts(address string) []int {
	ports := make(chan int)
	var (
		mu   sync.Mutex
		open []int
		wg   sync.WaitGroup
	)
	for range 256 {
		wg.Go(func() {
			for port := range ports {
				if accepts(address, port) {
					mu.Lock()
					open = append(open, port)
					mu.Unlock()
				}
			}
		})
	}
	for port := 1; port <= 65535; port++ {
		ports <- port
	}
	close(ports)
	wg.Wait()
	slices.Sort(open)
	return open
}

// portOf is the container port that a listener, such as "9443/tcp", listens on.
func portOf(listener string) int {
	port, err := strconv.Atoi(strings.TrimSuffix(listener, "/tcp"))
	if err != nil {
		panic(err)
	}
	return port
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

// APISIX checks every item of apisix.yaml on its own, drops one that fails, and loads the rest
// (section 03). It drops a whole route for a plugin that it does not know or that config.yaml does
// not enable, and for a name longer than 256 characters. A dropped route answers 404, and a route
// whose upstream was dropped answers 502.
func TestAPISIXDropsAnItemThatFailsItsValidationAndLoadsTheRest(t *testing.T) {
	t.Parallel()
	current := projectsOnly(t)
	out := current.render(t)
	rendered := map[string][]string{"routes": renderedIDs(t, out, "routes"), "upstreams": renderedIDs(t, out, "upstreams")}
	onExternal := [][]any{{"server_port", "==", portOf(externalListener)}}
	route := func(id string, fields map[string]any) map[string]any {
		r := map[string]any{"id": id, "name": id, "uri": "/" + id, "upstream_id": "projects", "vars": onExternal}
		for key, value := range fields {
			r[key] = value
		}
		return r
	}
	addItems(t, out, map[string][]any{
		"routes": {
			route("unknown-plugin", map[string]any{"plugins": map[string]any{"no-such-plugin": map[string]any{}}}),
			route("disabled-plugin", map[string]any{"plugins": map[string]any{"cors": map[string]any{}}}),
			route("long-name", map[string]any{"name": strings.Repeat("n", 257)}),
			route("orphan", map[string]any{"upstream_id": "invalid-upstream"}),
		},
		"upstreams": {map[string]any{
			"id": "invalid-upstream", "type": "roundrobin", "scheme": "ftp",
			"nodes": []any{map[string]any{"host": hostGateway, "port": current.stubs[0].port, "weight": 1}},
		}},
	})
	edge := startEdge(t, current.ca, out)

	for kind, want := range map[string][]string{
		"routes":    append(rendered["routes"], "orphan"),
		"upstreams": rendered["upstreams"],
	} {
		loaded := edge.loadedIDs(t, kind)
		slices.Sort(loaded)
		slices.Sort(want)
		if !slices.Equal(loaded, want) {
			t.Errorf("%s: loaded %v, want %v", kind, loaded, want)
		}
	}
	for path, want := range map[string]int{
		"/api/v1/projects": stubStatus,
		"/unknown-plugin":  http.StatusNotFound,
		"/disabled-plugin": http.StatusNotFound,
		"/long-name":       http.StatusNotFound,
		"/orphan":          http.StatusBadGateway,
	} {
		if got := edge.get(t, externalListener, path, nil).StatusCode; got != want {
			t.Errorf("%s: got %d, want %d", path, got, want)
		}
	}
}

// Assumption A3 of Edge's plan.
func TestARouteAnswersOnlyOnItsListenerAndARouteWithoutTheConditionOnEveryListener(t *testing.T) {
	t.Parallel()
	current := projectsOnly(t)
	out := current.render(t)
	addRoute(t, out, map[string]any{"id": "without-condition", "uri": "/unbound", "upstream_id": "projects"})
	// The renderer writes the port as a number. The condition holds with a string too.
	addRoute(t, out, map[string]any{"id": "port-as-string", "uri": "/port-as-string", "upstream_id": "projects",
		"vars": [][]any{{"server_port", "==", strconv.Itoa(portOf(externalListener))}}})
	edge := startEdge(t, current.ca, out)

	for _, c := range []struct {
		listener, path string
		want           int
	}{
		{externalListener, "/api/v1/projects", stubStatus},
		{internalListener, "/api/v1/projects", http.StatusNotFound},
		{externalListener, "/port-as-string", stubStatus},
		{internalListener, "/port-as-string", http.StatusNotFound},
		{externalListener, "/unbound", stubStatus},
		{internalListener, "/unbound", stubStatus},
	} {
		if got := edge.get(t, c.listener, c.path, nil).StatusCode; got != c.want {
			t.Errorf("%s on %s: got %d, want %d", c.path, c.listener, got, c.want)
		}
	}
}

// Each APISIX worker process checks the modification time of apisix.yaml once per polling interval
// and applies a changed file within a tenth of a second of reading it. The scenarios allow
// applyMargin for that tenth and for the request that observes the change (section 03).
const (
	pollingInterval = time.Second
	applyMargin     = 500 * time.Millisecond
)

// The scenarios that change the route file add the route on archivePath to projectsFragment.
const (
	archivePath     = "/api/v1/archive"
	archiveFragment = projectsFragment + `
  - {paths: [/api/v1/archive], methods: [GET], auth: anonymous}
`
)

func TestAFileRenamedIntoTheMountedDirectoryIsAppliedWithinOnePollingInterval(t *testing.T) {
	t.Parallel()
	current := projectsOnly(t)
	out := current.render(t)
	edge := startEdge(t, current.ca, out)
	edge.expectNotApplied(t, 0)

	renameIntoPlace(t, routesFile(out), changedRoutes(t, current))
	edge.expectApplied(t, time.Now())
}

// The reason for Edge ADR 0003.
func TestAMountOfTheFileItselfNeverSeesARename(t *testing.T) {
	t.Parallel()
	current := projectsOnly(t)
	out := current.render(t)
	edge := startEdgeMounting(t, current.ca, out, fileMount(out), controlAnswers(http.StatusOK))

	renameIntoPlace(t, routesFile(out), changedRoutes(t, current))
	edge.expectNotApplied(t, 3*pollingInterval)
}

// APISIX uses the modification time as the version of every resource and starts each at version 0.
// A copy from an archive without times gives a file this modification time. Until a file with
// another time replaces it, the instance has no certificate, so the external listener fails every
// TLS handshake, and the control listener cannot list the routes and fails with 500.
func TestAnInstanceThatStartsWithAFileWhoseModificationTimeIsZeroNeverAppliesIt(t *testing.T) {
	t.Parallel()
	current := projectsOnly(t)
	out := current.render(t)
	if err := os.Chtimes(routesFile(out), time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	edge := startEdgeMounting(t, current.ca, out, directoryMount(out), controlAnswers(http.StatusInternalServerError))

	for end := time.Now().Add(3 * pollingInterval); time.Now().Before(end); time.Sleep(250 * time.Millisecond) {
		response, err := edge.client(t, externalListener).Get("https://" + fixture.ApplicationHostname + "/api/v1/projects")
		if err == nil {
			response.Body.Close()
			t.Fatalf("/api/v1/projects answered %d: the file was applied", response.StatusCode)
		}
		// crypto/tls reports an alert that the server sent as a "remote error".
		if received := new(net.OpError); !errors.As(err, &received) || received.Op != "remote error" {
			t.Fatalf("/api/v1/projects: got %v, want a TLS alert from the external listener", err)
		}
	}
	if got := edge.control(t, "/v1/routes").StatusCode; got != http.StatusInternalServerError {
		t.Errorf("/v1/routes on the control listener: got %d, want 500", got)
	}
}

func TestAFileThatCannotBeOpenedIsRetriedAtTheNextPoll(t *testing.T) {
	t.Parallel()
	current := projectsOnly(t)
	out := current.render(t)
	edge := startEdge(t, current.ca, out)

	// The renderer's mode, before the deployment gives the file to the user that Edge runs as.
	unreadable := writeBeside(t, routesFile(out), changedRoutes(t, current))
	if err := os.Chmod(unreadable, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(unreadable, routesFile(out)); err != nil {
		t.Fatal(err)
	}
	edge.expectNotApplied(t, 2*pollingInterval)

	// A change of mode keeps the modification time, so only a retry applies the file.
	if err := os.Chmod(routesFile(out), 0o644); err != nil {
		t.Fatal(err)
	}
	edge.expectApplied(t, time.Now())
}

func TestAFileWithoutTheEndMarkerIsRetriedAtTheNextPoll(t *testing.T) {
	t.Parallel()
	current := projectsOnly(t)
	out := current.render(t)
	edge := startEdge(t, current.ca, out)

	changed := changedRoutes(t, current)
	unfinished, found := bytes.CutSuffix(changed, []byte("#END\n"))
	if !found {
		t.Fatal("the rendered file does not end with #END")
	}
	renameIntoPlace(t, routesFile(out), unfinished)
	edge.expectNotApplied(t, 2*pollingInterval)

	// The finished file keeps the modification time of the unfinished one, so only a retry applies it.
	info, err := os.Stat(routesFile(out))
	if err != nil {
		t.Fatal(err)
	}
	finished := writeBeside(t, routesFile(out), changed)
	if err := os.Chtimes(finished, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(finished, routesFile(out)); err != nil {
		t.Fatal(err)
	}
	edge.expectApplied(t, time.Now())
}

// expectApplied fails unless the route on archivePath answers within one polling interval and
// applyMargin of since. Each worker process applies the file on its own, so once that time has
// passed it also expects the route on new connections, which nginx spreads over its workers.
func (e *edge) expectApplied(t *testing.T, since time.Time) {
	t.Helper()
	deadline := since.Add(pollingInterval + applyMargin)
	for e.get(t, externalListener, archivePath, nil).StatusCode != stubStatus {
		if time.Now().After(deadline) {
			t.Fatalf("the change was not applied within %v", pollingInterval+applyMargin)
		}
		time.Sleep(50 * time.Millisecond)
	}
	first := time.Since(since)
	time.Sleep(time.Until(deadline))
	for i := range 8 {
		if got := e.get(t, externalListener, archivePath, nil).StatusCode; got != stubStatus {
			t.Fatalf("%s on connection %d after %v: got %d", archivePath, i+1, pollingInterval+applyMargin, got)
		}
	}
	t.Logf("applied %v after the change", first.Round(10*time.Millisecond))
}

// expectNotApplied fails if, at any time within d, the route on archivePath answers or the current
// routes stop answering. A d of 0 checks once.
func (e *edge) expectNotApplied(t *testing.T, d time.Duration) {
	t.Helper()
	for end := time.Now().Add(d); ; time.Sleep(250 * time.Millisecond) {
		if got := e.get(t, externalListener, archivePath, nil).StatusCode; got != http.StatusNotFound {
			t.Fatalf("%s answered %d: the change was applied", archivePath, got)
		}
		if got := e.get(t, externalListener, "/api/v1/projects", nil).StatusCode; got != stubStatus {
			t.Fatalf("/api/v1/projects answered %d: the current routes were dropped", got)
		}
		if !time.Now().Before(end) {
			return
		}
	}
}

// changedRoutes renders the scenario's inputs with archiveFragment instead of its fragments, and
// returns the new apisix.yaml. APISIX compares modification times in whole seconds, so it then
// waits a second, so that the changed file does not share the second of the current one.
func changedRoutes(t *testing.T, s setup) []byte {
	t.Helper()
	s.fragments = map[application.Slug]string{"projects": archiveFragment}
	content, err := os.ReadFile(routesFile(s.render(t)))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	return content
}

// projectsOnly is a scenario's input with one deployable, projects, its stub and projectsFragment.
func projectsOnly(t *testing.T) setup {
	t.Helper()
	ca := fixture.NewAuthority(t)
	return setup{ca: ca, stubs: []*stub{startStub(t, ca, "projects", "projects")},
		fragments: map[application.Slug]string{"projects": projectsFragment}}
}

// addRoute appends a hand-written route to the rendered apisix.yaml.
func addRoute(t *testing.T, out string, route map[string]any) {
	t.Helper()
	addItems(t, out, map[string][]any{"routes": {route}})
}

// addItems appends hand-written items of each kind, such as routes, to the rendered apisix.yaml.
func addItems(t *testing.T, out string, items map[string][]any) {
	t.Helper()
	path := routesFile(out)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]any
	if err := yaml.Unmarshal(content, &file); err != nil {
		t.Fatal(err)
	}
	for kind, added := range items {
		file[kind] = append(file[kind].([]any), added...)
	}
	renameIntoPlace(t, path, append(marshal(t, file), "#END\n"...))
}

// renameIntoPlace writes content beside path and renames it into place, as a deployment does.
func renameIntoPlace(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.Rename(writeBeside(t, path, content), path); err != nil {
		t.Fatal(err)
	}
}

// writeBeside writes content into a new file in the directory of path, readable for every user, and
// returns the new file's path.
func writeBeside(t *testing.T, path string, content []byte) string {
	t.Helper()
	temporary := filepath.Join(filepath.Dir(path), ".apisix.yaml.new")
	if err := os.WriteFile(temporary, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return temporary
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
