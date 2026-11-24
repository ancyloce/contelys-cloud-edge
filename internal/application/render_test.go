package application_test

import (
	"errors"
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
			{Paths: []string{"/api/v1/projects"}, Methods: []string{"GET"}, Auth: application.AuthAnonymous},
		}}},
	}
}

func render(in application.Inputs) (error, bool) {
	written := &sink{}
	err := application.Renderer{Source: source{in}, Sink: written}.Render(application.Request{})
	return err, written.written
}

func TestValidInputsAreRenderedAndWritten(t *testing.T) {
	if err, written := render(validInputs()); err != nil || !written {
		t.Fatalf("got %v, written %v; want the files written", err, written)
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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := validInputs()
			c.break_(&in)

			err, written := render(in)

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
