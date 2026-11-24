package infrastructure_test

import (
	"bytes"
	"os"
	"path/filepath"
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
