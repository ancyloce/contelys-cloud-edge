package infrastructure_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/contelys/contelys-cloud-edge/internal/application"
	"github.com/contelys/contelys-cloud-edge/internal/infrastructure"
	"github.com/contelys/contelys-cloud-edge/test/fixture"
)

// The rules of reading the input files, each with a failing fixture: strict decoding, the fragment
// schema, the contracts and the secret files.

func newInputs(t *testing.T) *fixture.Inputs {
	t.Helper()
	in := fixture.NewInputs(t, fixture.NewAuthority(t), "10.0.0.5")
	in.Fragment(t, "projects", `
routes:
  - {paths: [/api/v1/projects, /api/v1/projects/*], methods: [GET], auth: anonymous, traffic: transfer}
`)
	in.Contract(t, "projects", "/api/v1/projects/{projectId}", "/api/v1/projects")
	return in
}

func read(in *fixture.Inputs) (application.Inputs, error) {
	return infrastructure.Files{}.Read(in.Request(""))
}

func TestCompleteInputsAreRead(t *testing.T) {
	in := newInputs(t)
	if err := os.MkdirAll(filepath.Join(in.Dir, "fragments", "notifications"), 0o755); err != nil {
		t.Fatal(err) // a deployable without external routes has a directory and no fragment
	}
	in.Fragment(t, "authoring", `
routes:
  - {paths: ["/*"], methods: [GET], auth: anonymous}
`)
	// A contract may span several documents, in YAML or JSON. A file without an openapi key is not
	// one of them.
	in.Write(t, "contracts/projects/members.json", `{"openapi": "3.1.0", "paths": {"/api/v1/projects/{projectId}/members": {}}}`)
	in.Write(t, "contracts/projects/schemas.yaml", "paths:\n  /api/v1/not-a-contract: {}\n")

	inputs, err := read(in)

	if err != nil {
		t.Fatal(err)
	}
	want := []application.Fragment{
		{Slug: "authoring", Routes: []application.FragmentRoute{{
			Paths: []string{"/*"}, Methods: []string{"GET"}, Auth: application.AuthAnonymous,
		}}},
		{Slug: "projects", Routes: []application.FragmentRoute{{
			Paths: []string{"/api/v1/projects", "/api/v1/projects/*"}, Methods: []string{"GET"},
			Auth: application.AuthAnonymous, Traffic: application.TrafficTransfer,
		}}},
	}
	if !reflect.DeepEqual(inputs.Fragments, want) {
		t.Errorf("fragments: got %+v, want %+v", inputs.Fragments, want)
	}
	// Authoring has no contract, and so no entry.
	wantContracts := map[application.Slug][]string{"projects": {
		"/api/v1/projects", "/api/v1/projects/{projectId}", "/api/v1/projects/{projectId}/members",
	}}
	if !reflect.DeepEqual(inputs.Contracts, wantContracts) {
		t.Errorf("contracts: got %v, want %v", inputs.Contracts, wantContracts)
	}
	if got := inputs.Instances[application.EdgeSlug]; len(got) != 1 || got[0].Address != "10.0.0.5" {
		t.Errorf("Edge's instances: got %+v", got)
	}
	if inputs.Installation.ApplicationHostname != fixture.ApplicationHostname || inputs.Defaults.Listeners.ExternalPort != 9443 {
		t.Errorf("installation %+v, defaults %+v", inputs.Installation, inputs.Defaults.Listeners)
	}
	if !strings.HasPrefix(inputs.Secrets.ApplicationKey, "-----BEGIN PRIVATE KEY-----") {
		t.Error("the application key was not read")
	}
	if inputs.Secrets.TrustStore != string(in.Read(t, "secrets/ca.crt")) {
		t.Error("the trust store was not read")
	}
}

func TestEachReadingRuleRefusesItsFixture(t *testing.T) {
	ca := fixture.NewAuthority(t)
	otherCertificate, _, _ := ca.Issue(t, fixture.ApplicationHostname)
	const fragmentFile = "fragments/projects/route-fragment.yaml"
	cases := []struct {
		name, file, content string
		refusal             bool // a refusal names a fragment; any other failure is an error of the inputs
		message             string
	}{
		{"an unknown key in the instances file", "instances.yaml", `
edge:
  - {address: 10.0.0.5, port: 9443, weight: 1, zone: a}
`, false, "field zone not found"},
		{"an instances file that is not YAML", "instances.yaml", "edge: [", false, "instances.yaml: yaml:"},
		{"an unknown key in the installation file", "installation.yaml", `
application_hostname: app.example.test
authentication_hostname: auth.example.test
load_balancer_addresses: []
collector_addresses: []
region: eu
`, false, "field region not found"},
		{"a fragment that is not YAML", fragmentFile, "routes: [", true,
			"fragment projects: not YAML"},
		{"a fragment with an unknown field", fragmentFile, `
routes:
  - {paths: [/api/v1/projects], methods: [GET], auth: anonymous, cache: true}
`, true, "fragment projects: breaks the fragment schema: at /routes/0: additional properties 'cache' not allowed"},
		{"a fragment with an unknown value", fragmentFile, `
routes:
  - {paths: [/api/v1/projects], methods: [GET], auth: anonymous, traffic: bulk}
`, true, "fragment projects: breaks the fragment schema: at /routes/0/traffic: value must be one of"},
		{"a fragment with a path that is not a path", fragmentFile, `
routes:
  - {paths: [api/v1/projects], methods: [GET], auth: anonymous}
`, true, "fragment projects: breaks the fragment schema: at /routes/0/paths/0: "},
		{"a fragment with a route without methods", fragmentFile, `
routes:
  - {paths: [/api/v1/projects], auth: anonymous}
`, true, "fragment projects: breaks the fragment schema: at /routes/0: missing property 'methods'"},
		{"a fragment without routes", fragmentFile, "routes: []\n", true,
			"fragment projects: breaks the fragment schema: at /routes: minItems"},

		// The content that section 03 forbids a fragment, by the fields that would carry it.
		{"a fragment that names an upstream address", fragmentFile, `
routes:
  - {paths: [/api/v1/projects], methods: [GET], auth: anonymous, upstream: {nodes: {"10.9.9.9:443": 1}}}
`, true, "fragment projects: breaks the fragment schema: at /routes/0/upstream: a fragment cannot name an upstream address"},
		{"a fragment that names an upstream's nodes", fragmentFile, `
routes:
  - {paths: [/api/v1/projects], methods: [GET], auth: anonymous, nodes: [10.9.9.9]}
`, true, "fragment projects: breaks the fragment schema: at /routes/0/nodes: a fragment cannot name an upstream address"},
		{"a fragment that adds a plugin", fragmentFile, `
routes:
  - {paths: [/api/v1/projects], methods: [GET], auth: anonymous, plugins: {cors: {}}}
`, true, "fragment projects: breaks the fragment schema: at /routes/0/plugins: a fragment cannot add or configure a plugin"},
		{"a fragment with Lua", fragmentFile, `
routes:
  - {paths: [/api/v1/projects], methods: [GET], auth: anonymous, filter_func: "function(vars) return true end"}
`, true, "fragment projects: breaks the fragment schema: at /routes/0/filter_func: a fragment cannot contain Lua or a serverless function"},
		{"a fragment with a serverless function", fragmentFile, `
routes:
  - {paths: [/api/v1/projects], methods: [GET], auth: anonymous, functions: ["return function() end"]}
`, true, "fragment projects: breaks the fragment schema: at /routes/0/functions: a fragment cannot contain Lua or a serverless function"},
		{"a fragment that rewrites a path", fragmentFile, `
routes:
  - {paths: [/api/v1/projects], methods: [GET], auth: anonymous, regex_uri: ["^/api/(.*)", "/$1"]}
`, true, "fragment projects: breaks the fragment schema: at /routes/0/regex_uri: a fragment cannot rewrite a path"},
		{"a fragment with a raw timeout", fragmentFile, `
routes:
  - {paths: [/api/v1/projects], methods: [GET], auth: anonymous, timeout: {read: 600}}
`, true, "fragment projects: breaks the fragment schema: at /routes/0/timeout: a fragment cannot set a raw timeout"},
		{"a fragment with a raw size", fragmentFile, `
routes:
  - {paths: [/api/v1/projects], methods: [GET], auth: anonymous, max_body_size: 1073741824}
`, true, "fragment projects: breaks the fragment schema: at /routes/0/max_body_size: a fragment cannot set a raw size"},

		{"a contract that is not YAML", "contracts/projects/openapi.yaml", "openapi: [", false,
			filepath.Join("contracts", "projects", "openapi.yaml") + ": yaml:"},
		{"a JSON contract that is not JSON", "contracts/projects/members.json", `{"openapi": "3.1.0",`, false,
			filepath.Join("contracts", "projects", "members.json") + ": "},
		{"a certificate and a key that do not belong together", "secrets/application.crt", string(otherCertificate), false,
			"application.crt and application.key: "},
		{"a trust store without a certificate", "secrets/ca.crt", "not a certificate\n", false,
			"ca.crt holds no certificate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := newInputs(t)
			in.Write(t, c.file, c.content)

			_, err := read(in)

			var refusal *application.Refusal
			if err == nil || errors.As(err, &refusal) != c.refusal || !strings.Contains(err.Error(), c.message) {
				t.Errorf("got %v (refusal: %v), want %q (refusal: %v)", err, errors.As(err, &refusal), c.message, c.refusal)
			}
		})
	}
}

func TestAMissingContractsDirectoryIsAnErrorOfTheInputs(t *testing.T) {
	in := newInputs(t)
	if err := os.RemoveAll(filepath.Join(in.Dir, "contracts")); err != nil {
		t.Fatal(err)
	}

	_, err := read(in)

	var refusal *application.Refusal
	if err == nil || errors.As(err, &refusal) || !strings.Contains(err.Error(), "contracts") {
		t.Errorf("got %v, want an error that names the contracts directory", err)
	}
}

func TestAMissingSecretFileIsAnErrorOfTheInputs(t *testing.T) {
	in := newInputs(t)
	if err := os.Remove(filepath.Join(in.Dir, "secrets", infrastructure.ApplicationKeyFile)); err != nil {
		t.Fatal(err)
	}

	_, err := read(in)

	var refusal *application.Refusal
	if err == nil || errors.As(err, &refusal) || !strings.Contains(err.Error(), infrastructure.ApplicationKeyFile) {
		t.Errorf("got %v, want an error that names %s", err, infrastructure.ApplicationKeyFile)
	}
}
