//go:build scenario

package scenario

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/ancyloce/contelys-cloud-edge/internal/application"
	"github.com/ancyloce/contelys-cloud-edge/test/fixture"
)

const projectsFragment = `
routes:
  - paths: [/api/v1/projects, /api/v1/projects/*]
    methods: [GET, POST]
    auth: anonymous
`

func TestAnAnonymousRequestThroughTheExternalListenerReachesItsDeployableAndReturnsUnchanged(t *testing.T) {
	t.Parallel()
	ca := fixture.NewAuthority(t)
	projects := startStub(t, ca, "projects", "projects")
	out := setup{ca: ca, stubs: []*stub{projects}, fragments: map[application.Slug]string{"projects": projectsFragment}}.render(t)
	edge := startEdge(t, ca, out)

	response := edge.get(t, externalListener, "/api/v1/projects/42?view=full", http.Header{"X-Example": {"kept"}})

	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != stubStatus || response.Header.Get(stubHeader) != "projects" || string(body) != "answer of projects\n" {
		t.Errorf("the stub's answer changed on its way: %d %v %q", response.StatusCode, response.Header, body)
	}
	if got := response.Header.Get("Strict-Transport-Security"); got != "max-age=31536000" {
		t.Errorf("Strict-Transport-Security: got %q", got)
	}
	recorded := projects.recorded()
	if len(recorded) != 1 {
		t.Fatalf("the stub received %d requests, want 1", len(recorded))
	}
	request := recorded[0].parsed(t)
	if recorded[0].serverName != "projects" {
		t.Errorf("TLS server name: got %q, want the slug", recorded[0].serverName)
	}
	if request.Method != http.MethodGet || request.RequestURI != "/api/v1/projects/42?view=full" {
		t.Errorf("request line: got %s %s", request.Method, request.RequestURI)
	}
	if request.Host != "projects" {
		t.Errorf("Host: got %q, want the name derived from the slug", request.Host)
	}
	if got := request.Header.Get("X-Forwarded-Host"); got != fixture.ApplicationHostname {
		t.Errorf("X-Forwarded-Host: got %q, want the original host", got)
	}
	if got := request.Header.Get("X-Example"); got != "kept" {
		t.Errorf("X-Example: got %q", got)
	}
}

const authoringFragment = `
routes:
  - paths: [/*]
    methods: [GET]
    auth: anonymous
`

// Design section 03: Authoring answers every path outside /api/ that no other route claims. An
// unclaimed path under /api/ gets Edge's 404, and so does a path under /internal, which no fragment
// may claim, whatever its case and also percent-encoded.
func TestAuthoringAnswersTheRemainderOutsideTheAPIAndNeverAnInternalPath(t *testing.T) {
	t.Parallel()
	ca := fixture.NewAuthority(t)
	authoring := startStub(t, ca, application.AuthoringSlug, string(application.AuthoringSlug))
	projects := startStub(t, ca, "projects", "projects")
	out := setup{ca: ca, stubs: []*stub{authoring, projects}, fragments: map[application.Slug]string{
		application.AuthoringSlug: authoringFragment,
		"projects":                projectsFragment,
	}}.render(t)
	edge := startEdge(t, ca, out)

	answered := map[string]string{"/": "authoring", "/editor/page": "authoring", "/apiary": "authoring", "/api/v1/projects/42": "projects"}
	for path, slug := range answered {
		response := edge.get(t, externalListener, path, nil)
		if response.StatusCode != stubStatus || response.Header.Get(stubHeader) != slug {
			t.Errorf("GET %s: got %d from %q, want %d from %s", path, response.StatusCode, response.Header.Get(stubHeader), stubStatus, slug)
		}
	}
	for _, path := range []string{"/api", "/api/", "/api/v1/unknown", "/internal", "/internal/v1/authentication", "/INTERNAL/v1/authentication", "/%69nternal/v1/authentication"} {
		response := edge.get(t, externalListener, path, nil)
		if response.StatusCode != http.StatusNotFound || response.Header.Get(stubHeader) != "" {
			t.Errorf("GET %s: got %d from %q, want Edge's 404", path, response.StatusCode, response.Header.Get(stubHeader))
		}
	}
	if got := len(authoring.recorded()); got != 3 {
		t.Errorf("Authoring received %d requests, want the 3 outside /api/", got)
	}
}

func TestAnUpstreamWhoseCertificateCarriesAnotherNameIsNeverCalled(t *testing.T) {
	t.Parallel()
	ca := fixture.NewAuthority(t)
	impostor := startStub(t, ca, "projects", "billing-entitlements") // a valid certificate of the installation, for another name
	out := setup{ca: ca, stubs: []*stub{impostor}, fragments: map[application.Slug]string{"projects": projectsFragment}}.render(t)
	edge := startEdge(t, ca, out)

	if got := edge.get(t, externalListener, "/api/v1/projects", nil).StatusCode; got != http.StatusBadGateway {
		t.Errorf("status: got %d, want 502", got)
	}
	if recorded := impostor.recorded(); len(recorded) != 0 {
		t.Errorf("the upstream received %d requests over a connection that should not verify", len(recorded))
	}
}

func TestBehindALoadBalancerTheExternalListenerAcceptsOnlyThePROXYProtocol(t *testing.T) {
	t.Parallel()
	ca := fixture.NewAuthority(t)
	projects := startStub(t, ca, "projects", "projects")
	out := setup{
		ca: ca, stubs: []*stub{projects}, fragments: map[application.Slug]string{"projects": projectsFragment},
		loadBalancerAddresses: []string{"192.0.2.10"},
	}.render(t)
	edge := startEdge(t, ca, out)
	address := edge.address(t, externalListener)
	config := &tls.Config{RootCAs: ca.Pool(), ServerName: fixture.ApplicationHostname}

	direct, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", address, config)
	if err == nil {
		direct.Close()
		t.Error("a TLS connection without a PROXY protocol header was accepted")
	}

	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			if _, err := io.WriteString(conn, "PROXY TCP4 198.51.100.7 192.0.2.20 40000 443\r\n"); err != nil {
				return nil, err
			}
			return tls.Client(conn, config), nil
		},
	}}
	response, err := client.Get("https://" + fixture.ApplicationHostname + "/api/v1/projects")
	if err != nil {
		t.Fatalf("a request behind a PROXY protocol header: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != stubStatus {
		t.Errorf("status: got %d, want the stub's %d", response.StatusCode, stubStatus)
	}
}
