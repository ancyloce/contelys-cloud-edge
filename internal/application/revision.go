package application

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// RevisionPath is the path of the fixed-response route, under Edge's own slug on the internal
// listener (section 03).
const RevisionPath = "/" + string(EdgeSlug) + "/revision"

// FixedResponsePlugin is the built-in plugin that answers the fixed-response route. On the test
// baseline mocking and fault-injection both answer it, and both read a $ in the body as a variable;
// mocking is the one made for a fixed answer.
const FixedResponsePlugin = "mocking"

// RevisionRouteID is the id of the fixed-response route, which answers GET on the internal listener.
var RevisionRouteID = routeID("internal", EdgeSlug, revisionPaths, revisionMethods)

var revisionPaths, revisionMethods = []string{RevisionPath}, []string{http.MethodGet}

// DigestPrefix names the hash function of a digest.
const DigestPrefix = "sha256:"

// DigestPlaceholder stands for the digest of the file in the fixed-response route's answer, because
// the digest cannot cover its own text. The sink encodes the file, takes the SHA-256 of those bytes
// and puts it in the placeholder's place, so the digest of section 03 is the SHA-256 of the file as
// it reads with zeros in place of the digest's hexadecimal digits. The zeros keep the digest's
// length, so the replacement changes nothing else in the file.
var DigestPlaceholder = DigestPrefix + strings.Repeat("0", hex.EncodedLen(sha256.Size))

// Answer is the body of the fixed-response route: the configuration revision and the digest of the
// file that the instance applied.
type Answer struct {
	Revision string `json:"revision"`
	Digest   string `json:"digest"`
}

// revisionPattern admits a Git commit and leaves out every character that the answer cannot carry
// as it is: a $, which the plugin reads as a variable, and quotes, backslashes and white space.
var revisionPattern = regexp.MustCompile(`^[0-9A-Za-z._-]+$`)

// checkRevision applies the rule of the configuration revision, which the command line gives.
func checkRevision(revision string) error {
	if !revisionPattern.MatchString(revision) {
		return fmt.Errorf("the configuration revision %q is not a Git commit: it may hold only letters, digits, \".\", \"_\" and \"-\"", revision)
	}
	return nil
}

// revisionRoute renders the fixed-response route. It answers GET on the internal listener alone,
// under any host, with the revision and the placeholder of the digest, and has no upstream.
func revisionRoute(in Inputs) (Route, error) {
	answer, err := json.Marshal(Answer{Revision: in.Revision, Digest: DigestPlaceholder})
	if err != nil {
		return Route{}, err
	}
	return Route{
		ID:      RevisionRouteID,
		Name:    routeName(EdgeSlug, revisionPaths, revisionMethods),
		URIs:    revisionPaths,
		Methods: revisionMethods,
		Vars:    onListener(in.Defaults.Listeners.InternalPort),
		Plugins: map[string]any{FixedResponsePlugin: Mocking{ContentType: "application/json", ResponseExample: string(answer)}},
	}, nil
}
