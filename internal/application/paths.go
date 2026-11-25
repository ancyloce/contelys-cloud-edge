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
// /internal, a domain service's paths lie under /api/ and Authoring's outside it, only Authoring
// claims the remainder, every path is in the deployable's OpenAPI contract except Authoring's, and
// two of its routes list one path only for methods that the other does not.
func checkPaths(fragment Fragment, contracts map[Slug][]string) error {
	refuse := func(format string, a ...any) error {
		return &Refusal{Fragment: fragment.Slug, Rule: fmt.Sprintf(format, a...)}
	}
	if fragment.Slug == keycloakSlug || fragment.Slug == EdgeSlug {
		return refuse("Edge's own template holds the routes to %s, so it ships no fragment", fragment.Slug)
	}
	authoring := fragment.Slug == AuthoringSlug
	contract, hasContract := contracts[fragment.Slug]
	if !authoring && !hasContract {
		return refuse("the deployable has no OpenAPI contract")
	}
	listedBy := map[string]int{} // "METHOD path" to the number of the route that lists it
	for i, route := range fragment.Routes {
		n := i + 1
		for _, path := range route.Paths {
			switch {
			case underInternal(path):
				return refuse("route %d: %s lies under /internal, which no fragment may claim", n, path)
			case path == remainder && !authoring:
				return refuse("route %d: %s claims the remainder of the application hostname, which only Authoring's fragment may claim", n, path)
			case !authoring && !underAPI(path):
				return refuse("route %d: %s lies outside /api/, where the API lives, and only Authoring's fragment claims paths outside it", n, path)
			case authoring && path != remainder && underAPI(path):
				return refuse("route %d: %s lies under /api/, which the API reserves", n, path)
			case !authoring && !inContract(path, contract):
				if _, isPrefix := prefixBase(path); isPrefix {
					return refuse("route %d: %s stands for no parameter of a path of the deployable's OpenAPI contract", n, path)
				}
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

// prefixBase is what precedes the * of a prefix, a path that ends in /*, such as /api/v1/projects/
// for /api/v1/projects/*. It reports false for an exact path.
func prefixBase(path string) (string, bool) {
	if !strings.HasSuffix(path, "/*") {
		return "", false
	}
	return strings.TrimSuffix(path, "*"), true
}

// under reports whether path lies under prefix, a path that ends in /*: it starts with what precedes
// the * and is not the prefix itself.
func under(path, prefix string) bool {
	base, isPrefix := prefixBase(prefix)
	return isPrefix && path != prefix && strings.HasPrefix(path, base)
}

// underInternal reports whether the first segment of path is internal, in any letters: Edge never
// publishes an internal operation on the external listener.
func underInternal(path string) bool {
	return strings.EqualFold(firstSegment(path), "internal")
}

// underAPI reports whether path lies in the reserved prefix /api/, where the API lives.
func underAPI(path string) bool {
	return firstSegment(path) == "api"
}

func firstSegment(path string) string {
	first, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	return first
}

// inContract reports whether a fragment's path is a path of the contract. An exact path matches one
// of the contract's path templates, in which a parameter such as {projectId} stands for one segment.
// A prefix stands for a parameter: the segment that follows it in one of the templates holds one, as
// /api/v1/projects/* stands for {projectId} in /api/v1/projects/{projectId}.
func inContract(path string, contract []string) bool {
	base, isPrefix := prefixBase(path)
	if !isPrefix {
		base = path
	}
	segments := strings.Split(strings.TrimPrefix(base, "/"), "/")
	if isPrefix {
		segments = segments[:len(segments)-1] // what follows the slash before the *
	}
	for _, template := range contract {
		templateSegments := strings.Split(strings.TrimPrefix(template, "/"), "/")
		switch {
		case isPrefix && len(templateSegments) > len(segments) && matchSegments(templateSegments[:len(segments)], segments) &&
			parameter.MatchString(templateSegments[len(segments)]):
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
