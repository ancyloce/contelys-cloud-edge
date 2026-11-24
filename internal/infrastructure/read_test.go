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
// schema and the secret files.

func newInputs(t *testing.T) *fixture.Inputs {
	t.Helper()
	in := fixture.NewInputs(t, fixture.NewAuthority(t), "10.0.0.5")
	in.Fragment(t, "projects", `
routes:
  - {paths: [/api/v1/projects, /api/v1/projects/*], methods: [GET], auth: anonymous, traffic: transfer}
`)
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

	inputs, err := read(in)

	if err != nil {
		t.Fatal(err)
	}
	want := []application.Fragment{{Slug: "projects", Routes: []application.FragmentRoute{{
		Paths: []string{"/api/v1/projects", "/api/v1/projects/*"}, Methods: []string{"GET"},
		Auth: application.AuthAnonymous, Traffic: application.TrafficTransfer,
	}}}}
	if !reflect.DeepEqual(inputs.Fragments, want) {
		t.Errorf("fragments: got %+v, want %+v", inputs.Fragments, want)
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
}

func TestEachReadingRuleRefusesItsFixture(t *testing.T) {
	ca := fixture.NewAuthority(t)
	otherCertificate, _, _ := ca.Issue(t, fixture.ApplicationHostname)
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
		{"a fragment that is not YAML", "fragments/projects/route-fragment.yaml", "routes: [", true,
			"fragment projects: not YAML"},
		{"a fragment with an unknown field", "fragments/projects/route-fragment.yaml", `
routes:
  - {paths: [/api/v1/projects], methods: [GET], auth: anonymous, upstream: 10.9.9.9}
`, true, "fragment projects: breaks the fragment schema: at /routes/0: additional properties 'upstream' not allowed"},
		{"a fragment with an unknown value", "fragments/projects/route-fragment.yaml", `
routes:
  - {paths: [/api/v1/projects], methods: [GET], auth: anonymous, traffic: bulk}
`, true, "fragment projects: breaks the fragment schema: at /routes/0/traffic: value must be one of"},
		{"a fragment with a path that is not a path", "fragments/projects/route-fragment.yaml", `
routes:
  - {paths: [api/v1/projects], methods: [GET], auth: anonymous}
`, true, "fragment projects: breaks the fragment schema: at /routes/0/paths/0: "},
		{"a fragment with a route without methods", "fragments/projects/route-fragment.yaml", `
routes:
  - {paths: [/api/v1/projects], auth: anonymous}
`, true, "fragment projects: breaks the fragment schema: at /routes/0: missing property 'methods'"},
		{"a fragment without routes", "fragments/projects/route-fragment.yaml", "routes: []\n", true,
			"fragment projects: breaks the fragment schema: at /routes: minItems"},
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
