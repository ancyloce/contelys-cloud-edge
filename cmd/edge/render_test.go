package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/contelys/contelys-cloud-edge/test/fixture"
)

// These tests drive the edge render command through its files, end to end, and read the output as
// APISIX reads it. The rules of each layer have their own failing fixtures beside the code.

const (
	edgeAddress          = "10.0.0.5"
	externalListenerPort = 9443
)

// newInputs is a complete set of inputs with the instances of two deployables.
func newInputs(t *testing.T) *fixture.Inputs {
	t.Helper()
	in := fixture.NewInputs(t, fixture.NewAuthority(t), edgeAddress)
	in.Write(t, "instances.yaml", `
edge:
  - {address: 10.0.0.5, port: 9443, weight: 1}
projects:
  - {address: 10.0.1.1, port: 8443, weight: 1}
  - {address: projects-2.internal.test, port: 8443, weight: 0}
authoring:
  - {address: 10.0.2.1, port: 8443, weight: 1}
`)
	return in
}

func TestAnAnonymousRouteIsServedOnTheApplicationHostnameByItsDeployable(t *testing.T) {
	in := newInputs(t)
	in.Fragment(t, "projects", `
routes:
  - paths: [/api/v1/projects, /api/v1/projects/*]
    methods: [GET, HEAD]
    auth: anonymous
`)

	file := renderOK(t, in).apisix(t, edgeAddress)

	route := file.routeWithURIs(t, "/api/v1/projects", "/api/v1/projects/*")
	equal(t, "id", route["id"], "external.projects.ff92e7c5e18a")
	equal(t, "name", route["name"], "projects GET,HEAD /api/v1/projects /api/v1/projects/*")
	equal(t, "host", route["host"], fixture.ApplicationHostname)
	equal(t, "methods", route["methods"], []any{"GET", "HEAD"})
	equal(t, "listener condition", route["vars"], []any{[]any{"server_port", "==", externalListenerPort}})

	upstream := file.upstream(t, route["upstream_id"])
	equal(t, "upstream id", upstream["id"], "projects")
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

func TestRoutesOnTheSamePathsWithOtherMethodsGetTheirOwnIds(t *testing.T) {
	in := newInputs(t)
	in.Fragment(t, "projects", `
routes:
  - {paths: [/api/v1/projects], methods: [GET], auth: anonymous}
  - {paths: [/api/v1/projects], methods: [POST, DELETE], auth: anonymous}
`)

	var ids, names []any
	for _, route := range renderOK(t, in).apisix(t, edgeAddress).items(t, "routes") {
		ids = append(ids, route["id"])
		names = append(names, route["name"])
	}

	// The first 12 hexadecimal digits of the SHA-256 of the sorted paths and then the sorted methods,
	// one per line.
	equal(t, "ids", ids, []any{"external.projects.cec1c806d8c4", "external.projects.0d8dcad58eaf"})
	// Metrics are labelled by route name, so the names differ too.
	equal(t, "names", names, []any{"projects GET /api/v1/projects", "projects POST,DELETE /api/v1/projects"})
}

func TestTheSameInputsGiveByteIdenticalFiles(t *testing.T) {
	in := newInputs(t)
	in.Fragment(t, "projects", `
routes:
  - {paths: [/api/v1/projects/*], methods: [GET], auth: anonymous}
  - {paths: [/api/v1/projects], methods: [POST, GET], auth: anonymous, traffic: transfer}
`)
	in.Fragment(t, "authoring", `
routes:
  - {paths: [/*], methods: [GET, HEAD], auth: anonymous}
`)

	first, second := renderOK(t, in), renderOK(t, in)

	for _, name := range []string{"config.yaml", filepath.Join(edgeAddress, "apisix.yaml")} {
		if !bytes.Equal(first.read(t, name), second.read(t, name)) {
			t.Errorf("%s differs between two renderings of the same inputs", name)
		}
	}
}

func TestApisixYamlCarriesTheApplicationCertificateAndEndsWithTheMarker(t *testing.T) {
	in := newInputs(t)
	in.Fragment(t, "projects", `
routes:
  - {paths: [/api/v1/projects], methods: [GET], auth: anonymous}
`)

	out := renderOK(t, in)

	content := out.read(t, filepath.Join(edgeAddress, "apisix.yaml"))
	if !bytes.HasSuffix(content, []byte("\n#END\n")) {
		t.Errorf("apisix.yaml does not end with the #END marker: %q", content[max(0, len(content)-40):])
	}
	ssls := out.apisix(t, edgeAddress).items(t, "ssls")
	if len(ssls) != 1 {
		t.Fatalf("got %d certificates, want 1", len(ssls))
	}
	equal(t, "snis", ssls[0]["snis"], []any{fixture.ApplicationHostname})
	equal(t, "cert", ssls[0]["cert"], string(in.Read(t, "secrets/application.crt")))
	equal(t, "key", ssls[0]["key"], string(in.Read(t, "secrets/application.key")))
}

func TestConfigYamlRunsFileDrivenModeWithTheListenersOfTheDesign(t *testing.T) {
	config := renderOK(t, newInputs(t)).config(t)

	equal(t, "deployment", config["deployment"], map[string]any{
		"role":            "data_plane",
		"role_data_plane": map[string]any{"config_provider": "yaml"},
	})
	apisix := config["apisix"].(map[string]any)
	equal(t, "loopback listener", apisix["node_listen"], []any{map[string]any{"ip": "127.0.0.1", "port": 9080}})
	equal(t, "TLS listeners", apisix["ssl"], map[string]any{
		"enable":                  true,
		"listen":                  []any{map[string]any{"port": externalListenerPort}, map[string]any{"port": 9444}},
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
	in.Installation(t, "10.0.9.1", "10.0.9.2")

	apisix := renderOK(t, in).config(t)["apisix"].(map[string]any)

	equal(t, "TLS listeners", apisix["ssl"].(map[string]any)["listen"], []any{map[string]any{"port": 9444}})
	equal(t, "PROXY protocol", apisix["proxy_protocol"], map[string]any{"listen_https_port": externalListenerPort})
}

func TestARefusedRenderingExitsWithOneAndWritesNothing(t *testing.T) {
	in := newInputs(t)
	in.Fragment(t, "projects", `
routes:
  - {paths: [/api/v1/projects], methods: [GET], auth: anonymous, upstream: 10.9.9.9}
`)

	code, stderr, out := render(t, in)

	if code != 1 || !strings.Contains(stderr, "refused: fragment projects: ") {
		t.Errorf("exit code %d, stderr %q; want 1 and the fragment named", code, stderr)
	}
	if _, err := os.Stat(out.dir); !os.IsNotExist(err) {
		t.Errorf("a refused rendering wrote %s", out.dir)
	}
}

func TestAMissingInputIsAUsageError(t *testing.T) {
	args := newInputs(t).Args(t.TempDir())
	var stdout, stderr bytes.Buffer

	code := run(args[:len(args)-2], &stdout, &stderr) // without --out

	if code != 2 || !strings.Contains(stderr.String(), "--out is required") {
		t.Errorf("exit code %d, stderr %q; want 2 and the missing flag named", code, stderr.String())
	}
}

// render runs the command and returns its exit code, its standard error and the output directory.
func render(t *testing.T, in *fixture.Inputs) (int, string, output) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out")
	var stdout, stderr bytes.Buffer
	code := run(in.Args(out), &stdout, &stderr)
	return code, stderr.String(), output{dir: out}
}

func renderOK(t *testing.T, in *fixture.Inputs) output {
	t.Helper()
	code, stderr, out := render(t, in)
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
