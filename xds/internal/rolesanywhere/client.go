package rolesanywhere

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Credentials are a session's: what the AWS SDKs' container provider takes.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

// Certificate is an X.509 SVID ready to sign with: the leaf, then its
// intermediates, and the leaf's private key.
type Certificate struct {
	Chain  []*x509.Certificate
	Signer crypto.Signer
}

// ParseCertificate reads a PEM chain (the leaf first) and the leaf's PKCS #8
// private key, as identity.X509SVID holds them, and checks that the key is
// the leaf's.
func ParseCertificate(chainPEM, keyPEM []byte) (Certificate, error) {
	var chain []*x509.Certificate
	for rest := chainPEM; ; {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return Certificate{}, fmt.Errorf("the SVID's chain: %w", err)
		}
		chain = append(chain, c)
	}
	if len(chain) == 0 {
		return Certificate{}, errors.New("the SVID's chain has no certificate")
	}
	b, _ := pem.Decode(keyPEM)
	if b == nil {
		return Certificate{}, errors.New("the SVID's key is not PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(b.Bytes)
	if err != nil {
		// Never wrap the parser's error: nothing of the key reaches a log.
		return Certificate{}, errors.New("the SVID's key is not a PKCS #8 private key")
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return Certificate{}, errors.New("the SVID's key cannot sign")
	}
	switch signer.Public().(type) {
	case *ecdsa.PublicKey, *rsa.PublicKey:
	default:
		return Certificate{}, errors.New("the SVID's key is neither ECDSA nor RSA")
	}
	pub, ok := signer.Public().(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(chain[0].PublicKey) {
		return Certificate{}, errors.New("the SVID's key is not its certificate's")
	}
	return Certificate{Chain: chain, Signer: signer}, nil
}

// Error is CreateSession's answer when it is not a session.
type Error struct {
	Status int
	// Message is the start of the answer's body, AWS's reason.
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("CreateSession: %d %s: %s", e.Status, http.StatusText(e.Status), e.Message)
}

// Refused reports whether AWS refused the request for good (a 4xx other than
// throttling): the trust anchor, the profile or the role do not accept this
// certificate, and asking again does not change that.
func (e *Error) Refused() bool {
	return e.Status >= 400 && e.Status < 500 && e.Status != http.StatusTooManyRequests
}

// Client calls CreateSession.
type Client struct {
	// HTTP sends the request (Tunnels.Client); required, never a default
	// client, which would dial directly or through a proxy from the
	// environment. The request's URL is always
	// https://<Profile.Host>/sessions: how the connection gets there is the
	// client's business.
	HTTP *http.Client
	// Now is the clock the request is signed at; time.Now when nil.
	Now func() time.Time
}

// ErrNoHTTPClient is CreateSession's error for a Client without HTTP.
var ErrNoHTTPClient = errors.New("rolesanywhere: no HTTP client")

// maxAnswer bounds CreateSession's answer, a few kilobytes.
const maxAnswer = 1 << 20

// CreateSession asks for a session of p's role with cert. p must be valid
// (Profile.Validate).
func (c *Client) CreateSession(ctx context.Context, p Profile, cert Certificate) (Credentials, error) {
	if c.HTTP == nil {
		return Credentials{}, ErrNoHTTPClient
	}
	if err := p.Validate(); err != nil {
		return Credentials{}, err
	}
	body, err := json.Marshal(map[string]any{
		"durationSeconds": int(p.duration() / time.Second),
		"profileArn":      p.ProfileARN,
		"roleArn":         p.RoleARN,
		"trustAnchorArn":  p.TrustAnchorARN,
	})
	if err != nil {
		return Credentials{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+p.Host()+"/sessions", bytes.NewReader(body))
	if err != nil {
		return Credentials{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	if err := sign(req, body, p.Region(), cert, now()); err != nil {
		return Credentials{}, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Credentials{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return Credentials{}, err
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		msg := string(bytes.TrimSpace(raw))
		if len(msg) > 512 {
			msg = msg[:512]
		}
		return Credentials{}, &Error{Status: resp.StatusCode, Message: msg}
	}
	return decodeSession(raw)
}

// decodeSession is the one session of CreateSession's answer, complete.
func decodeSession(raw []byte) (Credentials, error) {
	var out struct {
		CredentialSet []struct {
			Credentials struct {
				AccessKeyID     string    `json:"accessKeyId"`
				SecretAccessKey string    `json:"secretAccessKey"`
				SessionToken    string    `json:"sessionToken"`
				Expiration      time.Time `json:"expiration"`
			} `json:"credentials"`
		} `json:"credentialSet"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		// Not the decoder's error: the answer is not echoed into a log in
		// any form.
		return Credentials{}, errors.New("CreateSession answered something that is not a session")
	}
	if len(out.CredentialSet) != 1 {
		return Credentials{}, fmt.Errorf("CreateSession answered %d credential sets, want 1", len(out.CredentialSet))
	}
	s := out.CredentialSet[0].Credentials
	if s.AccessKeyID == "" || s.SecretAccessKey == "" || s.SessionToken == "" || s.Expiration.IsZero() {
		return Credentials{}, errors.New("CreateSession answered an incomplete session")
	}
	return Credentials{AccessKeyID: s.AccessKeyID, SecretAccessKey: s.SecretAccessKey, SessionToken: s.SessionToken, Expiration: s.Expiration.UTC()}, nil
}

// sign signs req, whose body is body, with cert's key, for region's
// rolesanywhere at now.
func sign(req *http.Request, body []byte, region string, cert Certificate, now time.Time) error {
	var algorithm string
	switch cert.Signer.Public().(type) {
	case *ecdsa.PublicKey:
		algorithm = "AWS4-X509-ECDSA-SHA256"
	case *rsa.PublicKey:
		algorithm = "AWS4-X509-RSA-SHA256"
	default:
		return errors.New("the certificate's key is neither ECDSA nor RSA")
	}
	stamp := now.UTC().Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-X509", base64.StdEncoding.EncodeToString(cert.Chain[0].Raw))
	if len(cert.Chain) > 1 {
		rest := make([]string, 0, len(cert.Chain)-1)
		for _, c := range cert.Chain[1:] {
			rest = append(rest, base64.StdEncoding.EncodeToString(c.Raw))
		}
		req.Header.Set("X-Amz-X509-Chain", strings.Join(rest, ","))
	}
	payload := sha256.Sum256(body)
	signed, canonical := canonicalHeaders(req)
	request := strings.Join([]string{
		req.Method, escapePath(req.URL.Path), req.URL.RawQuery, canonical, signed, hex.EncodeToString(payload[:]),
	}, "\n")
	scope := stamp[:8] + "/" + region + "/rolesanywhere/aws4_request"
	digest := sha256.Sum256([]byte(request))
	toSign := algorithm + "\n" + stamp + "\n" + scope + "\n" + hex.EncodeToString(digest[:])
	h := sha256.Sum256([]byte(toSign))
	signature, err := cert.Signer.Sign(rand.Reader, h[:], crypto.SHA256)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", algorithm+" Credential="+cert.Chain[0].SerialNumber.String()+"/"+scope+
		", SignedHeaders="+signed+", Signature="+hex.EncodeToString(signature))
	return nil
}

// canonicalHeaders are the signed headers' names, sorted and joined by ;,
// and their canonical form, one name:value line each: host, content-type
// and every x-amz-*.
func canonicalHeaders(req *http.Request) (signed, canonical string) {
	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		if k := strings.ToLower(k); k == "content-type" || strings.HasPrefix(k, "x-amz-") {
			headers[k] = strings.TrimSpace(strings.Join(v, ","))
		}
	}
	names := slices.Sorted(maps.Keys(headers))
	var b strings.Builder
	for _, k := range names {
		b.WriteString(k + ":" + headers[k] + "\n")
	}
	return strings.Join(names, ";"), b.String()
}

// escapePath is a path URI-encoded as Signature Version 4 has it: every byte
// but the unreserved ones and /, upper-case hex.
func escapePath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_', c == '.', c == '~', c == '/':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
