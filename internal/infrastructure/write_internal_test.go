package infrastructure

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contelys/contelys-cloud-edge/internal/application"
)

// answerRoute carries the placeholder of the file's digest, as the fixed-response route of every
// rendered apisix.yaml does, so that the sink can put the digest in its place.
var answerRoute = application.Route{ID: "internal.edge.937ee30909fc", Plugins: map[string]any{
	"mocking": application.Mocking{ResponseExample: `{"digest":"` + application.DigestPlaceholder + `"}`},
}}

// A rename that fails after another succeeded puts back the file already replaced, or removes it when
// there was none, so that every instance keeps its current file and no other file is left behind.
// The second of the three renames fails: the first instance's apisix.yaml is already in place, and the
// second instance's and config.yaml are not.
func TestAFailedRenamePutsBackTheFilesAlreadyReplaced(t *testing.T) {
	output := func(id string) application.Output {
		file := application.ApisixFile{Routes: []application.Route{{ID: id}, answerRoute}}
		return application.Output{Instances: []application.InstanceOutput{{Address: "10.0.0.5", File: file}, {Address: "10.0.0.6", File: file}}}
	}
	for name, current := range map[string]bool{"over current files": true, "without current files": false} {
		t.Run(name, func(t *testing.T) {
			out := t.TempDir()
			if current {
				if err := (Files{}).Write(out, output("current")); err != nil {
					t.Fatal(err)
				}
			}
			before := everyFile(t, out)

			renames := 0
			err := write(out, output("changed"), func(from, to string) error {
				if renames++; renames == 2 {
					return errors.New("the rename is refused")
				}
				return os.Rename(from, to)
			})

			if err == nil || !strings.Contains(err.Error(), "the rename is refused") {
				t.Fatalf("got %v, want the failed rename", err)
			}
			if after := everyFile(t, out); !maps.Equal(after, before) {
				t.Errorf("the files changed:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

// everyFile maps each file under dir, hidden ones too, to its content.
func everyFile(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		files[strings.TrimPrefix(path, dir)] = string(content)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// The encoder always ends apisix.yaml with the #END marker, so no input can produce a file without
// it. The fixtures are such files. APISIX looks for the marker, followed by nothing but white space,
// in the last 10 bytes of the file.
func TestARouteFileWithoutTheEndMarkerIsRefused(t *testing.T) {
	const want = "the apisix.yaml of 10.0.0.5 does not end with the #END marker, without which APISIX never applies it"
	for _, content := range []string{
		"routes: []\n",
		"routes: []\n#END\nupstreams: []\n",
		"routes: []\n#END\n\n\n\n\n\n\n\n",
	} {
		if err := checkRouteFile("the apisix.yaml of 10.0.0.5", []byte(content)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", content, err, want)
		}
	}
	for _, content := range []string{"routes: []\n#END\n", "routes: []\n#END"} {
		if err := checkRouteFile("the apisix.yaml of 10.0.0.5", []byte(content)); err != nil {
			t.Errorf("%q: %v", content, err)
		}
	}
}
