package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/contelys/contelys-cloud-edge/internal/application"
	"github.com/contelys/contelys-cloud-edge/test/fixture"
)

// These tests drive edge confirm through its command line against an instance of their own: a TLS
// server for the name edge in place of the internal listener, which answers the fixed-response
// route, and a plain server in place of the control listener. The scenarios confirm a running Edge.

// quickly is how these tests confirm: a polling interval of a fifth of a second, read every 20 ms,
// given up after two seconds.
var quickly = application.Timing{Interval: 200 * time.Millisecond, Every: 20 * time.Millisecond, Timeout: 2 * time.Second}

// rendered is the output of edge render for one Edge instance, and what its distributed file holds.
type rendered struct {
	in        *fixture.Inputs
	out       output
	answer    string
	routes    []string
	upstreams []string
}

func renderForConfirmation(t *testing.T, ca *fixture.Authority) rendered {
	t.Helper()
	in := newInputsFrom(t, ca)
	in.Fragment(t, "projects", `
routes:
  - {paths: [/api/v1/projects], methods: [GET], auth: anonymous}
`)
	out := renderOK(t, in)
	file := out.apisix(t, edgeAddress)
	r := rendered{in: in, out: out}
	route := file.routeWithURIs(t, application.RevisionPath)
	r.answer = route["plugins"].(map[string]any)["mocking"].(map[string]any)["response_example"].(string)
	for _, item := range file.items(t, "routes") {
		r.routes = append(r.routes, item["id"].(string))
	}
	for _, item := range file.items(t, "upstreams") {
		r.upstreams = append(r.upstreams, item["id"].(string))
	}
	return r
}

// instance stands in for a running Edge instance. Its nth answer of the fixed-response route, from
// 0, is answer(n), and its control listener reports the ids it was given.
type instance struct {
	internal, control *httptest.Server
	reads             atomic.Int64
}

func startInstance(t *testing.T, ca *fixture.Authority, answer func(n int) string, routes, upstreams []string) *instance {
	t.Helper()
	i := &instance{}
	_, _, pair := ca.Issue(t, string(application.EdgeSlug))
	i.internal = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != application.RevisionPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, answer(int(i.reads.Add(1)-1)))
	}))
	i.internal.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	i.internal.StartTLS()
	t.Cleanup(i.internal.Close)
	ids := func(list []string) []byte {
		items := make([]map[string]any, len(list))
		for n, id := range list {
			items[n] = map[string]any{"value": map[string]any{"id": id}}
		}
		content, err := json.Marshal(items)
		if err != nil {
			t.Fatal(err)
		}
		return content
	}
	i.control = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/routes":
			w.Write(ids(routes))
		case "/v1/upstreams":
			w.Write(ids(upstreams))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(i.control.Close)
	return i
}

// confirm runs edge confirm against the instance and returns its exit code, its standard output and
// its standard error.
func confirm(t *testing.T, r rendered, i *instance, revision string) (int, string, string) {
	t.Helper()
	args := []string{"confirm",
		"--out", r.out.dir,
		"--instance", edgeAddress,
		"--revision", revision,
		"--internal", i.internal.Listener.Addr().String(),
		"--control", i.control.Listener.Addr().String(),
		"--secrets", filepath.Join(r.in.Dir, "secrets"),
	}
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, loadsEverything{}, quickly)
	return code, stdout.String(), stderr.String()
}

func always(answer string) func(int) string { return func(int) string { return answer } }

func TestAnInstanceThatAnswersTheDistributedRevisionAndLoadedEveryItemIsConfirmed(t *testing.T) {
	ca := fixture.NewAuthority(t)
	r := renderForConfirmation(t, ca)
	i := startInstance(t, ca, always(r.answer), r.routes, r.upstreams)

	code, stdout, stderr := confirm(t, r, i, fixture.Revision)

	var answer application.Answer
	if err := json.Unmarshal([]byte(r.answer), &answer); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("edge confirm: %s applied revision %s with digest %s", edgeAddress, fixture.Revision, answer.Digest)
	if code != 0 || !strings.Contains(stdout, want) {
		t.Errorf("exit code %d, stdout %q, stderr %q; want 0 and %q", code, stdout, stderr, want)
	}
	// Every read over one polling interval opened a connection of its own, at most every 20 ms.
	if reads := i.reads.Load(); reads < 5 {
		t.Errorf("the fixed-response route was read %d times over one polling interval, want at least 5", reads)
	}
}

// older is the answer of an instance that still serves an earlier revision.
const older = `{"revision":"fedcba9876543210fedcba9876543210fedcba98","digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111"}`

// Each worker process applies the file on its own, so an instance answers the earlier revision for
// a while and the confirmation waits.
func TestAnInstanceThatAnswersTheEarlierRevisionForAWhileIsConfirmedOnceItAppliedTheNewOne(t *testing.T) {
	ca := fixture.NewAuthority(t)
	r := renderForConfirmation(t, ca)
	i := startInstance(t, ca, func(n int) string {
		if n < 10 || n == 12 { // a worker process that has not applied the file yet
			return older
		}
		return r.answer
	}, r.routes, r.upstreams)

	code, _, stderr := confirm(t, r, i, fixture.Revision)

	if code != 0 || i.reads.Load() < 20 {
		t.Errorf("exit code %d after %d reads, stderr %q; want 0 after more than one polling interval of new answers", code, i.reads.Load(), stderr)
	}
}

func TestAnInstanceThatKeepsAnsweringTheEarlierRevisionHasNotAppliedIt(t *testing.T) {
	ca := fixture.NewAuthority(t)
	r := renderForConfirmation(t, ca)
	i := startInstance(t, ca, always(older), r.routes, r.upstreams)

	started := time.Now()
	code, _, stderr := confirm(t, r, i, fixture.Revision)

	const want = "edge confirm: the instance at 10.0.0.5 has not applied its distributed apisix.yaml: after 2s it answers revision fedcba9876543210fedcba9876543210fedcba98"
	if code != 1 || !strings.Contains(stderr, want) || time.Since(started) < quickly.Timeout {
		t.Errorf("exit code %d after %v, stderr %q; want 1 after the timeout and %q", code, time.Since(started), stderr, want)
	}
}

// The answers prove that the file was read; the control listener proves that no item was dropped.
func TestAnInstanceThatDidNotLoadEveryDistributedItemHasNotAppliedIt(t *testing.T) {
	ca := fixture.NewAuthority(t)
	r := renderForConfirmation(t, ca)
	cases := []struct {
		name              string
		routes, upstreams []string
		want              string
	}{
		{"a dropped route", r.routes[:1], r.upstreams, "it did not load the routes [" + application.RevisionRouteID + "]"},
		{"a dropped upstream", r.routes, nil, "it did not load the upstreams [projects]"},
		{"an upstream that was not distributed", r.routes, append(append([]string{}, r.upstreams...), "billing"),
			"it loaded the upstreams [billing], which the distributed file does not hold"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			i := startInstance(t, ca, always(r.answer), c.routes, c.upstreams)

			code, _, stderr := confirm(t, r, i, fixture.Revision)

			if code != 1 || !strings.Contains(stderr, c.want) {
				t.Errorf("exit code %d, stderr %q; want 1 and %q", code, stderr, c.want)
			}
		})
	}
}

func TestARevisionThatTheDistributedFileDoesNotCarryExitsWithTwo(t *testing.T) {
	ca := fixture.NewAuthority(t)
	r := renderForConfirmation(t, ca)
	i := startInstance(t, ca, always(r.answer), r.routes, r.upstreams)

	code, _, stderr := confirm(t, r, i, "fedcba9876543210fedcba9876543210fedcba98")

	const want = "edge confirm: the distributed apisix.yaml of 10.0.0.5 carries the revision " + fixture.Revision + ", not fedcba9876543210fedcba9876543210fedcba98"
	if code != 2 || !strings.Contains(stderr, want) || i.reads.Load() != 0 {
		t.Errorf("exit code %d after %d reads, stderr %q; want 2 before any read and %q", code, i.reads.Load(), stderr, want)
	}
}

// The internal listener is verified against the installation's certificate authority and the name
// edge, so a listener that presents another name is never believed.
func TestAnInternalListenerWithAnotherNameIsNeverBelieved(t *testing.T) {
	ca := fixture.NewAuthority(t)
	r := renderForConfirmation(t, ca)
	i := startInstance(t, ca, always(r.answer), r.routes, r.upstreams)
	_, _, other := ca.Issue(t, "projects")
	i.internal.TLS.Certificates = []tls.Certificate{other}

	code, _, stderr := confirm(t, r, i, fixture.Revision)

	if code != 1 || !strings.Contains(stderr, "its last read failed") || !strings.Contains(stderr, "certificate") {
		t.Errorf("exit code %d, stderr %q; want 1 and a failed verification", code, stderr)
	}
}
