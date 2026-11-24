package application

import (
	"strings"
	"testing"
)

// The checks of the rendered apisix.yaml that guard against a defect of Edge's own templates. No
// input can produce such a file, so each fixture breaks one rule of a rendered file directly.

var listeners = Listeners{ExternalPort: 9443, InternalPort: 9444, LoopbackPort: 9080, ControlPort: 9090}

// renderedFile passes every check. Each fixture breaks one rule of it.
func renderedFile() ApisixFile {
	route := func(id, name, path string) Route {
		return Route{ID: id, Name: name, URIs: []string{path}, Methods: []string{"GET"},
			Vars: onListener(listeners.ExternalPort), UpstreamID: "projects"}
	}
	return ApisixFile{
		Routes: []Route{
			route("external.projects.cec1c806d8c4", "projects GET /api/v1/projects", "/api/v1/projects"),
			route("external.projects.0d8dcad58eaf", "projects GET /api/v1/projects/*", "/api/v1/projects/*"),
		},
		Upstreams: []Upstream{{ID: "projects", Name: "projects"}, {ID: "search", Name: "search"}},
		SSLs:      []SSL{{ID: "application-hostname"}},
	}
}

func TestARenderedFileThatKeepsTheRulesPasses(t *testing.T) {
	if err := checkRendered(renderedFile(), listeners); err != nil {
		t.Fatal(err)
	}
}

func TestEachRuleOfTheRenderedFileRefusesItsFixture(t *testing.T) {
	cases := []struct {
		name    string
		break_  func(*ApisixFile)
		message string
	}{
		{"a route without an explicit id", func(f *ApisixFile) {
			f.Routes[1].ID = ""
		}, "the rendered apisix.yaml: route 2, to projects, has no explicit id"},
		{"an upstream without an explicit id", func(f *ApisixFile) {
			f.Upstreams[1].ID = ""
		}, "the rendered apisix.yaml: upstream 2 has no explicit id"},
		{"a route without a name", func(f *ApisixFile) {
			f.Routes[0].Name = ""
		}, "the rendered apisix.yaml: route external.projects.cec1c806d8c4, to projects, has no name"},
		{"a route without a listener condition", func(f *ApisixFile) {
			f.Routes[0].Vars = nil
		}, "the rendered apisix.yaml: route external.projects.cec1c806d8c4, to projects, has no condition that binds it to one of Edge's listeners, so it would answer on every listener"},
		{"a route whose condition names a port that is no listener", func(f *ApisixFile) {
			f.Routes[0].Vars = onListener(8443)
		}, "route external.projects.cec1c806d8c4, to projects, has no condition that binds it to one of Edge's listeners"},
		{"a route whose condition is not an equality", func(f *ApisixFile) {
			f.Routes[0].Vars = [][]any{{"server_port", "~=", listeners.ExternalPort}}
		}, "route external.projects.cec1c806d8c4, to projects, has no condition that binds it to one of Edge's listeners"},
		{"a route on the control listener", func(f *ApisixFile) {
			f.Routes[0].Vars = onListener(listeners.ControlPort)
		}, "route external.projects.cec1c806d8c4, to projects, has no condition that binds it to one of Edge's listeners"},
		{"two routes with one id", func(f *ApisixFile) {
			f.Routes[1].ID = f.Routes[0].ID
		}, "the rendered apisix.yaml: routes 1 and 2 have the id external.projects.cec1c806d8c4, and APISIX would let the later replace the earlier"},
		{"two upstreams with one id", func(f *ApisixFile) {
			f.Upstreams[1].ID = "projects"
		}, "the rendered apisix.yaml: upstreams 1 and 2 have the id projects"},
		{"two certificates with one id", func(f *ApisixFile) {
			f.SSLs = append(f.SSLs, SSL{ID: "application-hostname"})
		}, "the rendered apisix.yaml: ssls 1 and 2 have the id application-hostname"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			file := renderedFile()
			c.break_(&file)

			err := checkRendered(file, listeners)

			if err == nil || !strings.Contains(err.Error(), c.message) {
				t.Errorf("got %v, want %q", err, c.message)
			}
		})
	}
}
