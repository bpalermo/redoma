package rolesanywhere

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bpalermo/redoma/xds/internal/rolesanywhere/rolesanywheretest"
)

// The test ARNs, composed from their parts by rolesanywheretest.
var (
	anchor  = rolesanywheretest.Anchor("us-east-1")
	profile = rolesanywheretest.Profile("us-east-1")
	role    = rolesanywheretest.Role("example-agent")
	arn     = rolesanywheretest.ARN
	inPart  = rolesanywheretest.PartitionARN
	account = rolesanywheretest.Account
)

func validProfile() Profile {
	return Profile{TrustAnchorARN: anchor, ProfileARN: profile, RoleARN: role}
}

func TestValidate(t *testing.T) {
	good := []Profile{
		validProfile(),
		{TrustAnchorARN: anchor, ProfileARN: profile, RoleARN: rolesanywheretest.Role("path/to/name+=,.@-_"), Duration: 15 * time.Minute},
		{TrustAnchorARN: anchor, ProfileARN: profile, RoleARN: role, Duration: 12 * time.Hour},
		{
			TrustAnchorARN: inPart("aws-us-gov", "rolesanywhere", "us-gov-west-1", account, "trust-anchor/"+rolesanywheretest.AnchorID),
			ProfileARN:     inPart("aws-us-gov", "rolesanywhere", "us-gov-west-1", account, "profile/"+rolesanywheretest.ProfileID),
			RoleARN:        inPart("aws-us-gov", "iam", "", account, "role/r"),
		},
	}
	for _, p := range good {
		if err := p.Validate(); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
	bad := map[string]func(*Profile){
		"anchor is a profile":        func(p *Profile) { p.TrustAnchorARN = profile },
		"profile is an anchor":       func(p *Profile) { p.ProfileARN = anchor },
		"anchor with trailing space": func(p *Profile) { p.TrustAnchorARN += " " },
		"anchor id upper case": func(p *Profile) {
			p.TrustAnchorARN = strings.ToUpper(anchor[:len(anchor)-36]) + strings.ToUpper(anchor[len(anchor)-36:])
		},
		"anchor id not a uuid":  func(p *Profile) { p.TrustAnchorARN = strings.TrimSuffix(anchor, "f0") },
		"anchor with no region": func(p *Profile) { p.TrustAnchorARN = strings.Replace(anchor, "us-east-1", "", 1) },
		"anchor short account":  func(p *Profile) { p.TrustAnchorARN = strings.Replace(anchor, "111122223333", "11112222333", 1) },
		"unknown partition": func(p *Profile) {
			p.TrustAnchorARN = inPart("aws-xx", "rolesanywhere", "us-east-1", account, "trust-anchor/"+rolesanywheretest.AnchorID)
		},
		"profile in another region":   func(p *Profile) { p.ProfileARN = strings.Replace(profile, "us-east-1", "us-east-2", 1) },
		"profile in another account":  func(p *Profile) { p.ProfileARN = strings.Replace(profile, "111122223333", "444455556666", 1) },
		"role in another account":     func(p *Profile) { p.RoleARN = strings.Replace(role, "111122223333", "444455556666", 1) },
		"role in another partition":   func(p *Profile) { p.RoleARN = inPart("aws-cn", "iam", "", account, "role/example-agent") },
		"role is a user":              func(p *Profile) { p.RoleARN = strings.Replace(role, ":role/", ":user/", 1) },
		"role with a region":          func(p *Profile) { p.RoleARN = strings.Replace(role, "iam::", "iam:us-east-1:", 1) },
		"role with no name":           func(p *Profile) { p.RoleARN = arn("iam", "", account, "role/") },
		"role name too long":          func(p *Profile) { p.RoleARN = rolesanywheretest.Role(strings.Repeat("a", 65)) },
		"role with a wildcard":        func(p *Profile) { p.RoleARN = rolesanywheretest.Role("*") },
		"duration below the minimum":  func(p *Profile) { p.Duration = 15*time.Minute - time.Second },
		"duration above the maximum":  func(p *Profile) { p.Duration = 12*time.Hour + time.Second },
		"duration not whole seconds":  func(p *Profile) { p.Duration = time.Hour + time.Millisecond },
		"duration negative":           func(p *Profile) { p.Duration = -time.Hour },
		"role arn with a newline":     func(p *Profile) { p.RoleARN += "\n" },
		"profile arn with a newline":  func(p *Profile) { p.ProfileARN += "\n" },
		"anchor with a newline first": func(p *Profile) { p.TrustAnchorARN = "\n" + anchor },
	}
	for name, mutate := range bad {
		p := validProfile()
		mutate(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: %+v is valid", name, p)
		}
	}
}

func TestRegionAndHost(t *testing.T) {
	p := validProfile()
	if p.Region() != "us-east-1" || p.Host() != "rolesanywhere.us-east-1.amazonaws.com" {
		t.Errorf("Region %q, Host %q", p.Region(), p.Host())
	}
	if h := Host("aws-cn", "cn-north-1"); h != "rolesanywhere.cn-north-1.amazonaws.com.cn" {
		t.Errorf("China: %q", h)
	}
	if (Profile{TrustAnchorARN: "nonsense"}).Region() != "" {
		t.Error("a region from a bad ARN")
	}
}

func certificate(t *testing.T, rsaLeaf bool) (Certificate, *x509.Certificate, *x509.Certificate) {
	t.Helper()
	chain, key, leaf, intermediate := rolesanywheretest.SVID(t, 0x1234567890, rsaLeaf)
	cert, err := ParseCertificate(chain, key)
	if err != nil {
		t.Fatal(err)
	}
	return cert, leaf, intermediate
}

// TestCreateSession: the request is what Roles Anywhere checks (the fake
// rebuilds the signature and verifies it with the certificate's key), with
// the intermediate in X-Amz-X509-Chain, and the session comes back.
func TestCreateSession(t *testing.T) {
	for _, rsaLeaf := range []bool{false, true} {
		f := rolesanywheretest.NewFake(t, "us-east-1")
		cert, _, intermediate := certificate(t, rsaLeaf)
		c := &Client{HTTP: f.Client()}
		p := validProfile()
		p.Duration = 2 * time.Hour
		got, err := c.CreateSession(context.Background(), p, cert)
		if err != nil {
			t.Fatalf("rsa %v: %v (the fake: %v)", rsaLeaf, err, f.Err())
		}
		if f.Err() != nil {
			t.Fatalf("rsa %v: the fake accepted with %v", rsaLeaf, f.Err())
		}
		if got.AccessKeyID != "ASIAEXAMPLE" || got.SecretAccessKey != "secret" || got.SessionToken != "token" {
			t.Errorf("rsa %v: %+v", rsaLeaf, got)
		}
		if d := time.Until(got.Expiration); d < 2*time.Hour-time.Minute || d > 2*time.Hour {
			t.Errorf("rsa %v: expires in %s, want 2h", rsaLeaf, d)
		}
		if got.Expiration.Location() != time.UTC {
			t.Errorf("rsa %v: expiration not UTC", rsaLeaf)
		}
		body := f.Body()
		if body["durationSeconds"] != float64(7200) || body["trustAnchorArn"] != anchor || body["profileArn"] != profile || body["roleArn"] != role {
			t.Errorf("rsa %v: body %v", rsaLeaf, body)
		}
		chain := f.Chain()
		if intermediate == nil {
			if len(chain) != 0 {
				t.Errorf("rsa %v: X-Amz-X509-Chain has %d certificates, want none", rsaLeaf, len(chain))
			}
		} else if len(chain) != 1 || !chain[0].Equal(intermediate) {
			t.Errorf("rsa %v: X-Amz-X509-Chain is not the intermediate", rsaLeaf)
		}
	}
}

func TestCreateSessionDefaultDuration(t *testing.T) {
	f := rolesanywheretest.NewFake(t, "us-east-1")
	cert, _, _ := certificate(t, false)
	if _, err := (&Client{HTTP: f.Client()}).CreateSession(context.Background(), validProfile(), cert); err != nil {
		t.Fatal(err)
	}
	if got := f.Body()["durationSeconds"]; got != float64(3600) {
		t.Errorf("durationSeconds %v, want 3600", got)
	}
}

// TestCreateSessionClock: the request is signed at the client's clock, which
// the fake holds within five minutes of its own.
func TestCreateSessionClock(t *testing.T) {
	f := rolesanywheretest.NewFake(t, "us-east-1")
	cert, _, _ := certificate(t, false)
	c := &Client{HTTP: f.Client(), Now: func() time.Time { return time.Now().Add(-time.Hour) }}
	if _, err := c.CreateSession(context.Background(), validProfile(), cert); err == nil {
		t.Fatal("a request signed an hour ago was accepted")
	}
	if f.Err() == nil || !strings.Contains(f.Err().Error(), "X-Amz-Date") {
		t.Errorf("the fake: %v, want an X-Amz-Date error", f.Err())
	}
}

func TestCreateSessionInvalidProfile(t *testing.T) {
	f := rolesanywheretest.NewFake(t, "us-east-1")
	cert, _, _ := certificate(t, false)
	p := validProfile()
	p.RoleARN = rolesanywheretest.Role("*")
	if _, err := (&Client{HTTP: f.Client()}).CreateSession(context.Background(), p, cert); err == nil {
		t.Fatal("an invalid profile was sent")
	}
	if f.Calls() != 0 {
		t.Errorf("%d calls, want none", f.Calls())
	}
}

func TestCreateSessionAnswers(t *testing.T) {
	cases := map[string]struct {
		status  int
		body    string
		refused bool
		typed   bool
	}{
		"access denied":         {http.StatusForbidden, `{"message":"AccessDeniedException"}`, true, true},
		"validation":            {http.StatusBadRequest, `{"message":"ValidationException"}`, true, true},
		"throttled":             {http.StatusTooManyRequests, `{"message":"Rate exceeded"}`, false, true},
		"server error":          {http.StatusInternalServerError, `{}`, false, true},
		"not json":              {http.StatusCreated, `<html>`, false, false},
		"no credential set":     {http.StatusCreated, `{"credentialSet":[]}`, false, false},
		"two credential sets":   {http.StatusCreated, `{"credentialSet":[{"credentials":{"accessKeyId":"a","secretAccessKey":"b","sessionToken":"c","expiration":"2030-01-01T00:00:00Z"}},{}]}`, false, false},
		"no token":              {http.StatusCreated, `{"credentialSet":[{"credentials":{"accessKeyId":"a","secretAccessKey":"b","expiration":"2030-01-01T00:00:00Z"}}]}`, false, false},
		"no expiration":         {http.StatusCreated, `{"credentialSet":[{"credentials":{"accessKeyId":"a","secretAccessKey":"b","sessionToken":"c"}}]}`, false, false},
		"secret in a bad shape": {http.StatusCreated, `{"credentialSet":[{"credentials":{"accessKeyId":"a","secretAccessKey":{"x":"SECRETVALUE"}}}]}`, false, false},
	}
	cert, _, _ := certificate(t, false)
	for name, tc := range cases {
		f := rolesanywheretest.NewFake(t, "us-east-1")
		f.Answer(func(w http.ResponseWriter) {
			w.WriteHeader(tc.status)
			w.Write([]byte(tc.body))
		})
		_, err := (&Client{HTTP: f.Client()}).CreateSession(context.Background(), validProfile(), cert)
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		var e *Error
		if errors.As(err, &e) != tc.typed || (tc.typed && (e.Status != tc.status || e.Refused() != tc.refused)) {
			t.Errorf("%s: %v (typed %v, want %v; refused %v)", name, err, errors.As(err, &e), tc.typed, tc.refused)
		}
		if strings.Contains(err.Error(), "SECRETVALUE") {
			t.Errorf("%s: the error quotes the answer's secret: %v", name, err)
		}
	}
}

func TestParseCertificate(t *testing.T) {
	chain, key, _, _ := rolesanywheretest.SVID(t, 7, false)
	_, otherKey, _, _ := rolesanywheretest.SVID(t, 8, false)
	if _, err := ParseCertificate(chain, key); err != nil {
		t.Fatal(err)
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sec1, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][2][]byte{
		"another certificate's key": {chain, otherKey},
		"no certificate":            {nil, key},
		"no key":                    {chain, nil},
		"a SEC 1 key":               {chain, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1})},
		"a broken key":              {chain, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("SECRETVALUE")})},
		"a broken certificate":      {pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")}), key},
	}
	for name, c := range cases {
		_, err := ParseCertificate(c[0], c[1])
		if err == nil {
			t.Errorf("%s: parsed", name)
		} else if strings.Contains(err.Error(), "SECRETVALUE") {
			t.Errorf("%s: the error quotes the key: %v", name, err)
		}
	}
}

// TestValidateRegionOfPartition: a region belongs to one partition, so an
// ARN whose region is another partition's is refused when the settings are
// read, not retried against an endpoint that never answers.
func TestValidateRegionOfPartition(t *testing.T) {
	in := func(partition, region string) Profile {
		return Profile{
			TrustAnchorARN: inPart(partition, "rolesanywhere", region, account, "trust-anchor/"+rolesanywheretest.AnchorID),
			ProfileARN:     inPart(partition, "rolesanywhere", region, account, "profile/"+rolesanywheretest.ProfileID),
			RoleARN:        inPart(partition, "iam", "", account, "role/r"),
		}
	}
	for _, ok := range [][2]string{
		{"aws", "us-east-1"},
		{"aws", "eu-west-3"},
		{"aws", "ap-southeast-2"},
		{"aws-cn", "cn-north-1"},
		{"aws-cn", "cn-northwest-1"},
		{"aws-us-gov", "us-gov-west-1"},
		{"aws-us-gov", "us-gov-east-1"},
	} {
		if err := in(ok[0], ok[1]).Validate(); err != nil {
			t.Errorf("%s in %s: %v", ok[1], ok[0], err)
		}
	}
	for _, bad := range [][2]string{
		{"aws", "cn-north-1"},
		{"aws", "us-gov-west-1"},
		{"aws-cn", "us-east-1"},
		{"aws-cn", "us-gov-west-1"},
		{"aws-us-gov", "us-east-1"},
		{"aws-us-gov", "cn-north-1"},
	} {
		if err := in(bad[0], bad[1]).Validate(); err == nil {
			t.Errorf("%s accepted in %s", bad[1], bad[0])
		}
	}
}

// TestCreateSessionNeedsAClient: no HTTP client is an error, never a
// fallback to the default one (a proxy from the environment, a direct dial).
func TestCreateSessionNeedsAClient(t *testing.T) {
	cert, _, _ := certificate(t, false)
	if _, err := (&Client{}).CreateSession(context.Background(), validProfile(), cert); !errors.Is(err, ErrNoHTTPClient) {
		t.Fatalf("no client: %v, want ErrNoHTTPClient", err)
	}
	// Checked first, before anything is validated or signed.
	if _, err := (&Client{}).CreateSession(context.Background(), Profile{}, Certificate{}); !errors.Is(err, ErrNoHTTPClient) {
		t.Fatalf("no client, nothing else either: %v, want ErrNoHTTPClient", err)
	}
}
