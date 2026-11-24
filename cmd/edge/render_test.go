package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	edgeAddress  = "10.0.0.5"
	externalPort = 9443
)

func TestAnAnonymousRouteIsServedOnTheApplicationHostnameByItsDeployable(t *testing.T) {
	in := newInputs(t)
	in.fragment(t, "projects", `
routes:
  - paths: [/api/v1/projects, /api/v1/projects/*]
    methods: [GET, HEAD]
    auth: anonymous
`)

	file := in.renderOK(t).apisix(t, edgeAddress)

	route := file.routeWithURIs(t, "/api/v1/projects", "/api/v1/projects/*")
	equal(t, "host", route["host"], "app.example.test")
	equal(t, "methods", route["methods"], []any{"GET", "HEAD"})
	equal(t, "listener condition", route["vars"], []any{[]any{"server_port", "==", externalPort}})

	upstream := file.upstream(t, route["upstream_id"])
	equal(t, "scheme", upstream["scheme"], "https")
	equal(t, "nodes", upstream["nodes"], []any{
		map[string]any{"host": "10.0.1.1", "port": 8443, "weight": 1},
		map[string]any{"host": "projects-2.internal.test", "port": 8443, "weight": 0},
	})
	equal(t, "pass_host", upstream["pass_host"], "rewrite")
	equal(t, "upstream_host", upstream["upstream_host"], "projects")
	equal(t, "tls", upstream["tls"], map[string]any{"verify": true})
	equal(t, "retries", upstream["retries"], 0)
}

func TestTheSameInputsGiveByteIdenticalFiles(t *testing.T) {
	in := newInputs(t)
	in.fragment(t, "projects", `
routes:
  - {paths: [/api/v1/projects/*], methods: [GET], auth: anonymous}
  - {paths: [/api/v1/projects], methods: [POST, GET], auth: anonymous, traffic: transfer}
`)
	in.fragment(t, "authoring", `
routes:
  - {paths: [/*], methods: [GET, HEAD], auth: anonymous}
`)

	first, second := in.renderOK(t), in.renderOK(t)

	for _, name := range []string{"config.yaml", filepath.Join(edgeAddress, "apisix.yaml")} {
		if !bytes.Equal(first.read(t, name), second.read(t, name)) {
			t.Errorf("%s differs between two renderings of the same inputs", name)
		}
	}
}

func TestApisixYamlCarriesTheApplicationCertificateAndEndsWithTheMarker(t *testing.T) {
	in := newInputs(t)
	in.fragment(t, "projects", `
routes:
  - {paths: [/api/v1/projects], methods: [GET], auth: anonymous}
`)

	out := in.renderOK(t)

	content := out.read(t, filepath.Join(edgeAddress, "apisix.yaml"))
	if !bytes.HasSuffix(content, []byte("\n#END\n")) {
		t.Errorf("apisix.yaml does not end with the #END marker: %q", content[max(0, len(content)-40):])
	}
	ssls := out.apisix(t, edgeAddress).items(t, "ssls")
	if len(ssls) != 1 {
		t.Fatalf("got %d certificates, want 1", len(ssls))
	}
	equal(t, "snis", ssls[0]["snis"], []any{"app.example.test"})
	equal(t, "cert", ssls[0]["cert"], string(in.read(t, "secrets/application.crt")))
	equal(t, "key", ssls[0]["key"], string(in.read(t, "secrets/application.key")))
}

func TestAFragmentThatBreaksTheSchemaOrNeedsWhatEdgeLacksIsRefusedAndNothingIsWritten(t *testing.T) {
	cases := []struct {
		name, fragment, rule string
	}{
		{"an unknown field", `
routes:
  - {paths: [/api/v1/projects], methods: [GET], auth: anonymous, upstream: 10.9.9.9}
`, "at /routes/0: additional properties 'upstream' not allowed"},
		{"an unknown value", `
routes:
  - {paths: [/api/v1/projects], methods: [GET], auth: anonymous, traffic: bulk}
`, "at /routes/0/traffic: value must be one of"},
		{"a path that is not a path", `
routes:
  - {paths: [api/v1/projects], methods: [GET], auth: anonymous}
`, "at /routes/0/paths/0: "},
		{"a route without methods", `
routes:
  - {paths: [/api/v1/projects], auth: anonymous}
`, "at /routes/0: missing property 'methods'"},
		{"an authenticated route, which needs forward-auth", `
routes:
  - {paths: [/api/v1/projects], methods: [GET]}
`, `route 1: auth "required" is not rendered yet`},
		{"a WebSocket route", `
routes:
  - {paths: [/api/v1/projects/live], methods: [GET], auth: anonymous, traffic: websocket}
`, `route 1: traffic "websocket" is not rendered yet`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := newInputs(t)
			in.fragment(t, "projects", c.fragment)

			code, stderr, out := in.render(t)

			if code != 1 {
				t.Errorf("exit code %d, want 1; stderr: %s", code, stderr)
			}
			if !strings.Contains(stderr, "fragment projects: ") || !strings.Contains(stderr, c.rule) {
				t.Errorf("the message names neither the fragment nor the rule %q: %s", c.rule, stderr)
			}
			if _, err := os.Stat(out.dir); !os.IsNotExist(err) {
				t.Errorf("a refused rendering wrote %s", out.dir)
			}
		})
	}
}

func TestAMissingInputIsAUsageError(t *testing.T) {
	in := newInputs(t)
	args := in.args(t.TempDir())
	var stdout, stderr bytes.Buffer

	code := run(args[:len(args)-2], &stdout, &stderr) // without --out

	if code != 2 || !strings.Contains(stderr.String(), "--out is required") {
		t.Errorf("exit code %d, stderr %q; want 2 and the missing flag named", code, stderr.String())
	}
}

func TestConfigYamlRunsFileDrivenModeWithTheListenersOfTheDesign(t *testing.T) {
	config := newInputs(t).renderOK(t).config(t)

	equal(t, "deployment", config["deployment"], map[string]any{
		"role":            "data_plane",
		"role_data_plane": map[string]any{"config_provider": "yaml"},
	})
	apisix := config["apisix"].(map[string]any)
	equal(t, "loopback listener", apisix["node_listen"], []any{map[string]any{"ip": "127.0.0.1", "port": 9080}})
	equal(t, "TLS listeners", apisix["ssl"], map[string]any{
		"enable":                  true,
		"listen":                  []any{map[string]any{"port": externalPort}, map[string]any{"port": 9444}},
		"ssl_protocols":           "TLSv1.2 TLSv1.3",
		"ssl_trusted_certificate": "/etc/edge/ca.crt",
	})
	equal(t, "PROXY protocol", apisix["proxy_protocol"], nil)
	equal(t, "control listener", apisix["control"], map[string]any{"ip": "0.0.0.0", "port": 9090})
	equal(t, "trusted addresses", apisix["trusted_addresses"], []any{"127.0.0.1"})

	nginx := config["nginx_config"].(map[string]any)
	equal(t, "worker processes", nginx["worker_processes"], "auto")
	equal(t, "worker connections", nginx["event"], map[string]any{"worker_connections": 10620})
	equal(t, "open files", nginx["worker_rlimit_nofile"], 20480)
	equal(t, "drain", nginx["worker_shutdown_timeout"], "120s")
	equal(t, "header sizes", nginx["http_configuration_snippet"],
		"client_header_buffer_size 4k;\nlarge_client_header_buffers 4 16k;\nproxy_buffer_size 16k;\nproxy_busy_buffers_size 16k;\n")

	equal(t, "plugins", config["plugins"], []any{
		"client-control", "forward-auth", "limit-conn", "limit-count", "limit-req", "opentelemetry",
		"prometheus", "proxy-rewrite", "public-api", "real-ip", "request-id", "response-rewrite",
	})
	equal(t, "plugin_attr", config["plugin_attr"], map[string]any{
		"forward-auth": map[string]any{"http_client": "ngx_http_ffi_client"},
		"prometheus":   map[string]any{"enable_export_server": false},
	})
}

func TestBehindALoadBalancerTheExternalListenerAcceptsOnlyThePROXYProtocol(t *testing.T) {
	in := newInputs(t)
	in.write(t, "installation.yaml", `
application_hostname: app.example.test
authentication_hostname: auth.example.test
load_balancer_addresses: [10.0.9.1, 10.0.9.2]
collector_addresses: []
`)

	apisix := in.renderOK(t).config(t)["apisix"].(map[string]any)

	equal(t, "TLS listeners", apisix["ssl"].(map[string]any)["listen"], []any{map[string]any{"port": 9444}})
	equal(t, "PROXY protocol", apisix["proxy_protocol"], map[string]any{"listen_https_port": externalPort})
}

// inputs is a complete set of the renderer's inputs in a temporary directory.
type inputs struct {
	dir string
}

func newInputs(t *testing.T) *inputs {
	t.Helper()
	in := &inputs{dir: t.TempDir()}
	in.write(t, "instances.yaml", `
edge:
  - {address: 10.0.0.5, port: 9443, weight: 1}
projects:
  - {address: 10.0.1.1, port: 8443, weight: 1}
  - {address: projects-2.internal.test, port: 8443, weight: 0}
authoring:
  - {address: 10.0.2.1, port: 8443, weight: 1}
`)
	in.write(t, "installation.yaml", `
application_hostname: app.example.test
authentication_hostname: auth.example.test
load_balancer_addresses: []
collector_addresses: []
`)
	if err := os.MkdirAll(filepath.Join(in.dir, "fragments"), 0o755); err != nil {
		t.Fatal(err)
	}
	certificate, key := selfSigned(t, "app.example.test")
	in.write(t, "secrets/application.crt", certificate)
	in.write(t, "secrets/application.key", key)
	in.write(t, "secrets/ca.crt", certificate)
	return in
}

func (in *inputs) write(t *testing.T, name, content string) {
	t.Helper()
	path := filepath.Join(in.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimLeft(content, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (in *inputs) read(t *testing.T, name string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(in.dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func (in *inputs) fragment(t *testing.T, slug, content string) {
	t.Helper()
	in.write(t, filepath.Join("fragments", slug, "route-fragment.yaml"), content)
}

func (in *inputs) args(out string) []string {
	return []string{"render",
		"--instances", filepath.Join(in.dir, "instances.yaml"),
		"--installation", filepath.Join(in.dir, "installation.yaml"),
		"--fragments", filepath.Join(in.dir, "fragments"),
		"--secrets", filepath.Join(in.dir, "secrets"),
		"--revision", "0123456789abcdef0123456789abcdef01234567",
		"--out", out,
	}
}

// render runs the command and returns its exit code, its standard error and the output directory.
func (in *inputs) render(t *testing.T) (int, string, output) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out")
	var stdout, stderr bytes.Buffer
	code := run(in.args(out), &stdout, &stderr)
	return code, stderr.String(), output{dir: out}
}

func (in *inputs) renderOK(t *testing.T) output {
	t.Helper()
	code, stderr, out := in.render(t)
	if code != 0 {
		t.Fatalf("render exited with %d: %s", code, stderr)
	}
	return out
}

type output struct {
	dir string
}

func (o output) read(t *testing.T, name string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(o.dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func (o output) apisix(t *testing.T, address string) apisixFile {
	t.Helper()
	var file map[string]any
	if err := yaml.Unmarshal(o.read(t, filepath.Join(address, "apisix.yaml")), &file); err != nil {
		t.Fatal(err)
	}
	return apisixFile(file)
}

func (o output) config(t *testing.T) map[string]any {
	t.Helper()
	var file map[string]any
	if err := yaml.Unmarshal(o.read(t, "config.yaml"), &file); err != nil {
		t.Fatal(err)
	}
	return file
}

type apisixFile map[string]any

func (f apisixFile) items(t *testing.T, kind string) []map[string]any {
	t.Helper()
	list, _ := f[kind].([]any)
	items := make([]map[string]any, 0, len(list))
	for _, item := range list {
		items = append(items, item.(map[string]any))
	}
	return items
}

func (f apisixFile) routeWithURIs(t *testing.T, uris ...string) map[string]any {
	t.Helper()
	want := make([]any, len(uris))
	for i, uri := range uris {
		want[i] = uri
	}
	for _, route := range f.items(t, "routes") {
		if reflect.DeepEqual(route["uris"], want) {
			return route
		}
	}
	t.Fatalf("no route with the uris %v in %v", uris, f["routes"])
	return nil
}

func (f apisixFile) upstream(t *testing.T, id any) map[string]any {
	t.Helper()
	for _, upstream := range f.items(t, "upstreams") {
		if upstream["id"] == id {
			return upstream
		}
	}
	t.Fatalf("no upstream with the id %v in %v", id, f["upstreams"])
	return nil
}

func equal(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: got %#v, want %#v", what, got, want)
	}
}

// selfSigned returns a PEM certificate and key for one name. The tests generate their keys, so
// that no key is tracked by Git.
func selfSigned(t *testing.T, name string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}
