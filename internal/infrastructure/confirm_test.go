package infrastructure_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ancyloce/contelys-cloud-edge/internal/application"
	"github.com/ancyloce/contelys-cloud-edge/internal/infrastructure"
)

// The rules of reading the distributed apisix.yaml, each with a failing fixture. The command's own
// tests read a file that edge render wrote.
func TestEachRuleOfReadingTheDistributedFileRefusesItsFixture(t *testing.T) {
	cases := []struct {
		name, content, message string
	}{
		{"a file that is not YAML", "routes: [", "apisix.yaml: yaml:"},
		{"a file without the fixed-response route", "routes:\n  - {id: external.projects.cec1c806d8c4}\n#END\n",
			"apisix.yaml holds no fixed-response route"},
		{"a fixed-response route without its plugin", "routes:\n  - {id: " + application.RevisionRouteID + "}\n#END\n",
			"the fixed-response route " + application.RevisionRouteID + " carries no answer: it has no plugin mocking"},
		{"an answer that is not JSON", "routes:\n  - id: " + application.RevisionRouteID + "\n    plugins: {mocking: {response_example: 'not json'}}\n#END\n",
			"the fixed-response route " + application.RevisionRouteID + " carries no answer: invalid character"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := t.TempDir()
			if err := os.MkdirAll(filepath.Join(out, "10.0.0.5"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(out, "10.0.0.5", "apisix.yaml"), []byte(c.content), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := infrastructure.Files{}.ReadDistributed(application.ConfirmRequest{Out: out, Instance: "10.0.0.5"})

			if err == nil || !strings.Contains(err.Error(), c.message) {
				t.Errorf("got %v, want %q", err, c.message)
			}
		})
	}
}

func TestAMissingDistributedFileIsAnError(t *testing.T) {
	_, err := infrastructure.Files{}.ReadDistributed(application.ConfirmRequest{Out: t.TempDir(), Instance: "10.0.0.5"})

	if err == nil || !os.IsNotExist(err) {
		t.Errorf("got %v, want an error that the file does not exist", err)
	}
}

// The confirmation compares an instance with the file that was distributed to it, so it recomputes
// the file's digest: a file changed after rendering, even where no id changes, is not the rendered one.
func TestADistributedFileIsReadAsTheSinkWroteItAndAChangeAfterRenderingIsAnError(t *testing.T) {
	out := t.TempDir()
	err := infrastructure.Files{}.Write(out, application.Output{Instances: []application.InstanceOutput{{
		Address: "10.0.0.5", File: application.ApisixFile{
			Routes:    []application.Route{{ID: "external.projects.cec1c806d8c4", UpstreamID: "projects"}, answerRoute},
			Upstreams: []application.Upstream{{ID: "projects", Nodes: []application.Node{{Host: "10.0.1.1", Port: 8443, Weight: 1}}}},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	req := application.ConfirmRequest{Out: out, Instance: "10.0.0.5"}

	distributed, err := infrastructure.Files{}.ReadDistributed(req)

	if err != nil || len(distributed.Answer.Digest) != len(application.DigestPlaceholder) || distributed.Answer.Digest == application.DigestPlaceholder ||
		strings.Join(distributed.Routes, " ") != "external.projects.cec1c806d8c4 "+application.RevisionRouteID || strings.Join(distributed.Upstreams, " ") != "projects" {
		t.Fatalf("got %+v, %v", distributed, err)
	}

	path := filepath.Join(out, "10.0.0.5", "apisix.yaml")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(content), "port: 8443", "port: 9443", 1)), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = infrastructure.Files{}.ReadDistributed(req)

	const want = "the file changed after rendering"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("got %v, want %q", err, want)
	}
}
