//go:build scenario

package scenario

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/client"
	"go.yaml.in/yaml/v3"

	"github.com/contelys/contelys-cloud-edge/internal/application"
	"github.com/contelys/contelys-cloud-edge/internal/infrastructure"
)

// These scenarios show that a fragment that breaks a rule never reaches traffic (ticket 07 of
// Edge's plan): a rendering that a rule or the trial load refuses writes no file, so a running Edge
// keeps serving its current one.

// manyPaths lists so many paths that the route's name, which carries them all, exceeds the 256
// characters that APISIX allows a name. No rule of the renderer refuses it, and APISIX drops the
// route, so only the trial load finds it.
var manyPaths = func() string {
	var paths []string
	for i := range 12 {
		paths = append(paths, fmt.Sprintf("/api/v1/archive/collections-of-section-%02d", i+1))
	}
	return strings.Join(paths, ", ")
}()

func TestARefusedRenderingWritesNoFileAndEdgeKeepsServingItsCurrentRoutes(t *testing.T) {
	cases := []struct {
		name, fragment, message string
	}{
		{"a fragment that a rule refuses", archiveFragment + `
  - {paths: [/internal/v1/archive], methods: [GET], auth: anonymous}
`, "edge render: refused: fragment projects: route 3: /internal/v1/archive lies under /internal, which no fragment may claim"},
		{"a route that APISIX drops in the trial load", archiveFragment + `
  - {paths: [` + manyPaths + `], methods: [GET], auth: anonymous}
`, "edge render: refused: fragment projects: the trial load: APISIX did not load route external.projects."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			current := projectsOnly(t)
			out := current.render(t)
			edge := startEdge(t, current.ca, out)
			before, beforeInfo := readWithInfo(t, routesFile(out))
			config, _ := readWithInfo(t, filepath.Join(out, "config.yaml"))

			refused := current
			refused.fragments = map[application.Slug]string{"projects": c.fragment}
			output, err := refused.renderInto(t, out)

			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(output, c.message) {
				t.Fatalf("edge render: %v: %s; want exit code 1 and %q", err, output, c.message)
			}
			after, afterInfo := readWithInfo(t, routesFile(out))
			afterConfig, _ := readWithInfo(t, filepath.Join(out, "config.yaml"))
			if !bytes.Equal(after, before) || !afterInfo.ModTime().Equal(beforeInfo.ModTime()) || !bytes.Equal(afterConfig, config) {
				t.Error("the refused rendering replaced a file")
			}
			// The archive route, which the refused fragment also adds, never answers, and the current
			// routes keep answering.
			edge.expectNotApplied(t, 2*pollingInterval)
		})
	}
}

func TestTheTrialLoadStartsWithTheCandidateConfigYamlAndRemovesItsContainer(t *testing.T) {
	// Not parallel, so that no other scenario's trial load runs while this one counts containers.
	current := projectsOnly(t)
	out := current.render(t)
	candidate := application.Candidate{Image: image, TrustStore: string(current.ca.PEM)}
	decodeRendered(t, routesFile(out), &candidate.File)
	decodeFile(t, filepath.Join(out, "config.yaml"), &candidate.Config)
	route := candidate.File.Routes[0].ID
	if _, uses := candidate.File.Routes[0].Plugins["response-rewrite"]; !uses {
		t.Fatalf("route %s does not use response-rewrite", route)
	}

	loaded, err := infrastructure.DockerTrial{}.Load(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(loaded.Routes, route) || !slices.Equal(loaded.Upstreams, []string{"projects"}) {
		t.Errorf("with the installation's config.yaml: loaded %+v", loaded)
	}

	// The image's default config.yaml enables response-rewrite, among APISIX's default plugins. A
	// candidate config.yaml that does not enable it makes the instance drop the route.
	candidate.Config.Plugins = slices.DeleteFunc(slices.Clone(candidate.Config.Plugins), func(p string) bool { return p == "response-rewrite" })
	loaded, err = infrastructure.DockerTrial{}.Load(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(loaded.Routes, route) || !slices.Equal(loaded.Upstreams, []string{"projects"}) {
		t.Errorf("with a config.yaml that disables response-rewrite: loaded %+v, want route %s dropped", loaded, route)
	}

	docker, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	left, err := docker.ContainerList(context.Background(), client.ContainerListOptions{
		All: true, Filters: client.Filters{}.Add("label", infrastructure.TrialLabel),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(left.Items) > 0 {
		t.Errorf("the trial load left %d containers behind", len(left.Items))
	}
}

func readWithInfo(t *testing.T, path string) ([]byte, os.FileInfo) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return content, info
}

// decodeRendered decodes a rendered apisix.yaml as it reads with the placeholder in place of its
// digest, which is how section 03 defines the digest, so that the sink and the trial load can put
// the digest in again.
func decodeRendered(t *testing.T, path string, v any) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := regexp.MustCompile(application.DigestPrefix + "[0-9a-f]{64}")
	if n := len(digest.FindAll(content, -1)); n != 1 {
		t.Fatalf("%s holds %d digests, want 1", path, n)
	}
	if err := yaml.Unmarshal(digest.ReplaceAll(content, []byte(application.DigestPlaceholder)), v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func decodeFile(t *testing.T, path string, v any) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(content, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
