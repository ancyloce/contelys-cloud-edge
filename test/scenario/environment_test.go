//go:build scenario

// Package scenario holds Edge's verification scenarios (Edge detailed design, section 11). Each
// scenario renders its own files, starts the image that the image target built, and drives Edge at
// its listeners against stubs that run in this test process (Edge ADR 0002). testcontainers-go
// removes the containers, also when a run crashes, and the keys of the throwaway certificate
// authority live only in memory and in temporary directories outside the repository.
package scenario

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.yaml.in/yaml/v3"

	"github.com/contelys/contelys-cloud-edge/internal/application"
	"github.com/contelys/contelys-cloud-edge/internal/infrastructure"
)

const (
	image               = "contelys/edge"
	applicationHostname = "app.example.test"
	edgeAddress         = "edge-1"
	// The container reaches the stubs in this process through Docker's host gateway.
	hostGateway = "host.docker.internal"

	externalPort = "9443/tcp"
	internalPort = "9444/tcp"
	controlPort  = "9090/tcp"
)

// authority is the throwaway certificate authority of one scenario.
type authority struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	pem         []byte
}

func newAuthority(t *testing.T) *authority {
	t.Helper()
	key := generateKey(t)
	template := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: "Edge scenario certificate authority"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &authority{certificate: certificate, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns a certificate for one name, as PEM and as a tls.Certificate.
func (a *authority) issue(t *testing.T, name string) (certificatePEM, keyPEM []byte, pair tls.Certificate) {
	t.Helper()
	key := generateKey(t)
	template := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.certificate, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	pair, err = tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return certificatePEM, keyPEM, pair
}

func (a *authority) pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(a.certificate)
	return pool
}

func generateKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func serial(t *testing.T) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatal(err)
	}
	return n
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
	slug string
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
func startStub(t *testing.T, ca *authority, slug, certificateName string) *stub {
	t.Helper()
	_, _, pair := ca.issue(t, certificateName)
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
	w.Header().Set(stubHeader, s.slug)
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
	ca                    *authority
	stubs                 []*stub
	fragments             map[string]string
	loadBalancerAddresses []string
}

// render writes the inputs into a temporary directory and renders them with the renderer's use case.
// It returns the directory that holds config.yaml and the instance's apisix.yaml.
func (s setup) render(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, content []byte) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	instances := map[string][]application.Instance{application.EdgeSlug: {{Address: edgeAddress, Port: 9443, Weight: 1}}}
	for _, stub := range s.stubs {
		instances[stub.slug] = []application.Instance{{Address: hostGateway, Port: stub.port, Weight: 1}}
	}
	write("instances.yaml", marshal(t, instances))
	addresses := s.loadBalancerAddresses
	if addresses == nil {
		addresses = []string{}
	}
	write("installation.yaml", marshal(t, map[string]any{
		"application_hostname":    applicationHostname,
		"authentication_hostname": "auth.example.test",
		"load_balancer_addresses": addresses,
		"collector_addresses":     []string{},
	}))
	if err := os.MkdirAll(filepath.Join(dir, "fragments"), 0o755); err != nil {
		t.Fatal(err)
	}
	for slug, fragment := range s.fragments {
		write(filepath.Join("fragments", slug, infrastructure.FragmentFile), []byte(fragment))
	}
	certificate, key, _ := s.ca.issue(t, applicationHostname)
	write(filepath.Join("secrets", infrastructure.ApplicationCertificateFile), certificate)
	write(filepath.Join("secrets", infrastructure.ApplicationKeyFile), key)
	write(filepath.Join("secrets", infrastructure.TrustStoreFile), s.ca.pem)

	out := filepath.Join(dir, "out")
	files := infrastructure.Files{}
	err := application.Renderer{Source: files, Sink: files}.Render(application.Request{
		Instances:    filepath.Join(dir, "instances.yaml"),
		Installation: filepath.Join(dir, "installation.yaml"),
		Fragments:    filepath.Join(dir, "fragments"),
		Secrets:      filepath.Join(dir, "secrets"),
		Revision:     "0123456789abcdef0123456789abcdef01234567",
		Out:          out,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return out
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	content, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// edge is one running Edge instance.
type edge struct {
	container *testcontainers.DockerContainer
	ca        *authority
}

// startEdge starts an Edge instance with the rendered files in out, mounted as a deployment mounts
// them: config.yaml and the trust store as files, and the instance's directory, which holds its
// apisix.yaml, at /etc/edge/routes. A file renamed into that directory is applied without a restart.
// The renderer writes apisix.yaml for its owner alone, and the deployment gives it to the user that
// Edge runs as. Here the files are made readable for every user instead.
func startEdge(t *testing.T, ca *authority, out string) *edge {
	t.Helper()
	trustStore := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(trustStore, ca.pem, 0o644); err != nil {
		t.Fatal(err)
	}
	routes := filepath.Join(out, edgeAddress)
	for path, mode := range map[string]os.FileMode{
		routes: 0o755, filepath.Join(routes, "apisix.yaml"): 0o644, filepath.Join(out, "config.yaml"): 0o644,
	} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	c, err := testcontainers.GenericContainer(context.Background(), testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        image,
			ExposedPorts: []string{externalPort, internalPort, controlPort},
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.ExtraHosts = append(hc.ExtraHosts, hostGateway+":host-gateway")
				hc.Binds = append(hc.Binds,
					filepath.Join(out, "config.yaml")+":/usr/local/apisix/conf/config.yaml:ro",
					trustStore+":"+application.TrustStorePath+":ro",
					routes+":/etc/edge/routes:ro",
				)
			},
			WaitingFor: wait.ForHTTP("/v1/routes").WithPort(controlPort).WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatalf("start Edge: %v", err)
	}
	return &edge{container: c.(*testcontainers.DockerContainer), ca: ca}
}

func (e *edge) endpoint(t *testing.T, port string) string {
	t.Helper()
	endpoint, err := e.container.PortEndpoint(context.Background(), port, "")
	if err != nil {
		t.Fatal(err)
	}
	return endpoint
}

// client sends requests for the application hostname to one TLS listener of Edge, and verifies
// Edge's certificate against the scenario's certificate authority.
func (e *edge) client(t *testing.T, port string) *http.Client {
	t.Helper()
	address := e.endpoint(t, port)
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, address)
			},
			TLSClientConfig: &tls.Config{RootCAs: e.ca.pool(), ServerName: applicationHostname},
		},
	}
}

func (e *edge) get(t *testing.T, port, path string, header http.Header) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, "https://"+applicationHostname+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range header {
		request.Header[name] = values
	}
	response, err := e.client(t, port).Do(request)
	if err != nil {
		t.Fatalf("GET %s on %s: %v", path, port, err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

// exec runs a command in the container and returns its output.
func (e *edge) exec(t *testing.T, command ...string) string {
	t.Helper()
	code, reader, err := e.container.Exec(context.Background(), command, tcexec.Multiplexed())
	if err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("%v exited with %d: %s", command, code, output)
	}
	return string(output)
}

// listeningPorts reads the TCP ports that something in the container listens on.
func (e *edge) listeningPorts(t *testing.T) map[int]bool {
	t.Helper()
	ports := map[int]bool{}
	for _, line := range strings.Split(e.exec(t, "cat", "/proc/net/tcp", "/proc/net/tcp6"), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[3] != "0A" { // 0A is LISTEN
			continue
		}
		local := fields[1]
		port, err := strconv.ParseInt(local[strings.LastIndex(local, ":")+1:], 16, 32)
		if err != nil {
			t.Fatal(err)
		}
		ports[int(port)] = true
	}
	return ports
}

// loadedIDs reads the ids of the loaded routes or upstreams from the control listener.
func (e *edge) loadedIDs(t *testing.T, kind string) []string {
	t.Helper()
	response, err := http.Get("http://" + e.endpoint(t, controlPort) + "/v1/" + kind)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
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
