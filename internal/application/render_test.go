package application_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/contelys/contelys-cloud-edge/internal/application"
)

// The application's rules, each with a failing fixture. The use case is driven through its ports: a
// source that hands over decoded inputs and a sink that records whether anything was written.

type source struct{ inputs application.Inputs }

func (s source) Read(application.Request) (application.Inputs, error) { return s.inputs, nil }

type sink struct{ written bool }

func (s *sink) Write(string, application.Output) error {
	s.written = true
	return nil
}

// trial is a trial load whose instance loads every rendered item except the ids in drop, or fails
// with err. It records the candidates it was given.
type trial struct {
	drop       []string
	err        error
	candidates []application.Candidate
}

func (t *trial) Load(c application.Candidate) (application.Loaded, error) {
	t.candidates = append(t.candidates, c)
	if t.err != nil {
		return application.Loaded{}, t.err
	}
	var loaded application.Loaded
	for _, route := range c.File.Routes {
		if !slices.Contains(t.drop, route.ID) {
			loaded.Routes = append(loaded.Routes, route.ID)
		}
	}
	for _, upstream := range c.File.Upstreams {
		if !slices.Contains(t.drop, upstream.ID) {
			loaded.Upstreams = append(loaded.Upstreams, upstream.ID)
		}
	}
	return loaded, nil
}

// projectsContract is the OpenAPI contract of projects, as its paths are written there.
var projectsContract = []string{"/api/v1/projects", "/api/v1/projects/{projectId}", "/api/v1/projects/{projectId}/members"}

// validInputs renders. Each fixture breaks one rule of it.
func validInputs() application.Inputs {
	return application.Inputs{
		Defaults: application.Defaults{
			Listeners:               application.Listeners{ExternalPort: 9443, InternalPort: 9444, LoopbackPort: 9080, ControlPort: 9090},
			StrictTransportSecurity: "max-age=31536000",
		},
		Instances: application.Instances{
			"edge":     {{Address: "10.0.0.5", Port: 9443, Weight: 1}},
			"projects": {{Address: "10.0.1.1", Port: 8443, Weight: 1}, {Address: "projects-2.internal.test", Port: 8443, Weight: 0}},
		},
		Installation: application.Installation{
			ApplicationHostname:    "app.example.test",
			AuthenticationHostname: "auth.example.test",
			LoadBalancerAddresses:  []string{"10.0.9.1"},
			CollectorAddresses:     []string{"10.0.8.0/24"},
		},
		Fragments: []application.Fragment{{Slug: "projects", Routes: []application.FragmentRoute{
			anonymous([]string{"/api/v1/projects"}, "GET"),
		}}},
		Contracts: map[application.Slug][]string{"projects": projectsContract},
		Secrets:   application.Secrets{TrustStore: "-----BEGIN CERTIFICATE-----\n"},
	}
}

// anonymous is an anonymous route of a fragment.
func anonymous(paths []string, methods ...string) application.FragmentRoute {
	return application.FragmentRoute{Paths: paths, Methods: methods, Auth: application.AuthAnonymous}
}

// withFragment adds the fragment of another deployable, with an instance and, unless contract is
// nil, its OpenAPI contract. Fragments stay sorted by slug, as the source hands them over.
func withFragment(in *application.Inputs, slug application.Slug, contract []string, routes ...application.FragmentRoute) {
	in.Instances[slug] = []application.Instance{{Address: "10.0.2.1", Port: 8443, Weight: 1}}
	if contract != nil {
		in.Contracts[slug] = contract
	}
	in.Fragments = append(in.Fragments, application.Fragment{Slug: slug, Routes: routes})
	for i := len(in.Fragments) - 1; i > 0 && in.Fragments[i].Slug < in.Fragments[i-1].Slug; i-- {
		in.Fragments[i], in.Fragments[i-1] = in.Fragments[i-1], in.Fragments[i]
	}
}

// The image that the requests of these tests name for the trial load.
const image = "contelys/edge@sha256:0123456789abcdef"

func render(in application.Inputs) (error, bool) {
	return renderWith(in, &trial{})
}

func renderWith(in application.Inputs, loads *trial) (error, bool) {
	written := &sink{}
	err := application.Renderer{Source: source{in}, Trial: loads, Sink: written}.Render(application.Request{Image: image})
	return err, written.written
}

func TestValidInputsAreRenderedAndWritten(t *testing.T) {
	if err, written := render(validInputs()); err != nil || !written {
		t.Fatalf("got %v, written %v; want the files written", err, written)
	}
}

// Design section 03 gives Authoring every path outside /api/ that no other route claims. A route
// that claims the remainder therefore answers neither under /api/ nor under /internal, whatever the
// case of /internal, and such a request finds no route and gets Edge's 404. Every other route keeps
// its listener condition alone.
func TestAuthoringsRemainderLeavesThePathsUnderAPIAndInternalToEdge(t *testing.T) {
	in := validInputs()
	withFragment(&in, application.AuthoringSlug, nil, anonymous([]string{"/*"}, "GET"), anonymous([]string{"/editor/*"}, "GET"))
	loads := &trial{}
	if err, _ := renderWith(in, loads); err != nil {
		t.Fatal(err)
	}

	onExternal := []any{"server_port", "==", 9443}
	want := map[string][][]any{
		"/*":               {onExternal, {"uri", "!", "~~", "^/api(/|$)"}, {"uri", "!", "~*", "^/internal(/|$)"}},
		"/editor/*":        {onExternal},
		"/api/v1/projects": {onExternal},
	}
	for _, route := range loads.candidates[0].File.Routes {
		if !strings.HasPrefix(route.ID, "external.") {
			continue
		}
		path := route.URIs[0]
		if !slices.EqualFunc(route.Vars, want[path], slices.Equal) {
			t.Errorf("route %s: conditions %v, want %v", path, route.Vars, want[path])
		}
		delete(want, path)
	}
	if len(want) != 0 {
		t.Errorf("not rendered on the external listener: %v", want)
	}
}

func TestTheTrialLoadStartsTheImageWithTheCandidateFilesOnceForEachDistinctFile(t *testing.T) {
	in := validInputs()
	in.Instances["edge"] = append(in.Instances["edge"], application.Instance{Address: "10.0.0.6", Port: 9443, Weight: 1})
	loads := &trial{}

	if err, written := renderWith(in, loads); err != nil || !written {
		t.Fatalf("got %v, written %v; want the files written", err, written)
	}

	// Both Edge instances get the same file, so one trial load covers them.
	if len(loads.candidates) != 1 {
		t.Fatalf("got %d trial loads, want 1", len(loads.candidates))
	}
	candidate := loads.candidates[0]
	if candidate.Image != image || candidate.TrustStore != in.Secrets.TrustStore {
		t.Errorf("image %q, trust store %q", candidate.Image, candidate.TrustStore)
	}
	if routes := candidate.File.Routes; len(routes) != 1 || routes[0].ID != "external.projects.cec1c806d8c4" {
		t.Errorf("routes of the candidate apisix.yaml: %+v", routes)
	}
	// The candidate config.yaml is the installation's, with its listeners and its list of plugins.
	if candidate.Config.Apisix.Control.Port != 9090 || candidate.Config.Apisix.ProxyProtocol == nil ||
		!slices.Contains(candidate.Config.Plugins, "response-rewrite") || slices.Contains(candidate.Config.Plugins, "cors") {
		t.Errorf("candidate config.yaml: %+v", candidate.Config)
	}
}

func TestWhatTheTrialInstanceDoesNotLoadRefusesTheRenderingAndNothingIsWritten(t *testing.T) {
	cases := []struct {
		name    string
		loads   *trial
		refusal bool
		message string
	}{
		{"a route of a fragment", &trial{drop: []string{"external.projects.cec1c806d8c4"}}, true,
			"fragment projects: the trial load: APISIX did not load route external.projects.cec1c806d8c4 (projects GET /api/v1/projects), so it would never reach traffic"},
		{"an upstream", &trial{drop: []string{"projects"}}, false,
			"the trial load: APISIX did not load upstream projects, whose instances come from the instances file"},
		{"a trial load that cannot run", &trial{err: errors.New("the trial load: cannot reach Docker")}, false,
			"the trial load: cannot reach Docker"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err, written := renderWith(validInputs(), c.loads)

			var refusal *application.Refusal
			if err == nil || errors.As(err, &refusal) != c.refusal || !strings.Contains(err.Error(), c.message) {
				t.Errorf("got %v (refusal: %v), want %q (refusal: %v)", err, errors.As(err, &refusal), c.message, c.refusal)
			}
			if written {
				t.Error("a refused rendering was written")
			}
		})
	}
}

func TestTheRulesAdmitWhatSection03Allows(t *testing.T) {
	cases := []struct {
		name  string
		allow func(*application.Inputs)
	}{
		{"an exact path that a path template of the contract holds", func(in *application.Inputs) {
			in.Fragments[0].Routes[0].Paths = []string{"/api/v1/projects/archived"}
		}},
		{"a prefix that covers paths of the contract", func(in *application.Inputs) {
			in.Fragments[0].Routes[0].Paths = []string{"/api/v1/projects", "/api/v1/projects/*"}
		}},
		{"two routes on one path with other methods", func(in *application.Inputs) {
			in.Fragments[0].Routes = append(in.Fragments[0].Routes, anonymous([]string{"/api/v1/projects"}, "POST", "DELETE"))
		}},
		{"Authoring's remainder beside the paths of other deployables, without a contract", func(in *application.Inputs) {
			withFragment(in, "authoring", nil, anonymous([]string{"/*", "/editor/*"}, "GET", "HEAD"))
		}},
		{"a path of one deployable beside a prefix of another that does not cover it", func(in *application.Inputs) {
			withFragment(in, "search", []string{"/api/v1/projects-search", "/api/v1/search/{query}"},
				anonymous([]string{"/api/v1/projects-search", "/api/v1/search/*"}, "GET"))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := validInputs()
			c.allow(&in)

			if err, written := render(in); err != nil || !written {
				t.Errorf("got %v, written %v; want the files written", err, written)
			}
		})
	}
}

func TestEachRuleRefusesItsFixtureAndNothingIsWritten(t *testing.T) {
	cases := []struct {
		name    string
		break_  func(*application.Inputs)
		refusal bool // a refusal names a fragment; any other failure is an error of the inputs
		message string
	}{
		{"a slug in the instances file that is not a slug", func(in *application.Inputs) {
			in.Instances["Projects"] = in.Instances["projects"]
		}, false, `the instances file: "Projects" is not a slug`},
		{"an instance without an address", func(in *application.Inputs) {
			in.Instances["projects"][0].Address = ""
		}, false, "the instances file: projects, instance 1: the address is empty"},
		{"a port below 1", func(in *application.Inputs) {
			in.Instances["projects"][1].Port = 0
		}, false, "the instances file: projects, instance 2: 0 is not a port"},
		{"a port above 65535", func(in *application.Inputs) {
			in.Instances["projects"][0].Port = 65536
		}, false, "the instances file: projects, instance 1: 65536 is not a port"},
		{"a weight other than 0 and 1", func(in *application.Inputs) {
			in.Instances["projects"][0].Weight = 2
		}, false, "the instances file: projects, instance 1: the weight is 1 to serve or 0 to drain, not 2"},
		{"no Edge instance", func(in *application.Inputs) {
			delete(in.Instances, "edge")
		}, false, "the instances file lists no instance of edge"},
		{"no application hostname", func(in *application.Inputs) {
			in.Installation.ApplicationHostname = ""
		}, false, "the installation file: the application hostname is empty"},
		{"no authentication hostname", func(in *application.Inputs) {
			in.Installation.AuthenticationHostname = ""
		}, false, "the installation file: the authentication hostname is empty"},
		{"a load-balancer address that is not an address", func(in *application.Inputs) {
			in.Installation.LoadBalancerAddresses = []string{"balancer.internal"}
		}, false, `the installation file: "balancer.internal" is neither an address nor a prefix`},
		{"a collector address that is not an address", func(in *application.Inputs) {
			in.Installation.CollectorAddresses = []string{"10.0.8.0/99"}
		}, false, `the installation file: "10.0.8.0/99" is neither an address nor a prefix`},
		{"a fragment whose directory name is not a slug", func(in *application.Inputs) {
			in.Fragments[0].Slug = "Projects_2"
		}, true, "fragment Projects_2: the directory name is not a slug"},
		{"a fragment whose deployable has no instances", func(in *application.Inputs) {
			delete(in.Instances, "projects")
		}, true, "fragment projects: the instances file lists no instance of this deployable"},
		{"an authenticated route, which needs forward-auth", func(in *application.Inputs) {
			in.Fragments[0].Routes[0].Auth = application.AuthRequired
		}, true, `fragment projects: route 1: auth "required" is not rendered yet`},
		{"a route without auth, which defaults to required", func(in *application.Inputs) {
			in.Fragments[0].Routes[0].Auth = ""
		}, true, `fragment projects: route 1: auth "required" is not rendered yet`},
		{"a WebSocket route", func(in *application.Inputs) {
			in.Fragments[0].Routes[0].Traffic = application.TrafficWebSocket
		}, true, `fragment projects: route 1: traffic "websocket" is not rendered yet`},

		// The rules of section 03 for the paths of a fragment.
		{"a fragment of Keycloak, whose routes are Edge's own", func(in *application.Inputs) {
			withFragment(in, "keycloak", []string{"/realms/contelys"}, anonymous([]string{"/realms/contelys"}, "GET"))
		}, true, "fragment keycloak: Edge's own template holds the routes to keycloak, so it ships no fragment"},
		{"a fragment of Edge itself", func(in *application.Inputs) {
			in.Contracts["edge"] = []string{"/api/v1/edge"}
			in.Fragments = append([]application.Fragment{{Slug: "edge", Routes: []application.FragmentRoute{
				anonymous([]string{"/api/v1/edge"}, "GET"),
			}}}, in.Fragments...)
		}, true, "fragment edge: Edge's own template holds the routes to edge, so it ships no fragment"},
		{"a path under /internal", func(in *application.Inputs) {
			in.Fragments[0].Routes[0].Paths = []string{"/api/v1/projects", "/internal/v1/projects"}
		}, true, "fragment projects: route 1: /internal/v1/projects lies under /internal, which no fragment may claim"},
		{"a prefix under /internal", func(in *application.Inputs) {
			in.Fragments[0].Routes[0].Paths = []string{"/internal/*"}
		}, true, "fragment projects: route 1: /internal/* lies under /internal"},
		{"/internal in other letters", func(in *application.Inputs) {
			in.Fragments[0].Routes[0].Paths = []string{"/Internal/v1/projects"}
		}, true, "fragment projects: route 1: /Internal/v1/projects lies under /internal"},
		{"the remainder, claimed by a deployable other than Authoring", func(in *application.Inputs) {
			in.Fragments[0].Routes[0].Paths = []string{"/*"}
		}, true, "fragment projects: route 1: /* claims the remainder of the application hostname, which only Authoring's fragment may claim"},
		{"two routes of one fragment with the same path and the same method", func(in *application.Inputs) {
			in.Fragments[0].Routes = append(in.Fragments[0].Routes, anonymous([]string{"/api/v1/projects/*", "/api/v1/projects"}, "POST", "GET"))
		}, true, "fragment projects: routes 1 and 2 both list GET /api/v1/projects"},
		{"a path that equals a path of another fragment", func(in *application.Inputs) {
			withFragment(in, "search", projectsContract, anonymous([]string{"/api/v1/projects"}, "POST"))
		}, true, "fragment search: route 1: /api/v1/projects is also a path of fragment projects"},
		{"a path that lies under a prefix of another fragment", func(in *application.Inputs) {
			in.Fragments[0].Routes[0].Paths = []string{"/api/v1/projects/*"}
			withFragment(in, "search", []string{"/api/v1/projects/search"}, anonymous([]string{"/api/v1/projects/search"}, "GET"))
		}, true, "fragment search: route 1: /api/v1/projects/search lies under /api/v1/projects/* of fragment projects"},
		{"a prefix that covers a path of another fragment", func(in *application.Inputs) {
			withFragment(in, "billing", []string{"/api/v1/{resource}"}, anonymous([]string{"/api/v1/*"}, "GET"))
		}, true, "fragment projects: route 1: /api/v1/projects lies under /api/v1/* of fragment billing"},
		{"a prefix that covers a path of an earlier fragment", func(in *application.Inputs) {
			withFragment(in, "search", []string{"/api/v1/{resource}"}, anonymous([]string{"/api/v1/*"}, "GET"))
		}, true, "fragment search: route 1: /api/v1/* covers /api/v1/projects of fragment projects"},
		{"a path that the contract does not hold", func(in *application.Inputs) {
			in.Fragments[0].Routes[0].Paths = []string{"/api/v1/projects", "/api/v1/archive"}
		}, true, "fragment projects: route 1: /api/v1/archive is not a path of the deployable's OpenAPI contract"},
		{"a path with one segment more than the contract's", func(in *application.Inputs) {
			in.Fragments[0].Routes[0].Paths = []string{"/api/v1/projects/42/members/7"}
		}, true, "fragment projects: route 1: /api/v1/projects/42/members/7 is not a path of the deployable's OpenAPI contract"},
		{"a prefix that covers no path of the contract", func(in *application.Inputs) {
			in.Fragments[0].Routes[0].Paths = []string{"/api/v1/projects/42/members/*"}
		}, true, "fragment projects: route 1: /api/v1/projects/42/members/* stands for no parameter of a path of the deployable's OpenAPI contract"},
		{"a prefix over a literal segment of the contract's paths", func(in *application.Inputs) {
			in.Fragments[0].Routes[0].Paths = []string{"/api/v1/*"}
		}, true, "fragment projects: route 1: /api/v1/* stands for no parameter of a path of the deployable's OpenAPI contract"},
		{"a path of a domain service outside /api/", func(in *application.Inputs) {
			in.Contracts["projects"] = append(in.Contracts["projects"], "/editor/projects")
			in.Fragments[0].Routes[0].Paths = []string{"/editor/projects"}
		}, true, "fragment projects: route 1: /editor/projects lies outside /api/, where the API lives, and only Authoring's fragment claims paths outside it"},
		{"a path of Authoring under /api/", func(in *application.Inputs) {
			withFragment(in, "authoring", nil, anonymous([]string{"/*", "/api/v1/editor/*"}, "GET"))
		}, true, "fragment authoring: route 1: /api/v1/editor/* lies under /api/, which the API reserves"},
		{"a deployable without a contract", func(in *application.Inputs) {
			delete(in.Contracts, "projects")
		}, true, "fragment projects: the deployable has no OpenAPI contract"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := validInputs()
			c.break_(&in)
			loads := &trial{}

			err, written := renderWith(in, loads)

			var refusal *application.Refusal
			if err == nil || errors.As(err, &refusal) != c.refusal || !strings.Contains(err.Error(), c.message) {
				t.Errorf("got %v (refusal: %v), want %q (refusal: %v)", err, errors.As(err, &refusal), c.message, c.refusal)
			}
			if written || len(loads.candidates) > 0 {
				t.Errorf("a refused rendering was written: %v, or reached the trial load: %v", written, len(loads.candidates) > 0)
			}
		})
	}
}
