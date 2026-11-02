// Package rolesanywheretest is a fake IAM Roles Anywhere endpoint that checks
// a CreateSession request as AWS would, and a throwaway SPIRE-like CA that
// issues SVIDs to sign it with, for tests that need no AWS.
package rolesanywheretest

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// SVID issues what SPIRE does for an agent: a root, an intermediate under it
// and a leaf whose URI SAN is the agent's SPIFFE ID, PEM-encoded as
// identity.X509SVID holds it (leaf then intermediate; a PKCS #8 key). rsaLeaf
// gives the leaf an RSA key and no intermediate.
func SVID(t *testing.T, serial int64, rsaLeaf bool) (chainPEM, keyPEM []byte, leaf, intermediate *x509.Certificate) {
	t.Helper()
	issue := func(tmpl, parent *x509.Certificate, pub, signer any) *x509.Certificate {
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
		if err != nil {
			t.Fatal(err)
		}
		c, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	ecKey := func(c elliptic.Curve) *ecdsa.PrivateKey {
		k, err := ecdsa.GenerateKey(c, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	now := time.Now()
	rootKey := ecKey(elliptic.P384())
	rootT := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "root"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign,
	}
	root := issue(rootT, rootT, &rootKey.PublicKey, rootKey)
	parent, parentKey := root, any(rootKey)
	if !rsaLeaf {
		caKey := ecKey(elliptic.P384())
		intermediate = issue(&x509.Certificate{
			SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "intermediate"},
			NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
			KeyUsage: x509.KeyUsageCertSign,
		}, root, &caKey.PublicKey, rootKey)
		parent, parentKey = intermediate, caKey
	}
	var leafKey crypto.Signer = ecKey(elliptic.P256())
	if rsaLeaf {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		leafKey = k
	}
	id, _ := url.Parse("spiffe://redoma.internal/agent/example")
	leaf = issue(&x509.Certificate{
		SerialNumber: big.NewInt(serial), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		URIs: []*url.URL{id}, KeyUsage: x509.KeyUsageDigitalSignature,
	}, parent, leafKey.Public(), parentKey)
	for _, c := range []*x509.Certificate{leaf, intermediate} {
		if c != nil {
			chainPEM = append(chainPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
		}
	}
	der, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	return chainPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), leaf, intermediate
}

// Fake is a Roles Anywhere endpoint that checks a CreateSession request as
// AWS would (verify), then answers a session of the duration asked for,
// expiring from Now, unless Answer says otherwise.
type Fake struct {
	srv    *httptest.Server
	region string

	// Now is the fake's clock, which X-Amz-Date is checked against and
	// sessions expire from.
	Now func() time.Time

	mu       sync.Mutex
	calls    int
	lastErr  error
	lastBody map[string]any
	chain    []*x509.Certificate
	answer   func(w http.ResponseWriter)
	block    chan struct{}
}

// NewFake starts a fake endpoint of region's, closed when t ends.
func NewFake(t *testing.T, region string) *Fake {
	f := &Fake{region: region, Now: time.Now}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// Client sends every request to the fake, whatever its host, checking the
// fake's certificate as the one of example.com (httptest's).
func (f *Fake) Client() *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(f.srv.Certificate())
	addr := f.srv.Listener.Addr().String()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		},
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "example.com"},
	}}
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	err := f.verify(r, body)
	f.mu.Lock()
	f.calls++
	f.lastErr = err
	block, answer, fields := f.block, f.answer, f.lastBody
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if err != nil {
		http.Error(w, `{"message":"`+err.Error()+`"}`, http.StatusForbidden)
		return
	}
	if answer != nil {
		answer(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, `{"credentialSet":[{"assumedRoleUser":{},"credentials":{"accessKeyId":"ASIAEXAMPLE","secretAccessKey":"secret","sessionToken":"token","expiration":%q,"source":"rolesanywhere"},"roleArn":"x"}],"subjectArn":"x"}`,
		f.Now().Add(time.Duration(fields["durationSeconds"].(float64))*time.Second).UTC().Format(time.RFC3339))
}

// verify checks the request as Roles Anywhere would: the endpoint, the body,
// the certificate headers, and the signature rebuilt from what arrived and
// checked against the key of the certificate in X-Amz-X509.
func (f *Fake) verify(r *http.Request, body []byte) error {
	if r.Method != http.MethodPost || r.URL.Path != "/sessions" || r.URL.RawQuery != "" {
		return fmt.Errorf("%s %s", r.Method, r.URL)
	}
	if want := "rolesanywhere." + f.region + ".amazonaws.com"; r.Host != want {
		return fmt.Errorf("host %q, want %q", r.Host, want)
	}
	fields, err := checkBody(body)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.lastBody = fields
	f.mu.Unlock()
	leaf, chain, err := certificates(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.chain = chain
	f.mu.Unlock()
	stamp := r.Header.Get("X-Amz-Date")
	at, err := time.Parse("20060102T150405Z", stamp)
	if err != nil || at.Sub(f.Now()).Abs() > 5*time.Minute {
		return fmt.Errorf("X-Amz-Date %q", stamp)
	}
	a, err := parseAuthorization(r.Header.Get("Authorization"))
	if err != nil {
		return err
	}
	if a.serial != leaf.SerialNumber.String() {
		return fmt.Errorf("credential serial %q, want %s", a.serial, leaf.SerialNumber)
	}
	if want := stamp[:8] + "/" + f.region + "/rolesanywhere/aws4_request"; a.scope != want {
		return fmt.Errorf("scope %q, want %q", a.scope, want)
	}
	wantNames := "content-type;host;x-amz-date;x-amz-x509"
	if len(chain) > 0 {
		wantNames += ";x-amz-x509-chain"
	}
	if a.signedHeaders != wantNames {
		return fmt.Errorf("signed headers %q, want %q", a.signedHeaders, wantNames)
	}
	return a.verify(leaf, stringToSign(r, body, a, stamp))
}

// checkBody is the CreateSession body's fields, each one AWS requires
// there and of its type.
func checkBody(body []byte) (map[string]any, error) {
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	if d, ok := fields["durationSeconds"].(float64); !ok || d < 900 || d > 43200 {
		return nil, fmt.Errorf("durationSeconds %v", fields["durationSeconds"])
	}
	for _, k := range []string{"trustAnchorArn", "profileArn", "roleArn"} {
		if v, ok := fields[k].(string); !ok || v == "" {
			return nil, fmt.Errorf("no %s", k)
		}
	}
	return fields, nil
}

// certificates are the leaf of X-Amz-X509 and the intermediates of
// X-Amz-X509-Chain.
func certificates(r *http.Request) (*x509.Certificate, []*x509.Certificate, error) {
	leaf, err := parseBase64Certificate(r.Header.Get("X-Amz-X509"))
	if err != nil {
		return nil, nil, fmt.Errorf("X-Amz-X509: %w", err)
	}
	var chain []*x509.Certificate
	if header := r.Header.Get("X-Amz-X509-Chain"); header != "" {
		for _, c := range strings.Split(header, ",") {
			cert, err := parseBase64Certificate(c)
			if err != nil {
				return nil, nil, fmt.Errorf("X-Amz-X509-Chain: %w", err)
			}
			chain = append(chain, cert)
		}
	}
	return leaf, chain, nil
}

func parseBase64Certificate(s string) (*x509.Certificate, error) {
	der, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// authorization is the Authorization header's parts.
type authorization struct {
	algorithm, serial, scope, signedHeaders string
	signature                               []byte
}

func parseAuthorization(header string) (authorization, error) {
	algorithm, rest, _ := strings.Cut(header, " ")
	if algorithm != "AWS4-X509-ECDSA-SHA256" && algorithm != "AWS4-X509-RSA-SHA256" {
		return authorization{}, fmt.Errorf("algorithm %q", algorithm)
	}
	fields := map[string]string{}
	for _, field := range strings.Split(rest, ", ") {
		k, v, _ := strings.Cut(field, "=")
		fields[k] = v
	}
	serial, scope, _ := strings.Cut(fields["Credential"], "/")
	sig, err := hex.DecodeString(fields["Signature"])
	if err != nil {
		return authorization{}, err
	}
	return authorization{algorithm: algorithm, serial: serial, scope: scope, signedHeaders: fields["SignedHeaders"], signature: sig}, nil
}

// stringToSign is rebuilt from what arrived: the canonical request of the
// signed headers' values, hashed, under the algorithm, date and scope.
func stringToSign(r *http.Request, body []byte, a authorization, stamp string) string {
	var canonical strings.Builder
	for _, n := range strings.Split(a.signedHeaders, ";") {
		v := r.Header.Get(n)
		if n == "host" {
			v = r.Host
		}
		canonical.WriteString(n + ":" + v + "\n")
	}
	payload := sha256.Sum256(body)
	request := "POST\n/sessions\n\n" + canonical.String() + "\n" + a.signedHeaders + "\n" + hex.EncodeToString(payload[:])
	digest := sha256.Sum256([]byte(request))
	return a.algorithm + "\n" + stamp + "\n" + a.scope + "\n" + hex.EncodeToString(digest[:])
}

// verify checks the signature of toSign with leaf's key, under the
// algorithm that key's type calls for.
func (a authorization) verify(leaf *x509.Certificate, toSign string) error {
	h := sha256.Sum256([]byte(toSign))
	switch pub := leaf.PublicKey.(type) {
	case *ecdsa.PublicKey:
		if a.algorithm != "AWS4-X509-ECDSA-SHA256" || !ecdsa.VerifyASN1(pub, h[:], a.signature) {
			return fmt.Errorf("bad %s signature", a.algorithm)
		}
	case *rsa.PublicKey:
		if a.algorithm != "AWS4-X509-RSA-SHA256" || rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], a.signature) != nil {
			return fmt.Errorf("bad %s signature", a.algorithm)
		}
	default:
		return fmt.Errorf("leaf key %T", pub)
	}
	return nil
}

// Calls is how many requests arrived; Err is what verify found wrong with
// the last one (nil: it was what AWS takes); Body is its JSON body.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Err is what was wrong with the last request, nil if nothing was.
func (f *Fake) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastErr
}

// Body is the last request's JSON body.
func (f *Fake) Body() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastBody
}

// Chain is the intermediates the last request carried in
// X-Amz-X509-Chain.
func (f *Fake) Chain() []*x509.Certificate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chain
}

// Answer replaces the session answer of every request that verifies; nil
// restores it.
func (f *Fake) Answer(answer func(w http.ResponseWriter)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = answer
}

// Block holds every answer until the channel is closed; nil stops holding.
func (f *Fake) Block(c chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.block = c
}

// Test ARNs are composed from their parts here, and only here: AWS's
// documentation account and made-up ids, never a real account or resource.
const (
	// Account is AWS's documentation account.
	Account = "111122223333"
	// AnchorID and ProfileID are made-up ids.
	AnchorID  = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"
	ProfileID = "01234567-89ab-cdef-0123-456789abcdef"
)

// PartitionARN joins an ARN's parts.
func PartitionARN(partition, service, region, account, resource string) string {
	return "arn:" + partition + ":" + service + ":" + region + ":" + account + ":" + resource
}

// ARN is PartitionARN in the aws partition.
func ARN(service, region, account, resource string) string {
	return PartitionARN("aws", service, region, account, resource)
}

// Anchor is an example trust anchor of region, in Account.
func Anchor(region string) string {
	return ARN("rolesanywhere", region, Account, "trust-anchor/"+AnchorID)
}

// Profile is an example profile of region, in Account.
func Profile(region string) string {
	return ARN("rolesanywhere", region, Account, "profile/"+ProfileID)
}

// Role is an example role named name, in Account.
func Role(name string) string {
	return ARN("iam", "", Account, "role/"+name)
}
