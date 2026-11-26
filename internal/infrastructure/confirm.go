package infrastructure

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"

	"github.com/ancyloce/contelys-cloud-edge/internal/application"
)

// ReadDistributed implements application.Distribution. It reads <out>/<instance>/apisix.yaml as the
// sink wrote it: the ids of its routes and upstreams, and the answer of its fixed-response route. It
// recomputes the file's digest, so that a file changed after rendering is not taken for the rendered
// one, also where no id changed.
func (Files) ReadDistributed(req application.ConfirmRequest) (application.Distributed, error) {
	path := filepath.Join(req.Out, req.Instance, "apisix.yaml")
	content, err := os.ReadFile(path)
	if err != nil {
		return application.Distributed{}, err
	}
	var file struct {
		Routes []struct {
			ID      string               `yaml:"id"`
			Plugins map[string]yaml.Node `yaml:"plugins"`
		} `yaml:"routes"`
		Upstreams []struct {
			ID string `yaml:"id"`
		} `yaml:"upstreams"`
	}
	if err := yaml.Unmarshal(content, &file); err != nil {
		return application.Distributed{}, fmt.Errorf("%s: %w", path, err)
	}
	var distributed application.Distributed
	answered := false
	for _, route := range file.Routes {
		distributed.Routes = append(distributed.Routes, route.ID)
		if route.ID != application.RevisionRouteID {
			continue
		}
		if err := decodeAnswer(route.Plugins, &distributed.Answer); err != nil {
			return application.Distributed{}, fmt.Errorf("%s: the fixed-response route %s carries no answer: %w", path, route.ID, err)
		}
		answered = true
	}
	if !answered {
		return application.Distributed{}, fmt.Errorf("%s holds no fixed-response route", path)
	}
	if err := checkDigest(content, distributed.Answer.Digest); err != nil {
		return application.Distributed{}, fmt.Errorf("%s: %w", path, err)
	}
	for _, upstream := range file.Upstreams {
		distributed.Upstreams = append(distributed.Upstreams, upstream.ID)
	}
	return distributed, nil
}

// checkDigest recomputes the digest of a distributed file as section 03 defines it, with the
// placeholder in place of the digest that its fixed-response route carries.
func checkDigest(content []byte, digest string) error {
	if len(digest) != len(application.DigestPlaceholder) || bytes.Count(content, []byte(digest)) != 1 {
		return fmt.Errorf("its fixed-response route carries the digest %q, which the file does not hold once", digest)
	}
	if got := digestOf(bytes.Replace(content, []byte(digest), []byte(application.DigestPlaceholder), 1)); got != digest {
		return fmt.Errorf("its digest is %s, not the %s that its fixed-response route carries: the file changed after rendering", got, digest)
	}
	return nil
}

// decodeAnswer decodes the answer that the plugin of the fixed-response route gives.
func decodeAnswer(plugins map[string]yaml.Node, answer *application.Answer) error {
	node, found := plugins[application.FixedResponsePlugin]
	if !found {
		return fmt.Errorf("it has no plugin %s", application.FixedResponsePlugin)
	}
	var mocking application.Mocking
	if err := node.Decode(&mocking); err != nil {
		return err
	}
	return json.Unmarshal([]byte(mocking.ResponseExample), answer)
}

// Endpoints implements application.RunningInstances with the listeners that the command line names.
type Endpoints struct{}

// answerLimit bounds how much of an answer of the fixed-response route is read.
const answerLimit = 1 << 12

// Open implements application.RunningInstances. The internal listener is verified against the trust store
// of the secret files and the name edge, which its certificate carries (section 07).
func (Endpoints) Open(req application.ConfirmRequest) (application.RunningInstance, error) {
	trustStore, err := os.ReadFile(filepath.Join(req.Secrets, TrustStoreFile))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(trustStore) {
		return nil, fmt.Errorf("%s holds no certificate", TrustStoreFile)
	}
	return endpoints{
		internal: &http.Client{Timeout: controlRequestTimeout, Transport: &http.Transport{
			// A connection stays with one worker process, and each worker process applies a changed
			// file on its own, so every read opens a connection of its own.
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{RootCAs: pool, ServerName: string(application.EdgeSlug)},
		}},
		answerURL: "https://" + req.Internal + application.RevisionPath,
		control:   &http.Client{Timeout: controlRequestTimeout},
		base:      "http://" + req.Control,
	}, nil
}

type endpoints struct {
	internal  *http.Client
	answerURL string
	control   *http.Client
	base      string
}

func (e endpoints) Answer() (application.Answer, error) {
	response, err := e.internal.Get(e.answerURL)
	if err != nil {
		return application.Answer{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, answerLimit))
	if err != nil {
		return application.Answer{}, err
	}
	if response.StatusCode != http.StatusOK {
		return application.Answer{}, fmt.Errorf("%s answered %d", e.answerURL, response.StatusCode)
	}
	var answer application.Answer
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&answer); err != nil {
		return application.Answer{}, fmt.Errorf("%s: %w", e.answerURL, err)
	}
	return answer, nil
}

func (e endpoints) Loaded() (application.Loaded, error) {
	routes, err := loadedIDs(context.Background(), e.control, e.base+"/v1/routes")
	if err != nil {
		return application.Loaded{}, err
	}
	upstreams, err := loadedIDs(context.Background(), e.control, e.base+"/v1/upstreams")
	if err != nil {
		return application.Loaded{}, err
	}
	return application.Loaded{Routes: routes, Upstreams: upstreams}, nil
}
