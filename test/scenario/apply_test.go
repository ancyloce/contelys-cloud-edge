//go:build scenario

package scenario

import (
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/contelys/contelys-cloud-edge/internal/application"
	"github.com/contelys/contelys-cloud-edge/test/fixture"
)

// These scenarios show that a route change takes effect without a restart and that every instance
// confirms the revision it applied (ticket 08 of Edge's plan, V09).

// Three configuration revisions, as Git commits.
const (
	revisionA = fixture.Revision
	revisionB = "1111111111111111111111111111111111111111"
	revisionC = "2222222222222222222222222222222222222222"
)

// The fixed-response route answers on the internal listener alone, under any host, with the body
// that the distributed file carries, and the built-in mocking plugin adds no header of its own.
func TestTheFixedResponseRouteAnswersTheRevisionAndTheDigestOnTheInternalListenerAlone(t *testing.T) {
	t.Parallel()
	current := projectsOnly(t)
	out := current.render(t)
	edge := startEdge(t, current.ca, out)

	response := edge.internalGet(t, application.RevisionPath)
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if want := distributedAnswer(t, out, edgeAddress); response.StatusCode != http.StatusOK || string(body) != want {
		t.Errorf("the internal listener answered %d with %q, want 200 with %q", response.StatusCode, body, want)
	}
	if got := response.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type %q, want application/json", got)
	}
	if got := response.Header.Get("X-Mock-By"); got != "" {
		t.Errorf("the answer carries X-Mock-By: %s", got)
	}

	if got := edge.get(t, externalListener, application.RevisionPath, nil).StatusCode; got != http.StatusNotFound {
		t.Errorf("the external listener answered %s with %d, want 404", application.RevisionPath, got)
	}
	// The loopback listener binds 127.0.0.1 inside the container, and the image has no HTTP client.
	_, reader, err := edge.container.Exec(t.Context(), []string{"bash", "-c",
		`exec 3<>/dev/tcp/127.0.0.1/9080 && printf 'GET ` + application.RevisionPath + ` HTTP/1.0\r\nHost: edge\r\n\r\n' >&3 && head -n 1 <&3`})
	if err != nil {
		t.Fatal(err)
	}
	statusLine, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(statusLine), " 404 ") {
		t.Errorf("the loopback listener answered %q, want 404", statusLine)
	}

	edge.confirmed(t, out, edgeAddress, revisionA)
}

// APISIX compares the modification time of apisix.yaml at a resolution of one second, so a file
// whose time lies in the second that an instance loaded is never applied, whatever it holds. This is
// why the renderer never replaces the file twice within one second (section 03).
func TestAFileWhoseTimeLiesInTheSecondThatTheInstanceLoadedIsNeverApplied(t *testing.T) {
	t.Parallel()
	current := projectsOnly(t)
	out := current.render(t)
	edge := startEdge(t, current.ca, out)
	_, info := readWithInfo(t, routesFile(out))
	loaded := info.ModTime()

	changed := writeBeside(t, routesFile(out), changedRoutes(t, current))
	if err := os.Chtimes(changed, loaded, loaded); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(changed, routesFile(out)); err != nil {
		t.Fatal(err)
	}
	edge.expectNotApplied(t, 3*pollingInterval)

	// The same file, with a time in a later second, is applied.
	now := time.Now()
	if err := os.Chtimes(routesFile(out), now, now); err != nil {
		t.Fatal(err)
	}
	edge.expectApplied(t, now)
}

// V09: a changed route fragment takes effect while the container keeps its start time, and the
// instance confirms the new revision.
func TestARouteChangeTakesEffectWithoutARestartAndTheInstanceConfirmsTheNewRevision(t *testing.T) {
	t.Parallel()
	current := projectsOnly(t)
	out := current.render(t)
	edge := startEdge(t, current.ca, out)
	edge.confirmed(t, out, edgeAddress, revisionA)
	startedAt, restarts := edge.started(t)

	changed := current
	changed.fragments = map[application.Slug]string{"projects": archiveFragment}
	changed.revision = revisionB
	changed.apply(t, out)

	edge.confirmed(t, out, edgeAddress, revisionB)
	if got := edge.get(t, externalListener, archivePath, nil).StatusCode; got != stubStatus {
		t.Errorf("%s answered %d after the change, want %d", archivePath, got, stubStatus)
	}
	if nowStartedAt, nowRestarts := edge.started(t); nowStartedAt != startedAt || nowRestarts != restarts {
		t.Errorf("the container started at %s after %d restarts, then at %s after %d", startedAt, restarts, nowStartedAt, nowRestarts)
	}
	// The earlier revision no longer counts as applied.
	if output, err := edge.confirm(t, out, edgeAddress, revisionA); !exitedWith(err, 2) || !strings.Contains(output, "carries the revision "+revisionB) {
		t.Errorf("edge confirm %s: %v: %s; want exit code 2", revisionA, err, output)
	}
}

// A fragment that breaks a rule writes no file, so the instance goes on confirming its current
// revision (issue 13 of the phase 1 platform).
func TestAnInvalidFragmentLeavesTheCurrentRevisionServing(t *testing.T) {
	t.Parallel()
	current := projectsOnly(t)
	out := current.render(t)
	edge := startEdge(t, current.ca, out)

	invalid := current
	invalid.fragments = map[application.Slug]string{"projects": archiveFragment + `
  - {paths: [/internal/v1/archive], methods: [GET], auth: anonymous}
`}
	invalid.revision = revisionB
	if output, err := invalid.renderInto(t, out); !exitedWith(err, 1) {
		t.Fatalf("edge render: %v: %s; want exit code 1", err, output)
	}

	edge.confirmed(t, out, edgeAddress, revisionA)
	edge.expectNotApplied(t, 0)
}

// A rollback renders the older content as a new revision, so the history stays linear (ADR 0010).
func TestARollbackReportsANewRevisionWithTheOldRoutes(t *testing.T) {
	t.Parallel()
	first := projectsOnly(t)
	out := first.render(t)
	edge := startEdge(t, first.ca, out)
	edge.confirmed(t, out, edgeAddress, revisionA)
	firstAnswer, firstRoutes := distributedAnswer(t, out, edgeAddress), edge.loadedIDs(t, "routes")

	changed := first
	changed.fragments = map[application.Slug]string{"projects": archiveFragment}
	changed.revision = revisionB
	changed.apply(t, out)
	edge.confirmed(t, out, edgeAddress, revisionB)

	rollback := first
	rollback.revision = revisionC
	rollback.apply(t, out)
	edge.confirmed(t, out, edgeAddress, revisionC)

	if got := edge.loadedIDs(t, "routes"); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(firstRoutes))) {
		t.Errorf("after the rollback the instance loaded the routes %v, want those of the first revision, %v", got, firstRoutes)
	}
	if answer := distributedAnswer(t, out, edgeAddress); answer == firstAnswer || !strings.Contains(answer, revisionC) {
		t.Errorf("the rollback answers %s, the first revision %s; want a new revision and digest", answer, firstAnswer)
	}
	edge.expectNotApplied(t, 0)
}

// Each Edge instance gets its own apisix.yaml, and each confirms the revision that it applied.
func TestEveryEdgeInstanceConfirmsTheRevisionOfItsOwnFile(t *testing.T) {
	t.Parallel()
	current := projectsOnly(t)
	current.edges = []string{"edge-1", "edge-2"}
	out := current.render(t)
	for _, address := range current.edges {
		edge := startEdgeAt(t, current.ca, out, address)
		edge.confirmed(t, out, address, revisionA)
		if got := edge.get(t, externalListener, "/api/v1/projects", nil).StatusCode; got != stubStatus {
			t.Errorf("the instance at %s answered /api/v1/projects with %d", address, got)
		}
	}
}

// exitedWith reports whether err is the exit of a command with code.
func exitedWith(err error, code int) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == code
}
