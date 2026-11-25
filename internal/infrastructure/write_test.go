package infrastructure_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contelys/contelys-cloud-edge/internal/application"
	"github.com/contelys/contelys-cloud-edge/internal/infrastructure"
)

// answerRoute carries the placeholder of the file's digest, as the fixed-response route of every
// rendered apisix.yaml does, so that the sink can put the digest in its place.
var answerRoute = application.Route{ID: "internal.edge.937ee30909fc", Plugins: map[string]any{
	"mocking": application.Mocking{ResponseExample: `{"digest":"` + application.DigestPlaceholder + `"}`},
}}

func TestTheFilesReplaceTheCurrentOnesAndOnlyTheirOwnerReadsApisixYaml(t *testing.T) {
	out := t.TempDir()
	write := func(name string) {
		t.Helper()
		err := infrastructure.Files{}.Write(out, application.Output{
			Instances: []application.InstanceOutput{{Address: "10.0.0.5", File: application.ApisixFile{
				Routes: []application.Route{{ID: name, Name: name}, answerRoute},
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

// APISIX checks the modification time of apisix.yaml every second, at a resolution of one second, so
// a file whose time lies in the second that it has already loaded is never applied (section 03). A
// second replacement within that second therefore waits for the next one instead of being lost.
func TestASecondReplacementWithinOneSecondWaitsForTheNextSecond(t *testing.T) {
	out := t.TempDir()
	path := filepath.Join(out, "10.0.0.5", "apisix.yaml")
	write := func(id string) {
		t.Helper()
		err := infrastructure.Files{}.Write(out, application.Output{Instances: []application.InstanceOutput{{
			Address: "10.0.0.5", File: application.ApisixFile{Routes: []application.Route{{ID: id}, answerRoute}},
		}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	write("first")
	// The current file was written in this very second.
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}

	write("second")
	// It waited for the next second, rather than stamping a time ahead of the clock.
	if next := now.Truncate(time.Second).Add(time.Second); time.Now().Before(next) {
		t.Errorf("the second replacement returned before %v, the start of the next second", next)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.ModTime(); got.Unix() <= now.Unix() {
		t.Errorf("the second file's modification time %v lies in the second of the first, %v", got, now)
	}
	if content, _ := os.ReadFile(path); !strings.Contains(string(content), "id: second") {
		t.Errorf("the second file was lost: %s", content)
	}
}

// The sink puts the digest of the file in place of its placeholder, which only the fixed-response
// route carries. A file without it, or with it twice, is a defect of Edge's own templates.
func TestAFileThatDoesNotHoldThePlaceholderOfItsDigestOnceWritesNothing(t *testing.T) {
	twice := answerRoute
	twice.ID = "internal.edge.twice"
	for name, routes := range map[string][]application.Route{
		"without it": {{ID: "external.projects.cec1c806d8c4"}},
		"twice":      {answerRoute, twice},
	} {
		t.Run(name, func(t *testing.T) {
			out := t.TempDir()

			err := infrastructure.Files{}.Write(out, application.Output{Instances: []application.InstanceOutput{{
				Address: "10.0.0.5", File: application.ApisixFile{Routes: routes},
			}}})

			const want = "the apisix.yaml of 10.0.0.5 holds the placeholder of its digest"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("got %v, want %q", err, want)
			}
			if entries, _ := os.ReadDir(out); len(entries) > 0 {
				t.Errorf("the sink wrote %v", entries)
			}
		})
	}
}

// APISIX reads ${{NAME}} as a reference to an environment variable, in apisix.yaml and in
// config.yaml alike, and refuses the whole file when the variable is not set.
func TestARenderedFileWithAReferenceToAnEnvironmentVariableWritesNothing(t *testing.T) {
	valid := application.ApisixFile{Routes: []application.Route{{ID: "external.projects.cec1c806d8c4", Host: "app.example.test"}, answerRoute}}
	referring := application.ApisixFile{Routes: []application.Route{{ID: "external.projects.cec1c806d8c4", Host: "app${{HOST}}.example.test"}, answerRoute}}
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

// Every file is written beside its current one before any is renamed into place, so a failure while
// writing one of them replaces none.
func TestAFailureWhileWritingOneFileLeavesEveryCurrentFileInPlace(t *testing.T) {
	out := t.TempDir()
	file := func(id string) application.ApisixFile {
		return application.ApisixFile{Routes: []application.Route{{ID: id}, answerRoute}}
	}
	if err := (infrastructure.Files{}).Write(out, application.Output{
		Instances: []application.InstanceOutput{{Address: "10.0.0.5", File: file("current")}},
	}); err != nil {
		t.Fatal(err)
	}
	// The second instance's directory cannot be made, because a file holds its name.
	if err := os.WriteFile(filepath.Join(out, "10.0.0.6"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	err := infrastructure.Files{}.Write(out, application.Output{Instances: []application.InstanceOutput{
		{Address: "10.0.0.5", File: file("changed")}, {Address: "10.0.0.6", File: file("changed")},
	}})

	if err == nil {
		t.Fatal("the write succeeded")
	}
	content, err := os.ReadFile(filepath.Join(out, "10.0.0.5", "apisix.yaml"))
	if err != nil || !bytes.Contains(content, []byte("id: current")) {
		t.Errorf("the current apisix.yaml was replaced: %s, %v", content, err)
	}
	entries, err := os.ReadDir(filepath.Join(out, "10.0.0.5"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("a temporary file is left behind: %v", entries)
	}
}

// The trial load runs before the files are written, so it refuses such a file first, with the same
// message and before it starts a container.
func TestTheTrialLoadRefusesAReferenceToAnEnvironmentVariableBeforeItStartsAContainer(t *testing.T) {
	_, err := infrastructure.DockerTrial{}.Load(application.Candidate{
		Image:   "contelys/edge",
		Address: "10.0.0.5",
		File:    application.ApisixFile{Routes: []application.Route{{ID: "external.projects.cec1c806d8c4", Host: "app${{HOST}}.example.test"}, answerRoute}},
	})

	const want = "the apisix.yaml of 10.0.0.5 contains ${{, which APISIX reads as a reference to an environment variable"
	if err == nil || err.Error() != want {
		t.Errorf("got %v, want %q", err, want)
	}
}
