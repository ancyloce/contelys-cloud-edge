// Package fixture holds what the renderer's tests and the scenarios share: a throwaway certificate
// authority and a directory of the renderer's inputs. Keys exist only in memory and in temporary
// directories outside the repository, so that no key is ever tracked by Git.
package fixture

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/contelys/contelys-cloud-edge/internal/application"
	"github.com/contelys/contelys-cloud-edge/internal/infrastructure"
)

// The values that every set of inputs shares.
const (
	ApplicationHostname    = "app.example.test"
	AuthenticationHostname = "auth.example.test"
	Revision               = "0123456789abcdef0123456789abcdef01234567"
)

// Authority is a throwaway certificate authority.
type Authority struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	// PEM is the authority's certificate, which a trust store holds.
	PEM []byte
}

// NewAuthority generates a certificate authority that lives for one day.
func NewAuthority(t testing.TB) *Authority {
	t.Helper()
	key := generateKey(t)
	template := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: "Edge test certificate authority"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &Authority{certificate: certificate, key: key, PEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// Issue returns a server certificate for one name, as PEM and as a tls.Certificate.
func (a *Authority) Issue(t testing.TB, name string) (certificatePEM, keyPEM []byte, pair tls.Certificate) {
	t.Helper()
	key := generateKey(t)
	template := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.certificate, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	pair, err = tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return certificatePEM, keyPEM, pair
}

// Pool is a certificate pool that trusts only this authority.
func (a *Authority) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(a.certificate)
	return pool
}

func generateKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func serial(t testing.TB) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Inputs is a directory that holds a complete set of the renderer's inputs.
type Inputs struct {
	Dir string
}

// NewInputs writes a complete set of inputs into a temporary directory: an instances file with one
// Edge instance at edgeAddress, an installation file without load-balancer addresses, an empty
// fragments directory, and the application hostname's certificate and the trust store, both from ca.
func NewInputs(t testing.TB, ca *Authority, edgeAddress string) *Inputs {
	t.Helper()
	in := &Inputs{Dir: t.TempDir()}
	in.Instances(t, application.Instances{application.EdgeSlug: {{Address: edgeAddress, Port: 9443, Weight: 1}}})
	in.Installation(t)
	if err := os.MkdirAll(filepath.Join(in.Dir, "fragments"), 0o755); err != nil {
		t.Fatal(err)
	}
	certificate, key, _ := ca.Issue(t, ApplicationHostname)
	in.Write(t, filepath.Join("secrets", infrastructure.ApplicationCertificateFile), string(certificate))
	in.Write(t, filepath.Join("secrets", infrastructure.ApplicationKeyFile), string(key))
	in.Write(t, filepath.Join("secrets", infrastructure.TrustStoreFile), string(ca.PEM))
	return in
}

// Write writes one input file, relative to the directory.
func (in *Inputs) Write(t testing.TB, name, content string) {
	t.Helper()
	path := filepath.Join(in.Dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimLeft(content, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Read reads one input file, relative to the directory.
func (in *Inputs) Read(t testing.TB, name string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(in.Dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// Instances replaces the instances file.
func (in *Inputs) Instances(t testing.TB, instances application.Instances) {
	t.Helper()
	content, err := yaml.Marshal(instances)
	if err != nil {
		t.Fatal(err)
	}
	in.Write(t, "instances.yaml", string(content))
}

// Installation replaces the installation file, with the given load-balancer addresses.
func (in *Inputs) Installation(t testing.TB, loadBalancerAddresses ...string) {
	t.Helper()
	content, err := yaml.Marshal(application.Installation{
		ApplicationHostname:    ApplicationHostname,
		AuthenticationHostname: AuthenticationHostname,
		LoadBalancerAddresses:  append([]string{}, loadBalancerAddresses...),
		CollectorAddresses:     []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	in.Write(t, "installation.yaml", string(content))
}

// Fragment writes the route fragment of the deployable slug.
func (in *Inputs) Fragment(t testing.TB, slug, content string) {
	t.Helper()
	in.Write(t, filepath.Join("fragments", slug, infrastructure.FragmentFile), content)
}

// Request names these inputs, with out as the output directory.
func (in *Inputs) Request(out string) application.Request {
	return application.Request{
		Instances:    filepath.Join(in.Dir, "instances.yaml"),
		Installation: filepath.Join(in.Dir, "installation.yaml"),
		Fragments:    filepath.Join(in.Dir, "fragments"),
		Secrets:      filepath.Join(in.Dir, "secrets"),
		Revision:     Revision,
		Out:          out,
	}
}

// Args is the command line of edge render for these inputs.
func (in *Inputs) Args(out string) []string {
	req := in.Request(out)
	return []string{"render",
		"--instances", req.Instances,
		"--installation", req.Installation,
		"--fragments", req.Fragments,
		"--secrets", req.Secrets,
		"--revision", req.Revision,
		"--out", req.Out,
	}
}
