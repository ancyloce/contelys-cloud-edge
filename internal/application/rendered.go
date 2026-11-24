package application

import (
	"fmt"
	"slices"
)

// checkRendered applies the rules of section 03 to a rendered apisix.yaml: every route and upstream
// carries an explicit id, no two items of one kind share an id, and every route carries a name and a
// condition that binds it to one of Edge's listeners. APISIX gives an item without an id one from
// its position in the file, lets the later of two items with one id replace the earlier, and lets a
// route without the condition answer on every listener. No input can break these rules: they guard
// against a defect of Edge's own templates, so a broken one is an error and not a refusal.
func checkRendered(file ApisixFile, listeners Listeners) error {
	fail := func(format string, a ...any) error {
		return fmt.Errorf("the rendered apisix.yaml: "+format, a...)
	}
	for i, route := range file.Routes {
		switch {
		case route.ID == "":
			return fail("route %d, to %s, has no explicit id", i+1, route.UpstreamID)
		case route.Name == "":
			return fail("route %s, to %s, has no name", route.ID, route.UpstreamID)
		case !boundToListener(route.Vars, listeners):
			return fail("route %s, to %s, has no condition that binds it to one of Edge's listeners, so it would answer on every listener", route.ID, route.UpstreamID)
		}
	}
	for i, upstream := range file.Upstreams {
		if upstream.ID == "" {
			return fail("upstream %d has no explicit id", i+1)
		}
	}
	for _, kind := range []struct {
		name string
		ids  []string
	}{
		{"routes", file.routeIDs()},
		{"upstreams", file.upstreamIDs()},
		{"ssls", file.sslIDs()},
	} {
		for i, id := range kind.ids {
			if earlier := slices.Index(kind.ids[:i], id); earlier >= 0 {
				return fail("%s %d and %d have the id %s, and APISIX would let the later replace the earlier", kind.name, earlier+1, i+1, id)
			}
		}
	}
	return nil
}

// boundToListener reports whether vars hold the condition of onListener for the port of the
// external, internal or loopback listener.
func boundToListener(vars [][]any, listeners Listeners) bool {
	for _, port := range []int{listeners.ExternalPort, listeners.InternalPort, listeners.LoopbackPort} {
		if slices.ContainsFunc(vars, func(condition []any) bool {
			return slices.Equal(condition, onListener(port)[0])
		}) {
			return true
		}
	}
	return false
}
