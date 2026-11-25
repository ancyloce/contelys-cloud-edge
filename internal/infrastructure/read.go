package infrastructure

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"

	"github.com/contelys/contelys-cloud-edge/internal/application"
)

// The secret files that the renderer reads from the secrets directory.
const (
	ApplicationCertificateFile = "application.crt"
	ApplicationKeyFile         = "application.key"
	// EdgeCertificateFile and EdgeKeyFile are Edge's own certificate, which carries the name edge and
	// serves the internal listener (section 07).
	EdgeCertificateFile = "edge.crt"
	EdgeKeyFile         = "edge.key"
	// TrustStoreFile is the installation's certificate authority. config.yaml names it at its
	// place in the container, and the renderer checks that it holds a certificate and hands it to
	// the instance of the trial load.
	TrustStoreFile = "ca.crt"
)

// Files reads the inputs from files and writes the outputs to files.
type Files struct{}

// Read implements application.Source. It decodes the files strictly; the application checks their
// rules.
func (Files) Read(req application.Request) (application.Inputs, error) {
	defaults, err := Defaults()
	if err != nil {
		return application.Inputs{}, err
	}
	var instances application.Instances
	if err := decodeStrict(req.Instances, &instances); err != nil {
		return application.Inputs{}, err
	}
	var installation application.Installation
	if err := decodeStrict(req.Installation, &installation); err != nil {
		return application.Inputs{}, err
	}
	fragments, err := readFragments(req.Fragments)
	if err != nil {
		return application.Inputs{}, err
	}
	contracts, err := readContracts(req.Contracts, fragments)
	if err != nil {
		return application.Inputs{}, err
	}
	secrets, err := readSecrets(req.Secrets)
	if err != nil {
		return application.Inputs{}, err
	}
	return application.Inputs{
		Defaults:     defaults,
		Instances:    instances,
		Installation: installation,
		Fragments:    fragments,
		Contracts:    contracts,
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

func readSecrets(dir string) (application.Secrets, error) {
	read := func(name string) (string, error) {
		content, err := os.ReadFile(filepath.Join(dir, name))
		return string(content), err
	}
	// pair reads a certificate and its key, and checks that they belong together.
	pair := func(certificateFile, keyFile string) (string, string, error) {
		certificate, err := read(certificateFile)
		if err != nil {
			return "", "", err
		}
		key, err := read(keyFile)
		if err != nil {
			return "", "", err
		}
		if _, err := tls.X509KeyPair([]byte(certificate), []byte(key)); err != nil {
			return "", "", fmt.Errorf("%s and %s: %w", certificateFile, keyFile, err)
		}
		return certificate, key, nil
	}
	certificate, key, err := pair(ApplicationCertificateFile, ApplicationKeyFile)
	if err != nil {
		return application.Secrets{}, err
	}
	edgeCertificate, edgeKey, err := pair(EdgeCertificateFile, EdgeKeyFile)
	if err != nil {
		return application.Secrets{}, err
	}
	trustStore, err := read(TrustStoreFile)
	if err != nil {
		return application.Secrets{}, err
	}
	if !x509.NewCertPool().AppendCertsFromPEM([]byte(trustStore)) {
		return application.Secrets{}, fmt.Errorf("%s holds no certificate", TrustStoreFile)
	}
	return application.Secrets{
		ApplicationCertificate: certificate, ApplicationKey: key,
		EdgeCertificate: edgeCertificate, EdgeKey: edgeKey,
		TrustStore: trustStore,
	}, nil
}
