package infrastructure

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/contelys/contelys-cloud-edge/internal/application"
)

// FragmentFile is the name of a route fragment in its deployable's directory (ADR 0058). The
// renderer reads <fragments>/<slug>/route-fragment.yaml for every deployable that has one.
const FragmentFile = "route-fragment.yaml"

// The secret files that the renderer reads from the secrets directory.
const (
	ApplicationCertificateFile = "application.crt"
	ApplicationKeyFile         = "application.key"
	// TrustStoreFile is the installation's certificate authority. config.yaml names it at its
	// place in the container, and the renderer only checks that it holds a certificate.
	TrustStoreFile = "ca.crt"
)

var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// Files reads the inputs from files and writes the outputs to files.
type Files struct{}

// Read implements application.Source.
func (Files) Read(req application.Request) (application.Inputs, error) {
	defaults, err := Defaults()
	if err != nil {
		return application.Inputs{}, err
	}
	instances, err := readInstances(req.Instances)
	if err != nil {
		return application.Inputs{}, err
	}
	installation, err := readInstallation(req.Installation)
	if err != nil {
		return application.Inputs{}, err
	}
	fragments, err := readFragments(req.Fragments)
	if err != nil {
		return application.Inputs{}, err
	}
	secrets, err := readSecrets(req.Secrets)
	if err != nil {
		return application.Inputs{}, err
	}
	if req.Revision == "" {
		return application.Inputs{}, errors.New("the configuration revision is empty")
	}
	return application.Inputs{
		Defaults:     defaults,
		Instances:    instances,
		Installation: installation,
		Fragments:    fragments,
		Secrets:      secrets,
		Revision:     req.Revision,
	}, nil
}

// decodeStrict decodes one YAML document and fails on a field that v does not declare.
func decodeStrict(path string, v any) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func readInstances(path string) (application.Instances, error) {
	var instances application.Instances
	if err := decodeStrict(path, &instances); err != nil {
		return nil, err
	}
	for slug, list := range instances {
		if !slugPattern.MatchString(slug) {
			return nil, fmt.Errorf("%s: %q is not a slug", path, slug)
		}
		for i, instance := range list {
			switch {
			case instance.Address == "":
				return nil, fmt.Errorf("%s: %s, instance %d: the address is empty", path, slug, i+1)
			case instance.Port < 1 || instance.Port > 65535:
				return nil, fmt.Errorf("%s: %s, instance %d: the port %d is not a port", path, slug, i+1, instance.Port)
			case instance.Weight != 0 && instance.Weight != 1:
				return nil, fmt.Errorf("%s: %s, instance %d: the weight is 1 to serve or 0 to drain, not %d", path, slug, i+1, instance.Weight)
			}
		}
	}
	return instances, nil
}

func readInstallation(path string) (application.Installation, error) {
	var installation application.Installation
	if err := decodeStrict(path, &installation); err != nil {
		return application.Installation{}, err
	}
	if installation.ApplicationHostname == "" || installation.AuthenticationHostname == "" {
		return application.Installation{}, fmt.Errorf("%s: the application hostname and the authentication hostname are required", path)
	}
	for _, address := range slices.Concat(installation.LoadBalancerAddresses, installation.CollectorAddresses) {
		if _, err := netip.ParseAddr(address); err != nil {
			if _, err := netip.ParsePrefix(address); err != nil {
				return application.Installation{}, fmt.Errorf("%s: %q is neither an address nor a prefix", path, address)
			}
		}
	}
	return installation, nil
}

func readFragments(dir string) ([]application.Fragment, error) {
	schema, err := fragmentSchema()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir) // sorted by name, so the fragments are sorted by slug
	if err != nil {
		return nil, err
	}
	var fragments []application.Fragment
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name(), FragmentFile)
		content, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue // a deployable without external routes has no fragment
		}
		if err != nil {
			return nil, err
		}
		fragment, err := decodeFragment(schema, entry.Name(), content)
		if err != nil {
			return nil, err
		}
		fragments = append(fragments, fragment)
	}
	return fragments, nil
}

// decodeFragment checks a fragment against the fragment schema and decodes it. Every failure is a
// refusal that names the fragment.
func decodeFragment(schema *jsonschema.Schema, slug string, content []byte) (application.Fragment, error) {
	refuse := func(rule string) error { return &application.Refusal{Fragment: slug, Rule: rule} }
	if !slugPattern.MatchString(slug) {
		return application.Fragment{}, refuse("the directory name is not a slug")
	}
	var document any
	if err := yaml.Unmarshal(content, &document); err != nil {
		return application.Fragment{}, refuse("not YAML: " + err.Error())
	}
	// The schema is checked on the JSON form of the document, which is what JSON Schema describes.
	asJSON, err := json.Marshal(document)
	if err != nil {
		return application.Fragment{}, refuse("not a JSON-compatible document: " + err.Error())
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(asJSON))
	if err != nil {
		return application.Fragment{}, refuse(err.Error())
	}
	if err := schema.Validate(instance); err != nil {
		return application.Fragment{}, refuse("breaks the fragment schema: " + schemaFailure(err))
	}
	var decoded struct {
		Routes []application.FragmentRoute `yaml:"routes"`
	}
	if err := yaml.Unmarshal(content, &decoded); err != nil {
		return application.Fragment{}, refuse(err.Error())
	}
	return application.Fragment{Slug: slug, Routes: decoded.Routes}, nil
}

func fragmentSchema() (*jsonschema.Schema, error) {
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(application.FragmentSchema))
	if err != nil {
		return nil, fmt.Errorf("the embedded fragment schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("fragment.schema.json", document); err != nil {
		return nil, err
	}
	return compiler.Compile("fragment.schema.json")
}

var english = message.NewPrinter(language.English)

// schemaFailure names the first place in the fragment that breaks the schema, and why.
func schemaFailure(err error) string {
	var validation *jsonschema.ValidationError
	if !errors.As(err, &validation) {
		return err.Error()
	}
	for len(validation.Causes) > 0 {
		validation = validation.Causes[0]
	}
	location := "/"
	if len(validation.InstanceLocation) > 0 {
		location = ""
		for _, part := range validation.InstanceLocation {
			location += "/" + part
		}
	}
	return fmt.Sprintf("at %s: %s", location, validation.ErrorKind.LocalizedString(english))
}

func readSecrets(dir string) (application.Secrets, error) {
	read := func(name string) (string, error) {
		content, err := os.ReadFile(filepath.Join(dir, name))
		return string(content), err
	}
	certificate, err := read(ApplicationCertificateFile)
	if err != nil {
		return application.Secrets{}, err
	}
	key, err := read(ApplicationKeyFile)
	if err != nil {
		return application.Secrets{}, err
	}
	if _, err := tls.X509KeyPair([]byte(certificate), []byte(key)); err != nil {
		return application.Secrets{}, fmt.Errorf("%s and %s: %w", ApplicationCertificateFile, ApplicationKeyFile, err)
	}
	trustStore, err := read(TrustStoreFile)
	if err != nil {
		return application.Secrets{}, err
	}
	if !x509.NewCertPool().AppendCertsFromPEM([]byte(trustStore)) {
		return application.Secrets{}, fmt.Errorf("%s holds no certificate", TrustStoreFile)
	}
	return application.Secrets{ApplicationCertificate: certificate, ApplicationKey: key}, nil
}

// Write implements application.Sink. Each file is written beside the current one and renamed into
// place, so that APISIX never reads a half-written file. apisix.yaml holds keys, so only its owner
// may read it.
func (Files) Write(dir string, out application.Output) error {
	for _, instance := range out.Instances {
		content, err := encode(instance.File)
		if err != nil {
			return err
		}
		// APISIX reads a file only if it ends with this marker.
		content = append(content, "#END\n"...)
		instanceDir := filepath.Join(dir, instance.Address)
		if err := os.MkdirAll(instanceDir, 0o755); err != nil {
			return err
		}
		if err := writeAtomically(filepath.Join(instanceDir, "apisix.yaml"), content, 0o600); err != nil {
			return err
		}
	}
	content, err := encode(out.Config)
	if err != nil {
		return err
	}
	return writeAtomically(filepath.Join(dir, "config.yaml"), content, 0o644)
}

func encode(v any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func writeAtomically(path string, content []byte, mode os.FileMode) (err error) {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(temporary.Name())
		}
	}()
	if _, err = temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err = temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if err = temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}
