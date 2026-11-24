package application

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

// Source reads the renderer's inputs.
type Source interface {
	Read(Request) (Inputs, error)
}

// Sink writes the rendered files into the output directory, each beside the current one and
// renamed into place.
type Sink interface {
	Write(dir string, out Output) error
}

// Renderer is the use case behind the render command.
type Renderer struct {
	Source Source
	Sink   Sink
}

// Render reads the inputs, renders them and writes the files. A Refusal writes nothing.
func (r Renderer) Render(req Request) error {
	in, err := r.Source.Read(req)
	if err != nil {
		return err
	}
	out, err := render(in)
	if err != nil {
		return err
	}
	return r.Sink.Write(req.Out, out)
}

// Output is the rendered configuration: one apisix.yaml for each Edge instance and one config.yaml
// for the installation.
type Output struct {
	Instances []InstanceOutput
	Config    ConfigFile
}

// InstanceOutput is the apisix.yaml of the Edge instance at Address.
type InstanceOutput struct {
	Address string
	File    ApisixFile
}

// render checks and renders the inputs. The same inputs always give the same output.
func render(in Inputs) (Output, error) {
	if err := check(in); err != nil {
		return Output{}, err
	}
	file, err := renderApisix(in)
	if err != nil {
		return Output{}, err
	}
	out := Output{Config: renderConfig(in)}
	for _, edge := range in.Instances[EdgeSlug] {
		out.Instances = append(out.Instances, InstanceOutput{Address: edge.Address, File: file})
	}
	return out, nil
}

func renderApisix(in Inputs) (ApisixFile, error) {
	file := ApisixFile{
		Routes:    []Route{},
		Upstreams: []Upstream{},
		SSLs: []SSL{{
			ID:   "application-hostname",
			SNIs: []string{in.Installation.ApplicationHostname},
			Cert: in.Secrets.ApplicationCertificate,
			Key:  in.Secrets.ApplicationKey,
		}},
	}
	for _, fragment := range in.Fragments {
		instances := in.Instances[fragment.Slug]
		if len(instances) == 0 {
			return ApisixFile{}, &Refusal{Fragment: fragment.Slug, Rule: "the instances file lists no instance of this deployable"}
		}
		for i, route := range fragment.Routes {
			rendered, err := externalRoute(in, fragment.Slug, route)
			if err != nil {
				return ApisixFile{}, &Refusal{Fragment: fragment.Slug, Rule: fmt.Sprintf("route %d: %v", i+1, err)}
			}
			file.Routes = append(file.Routes, rendered)
		}
		file.Upstreams = append(file.Upstreams, upstream(fragment.Slug, instances))
	}
	return file, nil
}

// externalRoute renders one route of a fragment on the external listener and the application
// hostname. Its upstream is always the deployable that ships the fragment. Its name, like its id,
// carries its methods, because metrics are labelled by route name.
func externalRoute(in Inputs, slug Slug, route FragmentRoute) (Route, error) {
	auth := cmp.Or(route.Auth, AuthRequired)
	traffic := cmp.Or(route.Traffic, TrafficAPI)
	// A route that needs authentication or is a WebSocket route is never rendered without the rules
	// that keep it safe: forward-auth arrives with ticket 09 and the WebSocket rules with ticket 12 of
	// the Edge implementation plan. Until tickets 16 and 18 render the limits, timeouts and sizes of
	// the traffic classes, every route gets APISIX's own, whatever its class.
	if auth != AuthAnonymous {
		return Route{}, fmt.Errorf("auth %q is not rendered yet, because Edge has no forward-auth yet", auth)
	}
	if traffic == TrafficWebSocket {
		return Route{}, fmt.Errorf("traffic %q is not rendered yet, because Edge has no WebSocket rules yet", traffic)
	}
	return Route{
		ID:         routeID("external", slug, route.Paths, route.Methods),
		Name:       string(slug) + " " + strings.Join(route.Methods, ",") + " " + strings.Join(route.Paths, " "),
		Host:       in.Installation.ApplicationHostname,
		URIs:       route.Paths,
		Methods:    route.Methods,
		Vars:       onListener(in.Defaults.Listeners.ExternalPort),
		UpstreamID: string(slug),
		Plugins: map[string]any{
			"response-rewrite": ResponseRewrite{Headers: ResponseHeaders{Set: map[string]string{
				"Strict-Transport-Security": in.Defaults.StrictTransportSecurity,
			}}},
		},
	}, nil
}

// upstream renders the upstream of one deployable. Edge sends the name derived from the slug as
// Host and verifies the upstream's certificate against it, and never retries (section 06).
func upstream(slug Slug, instances []Instance) Upstream {
	nodes := make([]Node, len(instances))
	for i, instance := range instances {
		nodes[i] = Node{Host: instance.Address, Port: instance.Port, Weight: instance.Weight}
	}
	return Upstream{
		ID:           string(slug),
		Name:         string(slug),
		Type:         "roundrobin",
		Scheme:       "https",
		PassHost:     "rewrite",
		UpstreamHost: string(slug),
		TLS:          UpstreamTLS{Verify: true},
		Retries:      0,
		Nodes:        nodes,
	}
}

// onListener is the condition that binds a route to one listener. A route without it answers on
// every listener.
func onListener(port int) [][]any {
	return [][]any{{"server_port", "==", port}}
}

// routeID derives a route's id from its listener, the deployable's slug, its paths and its methods,
// so that the id stays the same while they do, and two routes on the same paths with other methods
// get their own ids. The hash reads the sorted paths and then the sorted methods, one per line; a
// path starts with / and a method never does. APISIX allows at most 64 characters of [a-zA-Z0-9._-].
func routeID(listener string, slug Slug, paths, methods []string) string {
	sortedPaths, sortedMethods := slices.Sorted(slices.Values(paths)), slices.Sorted(slices.Values(methods))
	sum := sha256.Sum256([]byte(strings.Join(slices.Concat(sortedPaths, sortedMethods), "\n")))
	return listener + "." + string(slug) + "." + hex.EncodeToString(sum[:6])
}
