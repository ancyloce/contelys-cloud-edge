package infrastructure_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contelys/contelys-cloud-edge/internal/application"
	"github.com/contelys/contelys-cloud-edge/internal/infrastructure"
)

func TestTheFilesReplaceTheCurrentOnesAndOnlyTheirOwnerReadsApisixYaml(t *testing.T) {
	out := t.TempDir()
	write := func(name string) {
		t.Helper()
		err := infrastructure.Files{}.Write(out, application.Output{
			Instances: []application.InstanceOutput{{Address: "10.0.0.5", File: application.ApisixFile{
				Routes: []application.Route{{ID: name, Name: name}},
			}}},
			Config: application.ConfigFile{Plugins: []string{name}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	write("first")
	write("second")

	apisix := filepath.Join(out, "10.0.0.5", "apisix.yaml")
	content, err := os.ReadFile(apisix)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(content, []byte("id: second")) || bytes.Contains(content, []byte("first")) {
		t.Errorf("apisix.yaml was not replaced: %s", content)
	}
	if !bytes.HasSuffix(content, []byte("\n#END\n")) {
		t.Errorf("apisix.yaml does not end with the #END marker: %q", content)
	}
	for path, want := range map[string]os.FileMode{apisix: 0o600, filepath.Join(out, "config.yaml"): 0o644} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s: mode %v, want %v", filepath.Base(path), info.Mode().Perm(), want)
		}
	}
	for _, dir := range []string{out, filepath.Dir(apisix)} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !entry.IsDir() && entry.Name() != "apisix.yaml" && entry.Name() != "config.yaml" {
				t.Errorf("a temporary file is left behind: %s", filepath.Join(dir, entry.Name()))
			}
		}
	}
}

// APISIX reads ${{NAME}} as a reference to an environment variable, in apisix.yaml and in
// config.yaml alike, and refuses the whole file when the variable is not set.
func TestARenderedFileWithAReferenceToAnEnvironmentVariableWritesNothing(t *testing.T) {
	valid := application.ApisixFile{Routes: []application.Route{{ID: "external.projects.cec1c806d8c4", Host: "app.example.test"}}}
	referring := application.ApisixFile{Routes: []application.Route{{ID: "external.projects.cec1c806d8c4", Host: "app${{HOST}}.example.test"}}}
	cases := []struct {
		name    string
		out     application.Output
		message string
	}{
		{"in the apisix.yaml of the second instance", application.Output{Instances: []application.InstanceOutput{
			{Address: "10.0.0.5", File: valid}, {Address: "10.0.0.6", File: referring},
		}}, "the apisix.yaml of 10.0.0.6 contains ${{, which APISIX reads as a reference to an environment variable"},
		{"in config.yaml", application.Output{
			Instances: []application.InstanceOutput{{Address: "10.0.0.5", File: valid}},
			Config:    application.ConfigFile{Plugins: []string{"${{PLUGIN}}"}},
		}, "config.yaml contains ${{, which APISIX reads as a reference to an environment variable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out")

			err := infrastructure.Files{}.Write(out, c.out)

			if err == nil || !strings.Contains(err.Error(), c.message) {
				t.Errorf("got %v, want %q", err, c.message)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Errorf("a refused file was written into %s", out)
			}
		})
	}
}
