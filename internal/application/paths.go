package application

import (
	"fmt"
	"regexp"
	"strings"
)

// The rules of section 03 for the paths of route fragments. A path is exact unless it ends in /*,
// which makes it a prefix of every path under it.

// remainder claims every path of the application hostname that no other route claims.
const remainder = "/*"

// checkPaths applies the rules of one fragment: it ships a fragment at all, no path lies under
// /internal, only Authoring claims the remainder, every path is in the deployable's OpenAPI contract
// except Authoring's, and two of its routes list one path only for methods that the other does not.
func checkPaths(fragment Fragment, contracts map[Slug][]string) error {
	refuse := func(format string, a ...any) error {
		return &Refusal{Fragment: fragment.Slug, Rule: fmt.Sprintf(format, a...)}
	}
	if fragment.Slug == KeycloakSlug || fragment.Slug == EdgeSlug {
		return refuse("Edge's own template holds the routes to %s, so it ships no fragment", fragment.Slug)
	}
	contract, hasContract := contracts[fragment.Slug]
	if fragment.Slug != AuthoringSlug && !hasContract {
		return refuse("the deployable has no OpenAPI contract")
	}
	listedBy := map[string]int{} // "METHOD path" to the number of the route that lists it
	for i, route := range fragment.Routes {
		n := i + 1
		for _, path := range route.Paths {
			switch {
			case underInternal(path):
				return refuse("route %d: %s lies under /internal, which no fragment may claim", n, path)
			case path == remainder && fragment.Slug != AuthoringSlug:
				return refuse("route %d: %s claims the remainder of the application hostname, which only Authoring's fragment may claim", n, path)
			case fragment.Slug != AuthoringSlug && !inContract(path, contract):
				return refuse("route %d: %s is not a path of the deployable's OpenAPI contract", n, path)
			}
			for _, method := range route.Methods {
				listed := method + " " + path
				if earlier, found := listedBy[listed]; found {
					return refuse("routes %d and %d both list %s", earlier, n, listed)
				}
				listedBy[listed] = n
			}
		}
	}
	return nil
}

// checkConflicts refuses a path of one fragment that equals a path of another or lies under it, so
// that each path belongs to exactly one deployable. Authoring's remainder is exempt, because a more
// specific route always wins over it. The fragments are sorted by slug, and the later fragment of
// a conflicting pair is refused.
func checkConflicts(fragments []Fragment) error {
	type claim struct {
		slug Slug
		path string
	}
	var earlier []claim
	for _, fragment := range fragments {
		var claims []claim
		for i, route := range fragment.Routes {
			for _, path := range route.Paths {
				if fragment.Slug == AuthoringSlug && path == remainder {
					continue
				}
				for _, other := range earlier {
					var rule string
					switch {
					case path == other.path:
						rule = fmt.Sprintf("route %d: %s is also a path of fragment %s", i+1, path, other.slug)
					case under(path, other.path):
						rule = fmt.Sprintf("route %d: %s lies under %s of fragment %s", i+1, path, other.path, other.slug)
					case under(other.path, path):
						rule = fmt.Sprintf("route %d: %s covers %s of fragment %s", i+1, path, other.path, other.slug)
					default:
						continue
					}
					return &Refusal{Fragment: fragment.Slug, Rule: rule}
				}
				claims = append(claims, claim{fragment.Slug, path})
			}
		}
		earlier = append(earlier, claims...)
	}
	return nil
}

// under reports whether path lies under prefix, a path that ends in /*: it starts with what precedes
// the * and is not the prefix itself.
func under(path, prefix string) bool {
	base, isPrefix := strings.CutSuffix(prefix, "*")
	return isPrefix && path != prefix && strings.HasPrefix(path, base)
}

// underInternal reports whether the first segment of path is internal, in any letters: Edge never
// publishes an internal operation on the external listener.
func underInternal(path string) bool {
	first, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	return strings.EqualFold(first, "internal")
}

// inContract reports whether a fragment's path is a path of the contract. An exact path matches one
// of the contract's path templates, in which a parameter such as {projectId} stands for one segment.
// A prefix covers at least one of them.
func inContract(path string, contract []string) bool {
	base, isPrefix := strings.CutSuffix(path, "*")
	segments := strings.Split(strings.TrimPrefix(base, "/"), "/")
	if isPrefix {
		segments = segments[:len(segments)-1] // what follows the slash before the *
	}
	for _, template := range contract {
		templateSegments := strings.Split(strings.TrimPrefix(template, "/"), "/")
		switch {
		case isPrefix && len(templateSegments) > len(segments) && matchSegments(templateSegments[:len(segments)], segments):
			return true
		case !isPrefix && len(templateSegments) == len(segments) && matchSegments(templateSegments, segments):
			return true
		}
	}
	return false
}

// matchSegments reports whether each segment of a path matches the template's segment at its place.
func matchSegments(template, segments []string) bool {
	for i, segment := range segments {
		if !matchSegment(template[i], segment) {
			return false
		}
	}
	return true
}

var parameter = regexp.MustCompile(`\{[^{}/]+\}`)

// matchSegment reports whether one segment of a path matches one segment of a path template, in
// which each parameter stands for at least one character, as in {name} or {name}.{format}.
func matchSegment(template, segment string) bool {
	literals := parameter.Split(template, -1)
	for i, literal := range literals {
		literals[i] = regexp.QuoteMeta(literal)
	}
	return regexp.MustCompile("^" + strings.Join(literals, ".+") + "$").MatchString(segment)
}
