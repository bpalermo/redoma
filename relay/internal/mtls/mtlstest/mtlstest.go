// Package mtlstest issues throwaway SPIFFE certificates for the relay's tests.
package mtlstest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

// CA is a self-signed CA of one trust domain.
type CA struct {
	TrustDomain string
	cert        *x509.Certificate
	key         *ecdsa.PrivateKey
}

// NewCA returns a CA for trustDomain.
func NewCA(t testing.TB, trustDomain string) *CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA " + trustDomain},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
		URIs:                  []*url.URL{{Scheme: "spiffe", Host: trustDomain}},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &CA{TrustDomain: trustDomain, cert: cert, key: key}
}

// Pool is the CA as a bundle.
func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

// BundlePEM is the CA's certificate, PEM.
func (ca *CA) BundlePEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
}

// Issue issues a certificate with the URI SANs ids (none for none), as an
// X.509 SVID is: digital signature, client and server auth.
func (ca *CA) Issue(t testing.TB, ids ...string) tls.Certificate {
	t.Helper()
	certPEM, keyPEM := ca.IssuePEM(t, ids...)
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// IssuePEM is Issue, as the PEM of the certificate and of its key.
func (ca *CA) IssuePEM(t testing.TB, ids ...string) ([]byte, []byte) {
	t.Helper()
	der, pkcs8 := ca.IssueDER(t, ids...)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
}

// DER is the CA's certificate.
func (ca *CA) DER() []byte { return ca.cert.Raw }

// Static is a relay identity that never changes (svid.Source).
type Static struct {
	SVID   *x509svid.SVID
	Bundle *x509bundle.Bundle
}

// GetX509SVID returns SVID.
func (s Static) GetX509SVID() (*x509svid.SVID, error) { return s.SVID, nil }

// GetX509BundleForTrustDomain returns Bundle for its trust domain.
func (s Static) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return s.Bundle.GetX509BundleForTrustDomain(td)
}

// Source is a Static identity of id with the CA as the bundle.
func (ca *CA) Source(t testing.TB, id string) Static {
	t.Helper()
	der, key := ca.IssueDER(t, id)
	svid, err := x509svid.ParseRaw(der, key)
	if err != nil {
		t.Fatal(err)
	}
	return Static{SVID: svid, Bundle: x509bundle.FromX509Authorities(spiffeid.RequireTrustDomainFromString(ca.TrustDomain), []*x509.Certificate{ca.cert})}
}

// IssueDER is Issue, as the DER of the certificate and its PKCS#8 key.
func (ca *CA) IssueDER(t testing.TB, ids ...string) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var uris []*url.URL
	for _, id := range ids {
		u, err := url.Parse(id)
		if err != nil {
			t.Fatal(err)
		}
		uris = append(uris, u)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:         uris,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return der, pkcs8
}
