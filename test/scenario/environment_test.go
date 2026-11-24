//go:build scenario

// Package scenario holds Edge's verification scenarios (Edge detailed design, section 11). Each
// scenario renders its own files with the edge command, starts the image that the image target
// built, and drives Edge at its listeners against stubs that run in this test process (Edge ADR
// 0002). testcontainers-go removes the containers, also when a run crashes, and the keys of the
// throwaway certificate authority live only in memory and in temporary directories outside the
// repository.
package scenario

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.yaml.in/yaml/v3"

	"github.com/contelys/contelys-cloud-edge/internal/application"
	"github.com/contelys/contelys-cloud-edge/test/fixture"
)

const (
	// image is the image that the image target built. The renderer's trial load starts it too.
	image       = fixture.Image
	edgeAddress = "edge-1"
	// The container reaches the stubs in this process through Docker's host gateway.
	hostGateway = "host.docker.internal"
)

// Edge's listeners, named by the container port that each listens on.
const (
	externalListener = "9443/tcp"
	internalListener = "9444/tcp"
	controlListener  = "9090/tcp"
	// adminAPI is where APISIX's Admin API would listen; file-driven mode opens no listener there.
	adminAPI = "9180/tcp"
)

// edgeCommand is the edge command, built once for the run, which renders every scenario's files
// through its command line.
var edgeCommand string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "edge-scenario-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	edgeCommand = filepath.Join(dir, "edge")
	build := exec.Command("go", "build", "-o", edgeCommand, "github.com/contelys/contelys-cloud-edge/cmd/edge")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build the edge command:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// The stub's answer, which a scenario expects to return to the client unchanged.
const (
	stubStatus = http.StatusAccepted
	stubHeader = "X-Stub-Answer"
)

// stub is an upstream in this process. It answers every request over TLS with a certificate for one
// name, and records each request byte for byte as it arrived after TLS, with the name that the
// caller asked for in the TLS handshake.
type stub struct {
	slug application.Slug
	port int

	mu       sync.Mutex
	requests []recordedRequest
}

type recordedRequest struct {
	raw        []byte
	serverName string
}

// parsed reads the recorded bytes back as a request.
func (r recordedRequest) parsed(t *testing.T) *http.Request {
	t.Helper()
	request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(r.raw)))
	if err != nil {
		t.Fatalf("the recorded bytes are not a request: %v\n%s", err, r.raw)
	}
	return request
}

// startStub starts a stub for the deployable slug that presents a certificate for certificateName.
func startStub(t *testing.T, ca *fixture.Authority, slug application.Slug, certificateName string) *stub {
	t.Helper()
	_, _, pair := ca.Issue(t, certificateName)
	listener, err := net.Listen("tcp", "0.0.0.0:0") // the container connects through the host gateway
	if err != nil {
		t.Fatal(err)
	}
	s := &stub{slug: slug, port: listener.Addr().(*net.TCPAddr).Port}
	server := &http.Server{
		Handler: http.HandlerFunc(s.answer),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, connectionKey{}, c)
		},
		ReadHeaderTimeout: 10 * time.Second,
	}
	go server.Serve(&recordingListener{Listener: listener, config: &tls.Config{Certificates: []tls.Certificate{pair}}})
	t.Cleanup(func() { server.Close() })
	return s
}

type connectionKey struct{}

func (s *stub) answer(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	conn := r.Context().Value(connectionKey{}).(*recordingConn)
	s.mu.Lock()
	s.requests = append(s.requests, recordedRequest{raw: conn.take(), serverName: conn.ConnectionState().ServerName})
	s.mu.Unlock()
	w.Header().Set(stubHeader, string(s.slug))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(stubStatus)
	fmt.Fprintf(w, "answer of %s\n", s.slug)
}

func (s *stub) recorded() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedRequest(nil), s.requests...)
}

// recordingListener ends TLS and hands the server connections that record what they read.
type recordingListener struct {
	net.Listener
	config *tls.Config
}

func (l *recordingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &recordingConn{Conn: tls.Server(conn, l.config)}, nil
}

type recordingConn struct {
	*tls.Conn
	mu   sync.Mutex
	read []byte
}

func (c *recordingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	c.read = append(c.read, p[:n]...)
	c.mu.Unlock()
	return n, err
}

// take returns the bytes read since the last call.
func (c *recordingConn) take() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	taken := c.read
	c.read = nil
	return taken
}

// setup is the renderer's input for one scenario.
type setup struct {
	ca                    *fixture.Authority
	stubs                 []*stub
	fragments             map[application.Slug]string
	loadBalancerAddresses []string
}

// render writes the inputs and renders them with the edge command, whose trial load starts the
// image. It returns the directory that holds config.yaml and the instance's apisix.yaml.
func (s setup) render(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out")
	if output, err := s.renderInto(t, out); err != nil {
		t.Fatalf("edge render: %v: %s", err, output)
	}
	return out
}

// renderInto renders the inputs into out, which may hold the files of a running instance, and
// returns what the edge command wrote and the error of its exit status. Each deployable except
// Authoring gets an OpenAPI contract that holds the paths of its fragment, a prefix such as
// /api/v1/projects/* as /api/v1/projects/{id}.
func (s setup) renderInto(t *testing.T, out string) (string, error) {
	t.Helper()
	in := fixture.NewInputs(t, s.ca, edgeAddress)
	instances := application.Instances{application.EdgeSlug: {{Address: edgeAddress, Port: 9443, Weight: 1}}}
	for _, stub := range s.stubs {
		instances[stub.slug] = []application.Instance{{Address: hostGateway, Port: stub.port, Weight: 1}}
	}
	in.Instances(t, instances)
	in.Installation(t, s.loadBalancerAddresses...)
	for slug, fragment := range s.fragments {
		in.Fragment(t, string(slug), fragment)
		if slug != application.AuthoringSlug {
			in.Contract(t, string(slug), contractPaths(t, fragment)...)
		}
	}
	output, err := exec.Command(edgeCommand, in.Args(out)...).CombinedOutput()
	return string(output), err
}

// contractPaths are the path templates of a contract that holds every path of a fragment.
func contractPaths(t *testing.T, fragment string) []string {
	t.Helper()
	var decoded struct {
		Routes []struct {
			Paths []string `yaml:"paths"`
		} `yaml:"routes"`
	}
	if err := yaml.Unmarshal([]byte(fragment), &decoded); err != nil {
		t.Fatalf("the scenario's fragment: %v", err)
	}
	var paths []string
	for _, route := range decoded.Routes {
		for _, path := range route.Paths {
			if base, isPrefix := strings.CutSuffix(path, "*"); isPrefix {
				path = base + "{id}"
			}
			paths = append(paths, path)
		}
	}
	return paths
}

// edge is one running Edge instance.
type edge struct {
	container *testcontainers.DockerContainer
	ca        *fixture.Authority
}

// startEdge starts an Edge instance with the rendered files in out, mounted as a deployment mounts
// them: config.yaml and the trust store as files, and the instance's directory, which holds its
// apisix.yaml, at /etc/edge/routes. A file renamed into that directory is applied without a restart.
// The renderer writes apisix.yaml for its owner alone, and the deployment gives it to the user that
// Edge runs as. Here the files are made readable for every user instead.
func startEdge(t *testing.T, ca *fixture.Authority, out string) *edge {
	t.Helper()
	return startEdgeMounting(t, ca, out, directoryMount(out), controlAnswers(http.StatusOK))
}

// routesDirectory is where an Edge instance mounts the directory that holds its apisix.yaml.
const routesDirectory = "/etc/edge/routes"

// routesFile is the instance's apisix.yaml in the rendered output out.
func routesFile(out string) string { return filepath.Join(out, edgeAddress, "apisix.yaml") }

// directoryMount mounts the instance's directory, as a deployment does (Edge ADR 0003).
func directoryMount(out string) string {
	return filepath.Dir(routesFile(out)) + ":" + routesDirectory + ":ro"
}

// fileMount mounts the instance's apisix.yaml itself, the mount that Edge ADR 0003 rejects.
func fileMount(out string) string {
	return routesFile(out) + ":" + routesDirectory + "/apisix.yaml:ro"
}

// controlAnswers is ready when the control listener answers the list of loaded routes with status.
func controlAnswers(status int) wait.Strategy {
	return wait.ForHTTP("/v1/routes").WithPort(controlListener).WithStartupTimeout(60 * time.Second).
		WithStatusCodeMatcher(func(got int) bool { return got == status })
}

// startEdgeMounting starts an Edge instance whose routes come in through the bind mount routesMount,
// and waits until ready holds.
func startEdgeMounting(t *testing.T, ca *fixture.Authority, out, routesMount string, ready wait.Strategy) *edge {
	t.Helper()
	trustStore := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(trustStore, ca.PEM, 0o644); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{
		filepath.Dir(routesFile(out)): 0o755, routesFile(out): 0o644, filepath.Join(out, "config.yaml"): 0o644,
	} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	c, err := testcontainers.GenericContainer(context.Background(), testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        image,
			ExposedPorts: []string{externalListener, internalListener, controlListener, adminAPI},
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.ExtraHosts = append(hc.ExtraHosts, hostGateway+":host-gateway")
				hc.Binds = append(hc.Binds,
					filepath.Join(out, "config.yaml")+":/usr/local/apisix/conf/config.yaml:ro",
					trustStore+":"+application.TrustStorePath+":ro",
					routesMount,
				)
			},
			WaitingFor: ready,
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatalf("start Edge: %v", err)
	}
	return &edge{container: c.(*testcontainers.DockerContainer), ca: ca}
}

// containerAddress is the container's own address on its Docker network.
func (e *edge) containerAddress(t *testing.T) string {
	t.Helper()
	address, err := e.container.ContainerIP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return address
}

// address is where this test process reaches one of Edge's listeners.
func (e *edge) address(t *testing.T, listener string) string {
	t.Helper()
	address, err := e.container.PortEndpoint(context.Background(), listener, "")
	if err != nil {
		t.Fatal(err)
	}
	return address
}

// client sends requests for the application hostname to one TLS listener of Edge, and verifies
// Edge's certificate against the scenario's certificate authority.
func (e *edge) client(t *testing.T, listener string) *http.Client {
	t.Helper()
	address := e.address(t, listener)
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, address)
			},
			TLSClientConfig: &tls.Config{RootCAs: e.ca.Pool(), ServerName: fixture.ApplicationHostname},
		},
	}
}

func (e *edge) get(t *testing.T, listener, path string, header http.Header) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, "https://"+fixture.ApplicationHostname+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range header {
		request.Header[name] = values
	}
	response, err := e.client(t, listener).Do(request)
	if err != nil {
		t.Fatalf("GET %s on %s: %v", path, listener, err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

// control sends a request to the control listener, as the VM's probe does.
func (e *edge) control(t *testing.T, path string) *http.Response {
	t.Helper()
	response, err := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + e.address(t, controlListener) + path)
	if err != nil {
		t.Fatalf("GET %s on the control listener: %v", path, err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

// loadedIDs reads the ids of the loaded routes or upstreams from the control listener.
func (e *edge) loadedIDs(t *testing.T, kind string) []string {
	t.Helper()
	body, err := io.ReadAll(e.control(t, "/v1/"+kind).Body)
	if err != nil {
		t.Fatal(err)
	}
	var items []struct {
		Value struct {
			ID string `json:"id"`
		} `json:"value"`
	}
	// APISIX encodes an empty list as an empty object, or answers with no body at all.
	if trimmed := strings.TrimSpace(string(body)); trimmed != "" && trimmed != "{}" {
		if err := json.Unmarshal(body, &items); err != nil {
			t.Fatalf("/v1/%s: %v: %s", kind, err, body)
		}
	}
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.Value.ID
	}
	return ids
}

// renderedIDs reads the ids of the rendered routes or upstreams from an apisix.yaml.
func renderedIDs(t *testing.T, out, kind string) []string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(out, edgeAddress, "apisix.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var file map[string][]struct {
		ID string `yaml:"id"`
	}
	if err := yaml.Unmarshal(content, &file); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, item := range file[kind] {
		ids = append(ids, item.ID)
	}
	return ids
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	content, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return content
}
