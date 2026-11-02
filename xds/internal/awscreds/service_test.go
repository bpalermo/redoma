package awscreds

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bpalermo/redoma/common/sockettest"
	"github.com/bpalermo/redoma/xds/internal/rolesanywhere"
	"github.com/bpalermo/redoma/xds/internal/rolesanywhere/rolesanywheretest"
)

// The test ARNs, composed from their parts by rolesanywheretest.
var (
	anchor  = rolesanywheretest.Anchor("us-east-1")
	profile = rolesanywheretest.Profile("us-east-1")
	role    = rolesanywheretest.Role("example-agent")
	other   = rolesanywheretest.Role("another")
)

func testProfile(r string) *rolesanywhere.Profile {
	return &rolesanywhere.Profile{TrustAnchorARN: anchor, ProfileARN: profile, RoleARN: r}
}

// sessions is a Sessions that answers what next says, counting the calls
// and the roles asked for; hold, when set, holds every call until closed.
type sessions struct {
	mu    sync.Mutex
	calls int
	roles []string
	next  func() (rolesanywhere.Credentials, error)
	hold  chan struct{}
	// called receives a value at the start of each call.
	called chan struct{}
}

func newSessions(life time.Duration) *sessions {
	return &sessions{
		called: make(chan struct{}, 64),
		next: func() (rolesanywhere.Credentials, error) {
			return rolesanywhere.Credentials{AccessKeyID: "ASIAEXAMPLE", SecretAccessKey: "secret", SessionToken: "token", Expiration: time.Now().Add(life)}, nil
		},
	}
}

func (s *sessions) CreateSession(ctx context.Context, p rolesanywhere.Profile, _ rolesanywhere.Certificate) (rolesanywhere.Credentials, error) {
	s.mu.Lock()
	s.calls++
	s.roles = append(s.roles, p.RoleARN)
	hold, next := s.hold, s.next
	s.mu.Unlock()
	s.called <- struct{}{}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return rolesanywhere.Credentials{}, ctx.Err()
		}
	}
	return next()
}

func (s *sessions) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *sessions) setHold(c chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold = c
}

func (s *sessions) setNext(next func() (rolesanywhere.Credentials, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next = next
}

func newService(t *testing.T, sess Sessions) *Service {
	t.Helper()
	s := &Service{Dir: sockettest.Dir(t), Sessions: sess, Log: slog.New(slog.DiscardHandler)}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func svid(t *testing.T) (chain, key []byte) {
	chain, key, _, _ = rolesanywheretest.SVID(t, 42, false)
	return chain, key
}

// get sends a request to agent id's handler, the way Envoy forwards it.
func get(t *testing.T, s *Service, id, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	s.mu.Lock()
	st := s.agents[id]
	s.mu.Unlock()
	if st == nil {
		t.Fatalf("no agent %s", id)
	}
	w := httptest.NewRecorder()
	s.handler(st).ServeHTTP(w, httptest.NewRequest(method, "http://127.0.0.1"+path, nil))
	return w
}

// waitCalls waits until n calls have started.
func waitCalls(t *testing.T, sess *sessions, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for sess.count() < n {
		select {
		case <-sess.called:
		case <-deadline:
			t.Fatalf("%d calls, want %d", sess.count(), n)
		}
	}
}

// waitFetched waits until agent id has no request in flight.
func waitFetched(t *testing.T, s *Service, id string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		s.mu.Lock()
		idle := s.agents[id].fetch == nil
		s.mu.Unlock()
		if idle {
			return
		}
	}
	t.Fatal("the session request did not return")
}

func wantReason(t *testing.T, w *httptest.ResponseRecorder, status int, reason string) {
	t.Helper()
	if w.Code != status || w.Header().Get(HeaderReason) != reason {
		t.Errorf("%d %s=%q (%s), want %d %q", w.Code, HeaderReason, w.Header().Get(HeaderReason), w.Body.String(), status, reason)
	}
}

// TestPrewarmThenServeFromMemory: the SVID's arrival asks for a session at
// once, and requests are answered from memory with the SDK's four fields.
func TestPrewarmThenServeFromMemory(t *testing.T) {
	// A zone that is not UTC: the Expiration served must still be.
	local := time.Local
	time.Local = time.FixedZone("UTC+3", 3*3600)
	t.Cleanup(func() { time.Local = local })
	sess := newSessions(time.Hour)
	s := newService(t, sess)
	chain, key := svid(t)
	s.Set("a", testProfile(role), nil, chain, key)
	waitCalls(t, sess, 1)
	waitFetched(t, s, "a")
	for range 3 {
		w := get(t, s, "a", http.MethodGet, CredentialsPath)
		if w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		var got map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		exp, err := time.Parse(time.RFC3339, got["Expiration"])
		if err != nil || exp.Location() != time.UTC || got["Expiration"][len(got["Expiration"])-1] != 'Z' {
			t.Errorf("Expiration %q is not RFC 3339 UTC", got["Expiration"])
		}
		if got["AccessKeyId"] != "ASIAEXAMPLE" || got["SecretAccessKey"] != "secret" || got["Token"] != "token" || len(got) != 4 {
			t.Errorf("body %v", got)
		}
		if w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("headers %v", w.Header())
		}
	}
	if sess.count() != 1 {
		t.Errorf("%d session requests, want the prewarm's only", sess.count())
	}
}

// TestNoSVIDNoPrewarm: without an SVID nothing is asked for and a request
// is a 503 no_svid; the SVID's arrival is the prewarm.
func TestNoSVIDNoPrewarm(t *testing.T) {
	sess := newSessions(time.Hour)
	s := newService(t, sess)
	s.Set("a", testProfile(role), nil, nil, nil)
	wantReason(t, get(t, s, "a", http.MethodGet, CredentialsPath), http.StatusServiceUnavailable, ReasonNoSVID)
	if sess.count() != 0 {
		t.Fatalf("%d session requests without an SVID", sess.count())
	}
	chain, key := svid(t)
	s.Set("a", testProfile(role), nil, chain, key)
	waitCalls(t, sess, 1)
	waitFetched(t, s, "a")
	if w := get(t, s, "a", http.MethodGet, CredentialsPath); w.Code != http.StatusOK {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
}

// TestSetAgainDoesNotAsk: the same profile and SVID set again (the control
// plane's rescans) ask for nothing more.
func TestSetAgainDoesNotAsk(t *testing.T) {
	sess := newSessions(time.Hour)
	s := newService(t, sess)
	chain, key := svid(t)
	for range 5 {
		s.Set("a", testProfile(role), nil, chain, key)
	}
	waitCalls(t, sess, 1)
	waitFetched(t, s, "a")
	for range 5 {
		s.Set("a", testProfile(role), nil, chain, key)
	}
	time.Sleep(50 * time.Millisecond)
	if sess.count() != 1 {
		t.Errorf("%d session requests, want 1", sess.count())
	}
}

// TestRefreshBeforeExpiry: a session is asked for again RefreshBefore its
// end, while the one in memory is still served.
func TestRefreshBeforeExpiry(t *testing.T) {
	sess := newSessions(DefaultRefreshBefore + 200*time.Millisecond)
	s := newService(t, sess)
	chain, key := svid(t)
	s.Set("a", testProfile(role), nil, chain, key)
	waitCalls(t, sess, 1)
	waitFetched(t, s, "a")
	hold := make(chan struct{})
	sess.setHold(hold)
	waitCalls(t, sess, 2)
	// The refresh is in flight; the session in memory has ~10m left.
	if w := get(t, s, "a", http.MethodGet, CredentialsPath); w.Code != http.StatusOK {
		t.Errorf("during the refresh: %d %s", w.Code, w.Body.String())
	}
	close(hold)
	waitFetched(t, s, "a")
}

// TestRequestJoinsTheRequestInFlight: a request with nothing in memory waits
// for the session request in flight (the prewarm's), and starts no other.
func TestRequestJoinsTheRequestInFlight(t *testing.T) {
	sess := newSessions(time.Hour)
	hold := make(chan struct{})
	sess.setHold(hold)
	s := newService(t, sess)
	chain, key := svid(t)
	s.Set("a", testProfile(role), nil, chain, key)
	waitCalls(t, sess, 1)
	time.AfterFunc(100*time.Millisecond, func() { close(hold) })
	if w := get(t, s, "a", http.MethodGet, CredentialsPath); w.Code != http.StatusOK {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
	if sess.count() != 1 {
		t.Errorf("%d session requests, want 1", sess.count())
	}
}

// TestRequestWait: a request waits RequestWait at most, then answers 503
// session_pending; the session, when it comes, serves the next.
func TestRequestWait(t *testing.T) {
	sess := newSessions(time.Hour)
	hold := make(chan struct{})
	sess.setHold(hold)
	s := newService(t, sess)
	s.RequestWait = 100 * time.Millisecond
	chain, key := svid(t)
	s.Set("a", testProfile(role), nil, chain, key)
	start := time.Now()
	wantReason(t, get(t, s, "a", http.MethodGet, CredentialsPath), http.StatusServiceUnavailable, ReasonSessionPending)
	if d := time.Since(start); d < 100*time.Millisecond || d > 2*time.Second {
		t.Errorf("answered after %s, want RequestWait", d)
	}
	close(hold)
	waitFetched(t, s, "a")
	if w := get(t, s, "a", http.MethodGet, CredentialsPath); w.Code != http.StatusOK {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
}

func TestNotFound(t *testing.T) {
	s := newService(t, newSessions(time.Hour))
	chain, key := svid(t)
	s.Set("a", testProfile(role), nil, chain, key)
	for _, c := range [][2]string{
		{http.MethodPost, CredentialsPath},
		{http.MethodPut, CredentialsPath},
		{http.MethodGet, "/v1/credentials/"},
		{http.MethodGet, "/"},
		{http.MethodGet, "/latest/meta-data/iam/security-credentials/"},
	} {
		if w := get(t, s, "a", c[0], c[1]); w.Code != http.StatusNotFound {
			t.Errorf("%s %s: %d, want 404", c[0], c[1], w.Code)
		}
	}
}

// TestPerAgentLimit: past PerAgentLimit requests in flight an agent's next
// is a 429 at once, and another agent is not affected.
func TestPerAgentLimit(t *testing.T) {
	sess := newSessions(time.Hour)
	hold := make(chan struct{})
	sess.setHold(hold)
	s := newService(t, sess)
	s.PerAgentLimit, s.RequestWait = 2, 5*time.Second
	chain, key := svid(t)
	// Set before PerAgentLimit is read: open sizes the slots from it.
	s.Set("a", testProfile(role), nil, chain, key)
	s.Set("b", testProfile(role), nil, nil, nil)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { get(t, s, "a", http.MethodGet, CredentialsPath) })
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		s.mu.Lock()
		n := len(s.agents["a"].slots)
		s.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d requests in flight, want 2", n)
		}
	}
	w := get(t, s, "a", http.MethodGet, CredentialsPath)
	wantReason(t, w, http.StatusTooManyRequests, ReasonAgentBusy)
	if w.Header().Get("Retry-After") != "1" {
		t.Errorf("Retry-After %q", w.Header().Get("Retry-After"))
	}
	wantReason(t, get(t, s, "b", http.MethodGet, CredentialsPath), http.StatusServiceUnavailable, ReasonNoSVID)
	close(hold)
	wg.Wait()
}

func TestConfigInvalid(t *testing.T) {
	sess := newSessions(time.Hour)
	s := newService(t, sess)
	chain, key := svid(t)
	s.Set("a", nil, errors.New("aws/a.conf: unknown key"), chain, key)
	wantReason(t, get(t, s, "a", http.MethodGet, CredentialsPath), http.StatusServiceUnavailable, ReasonConfigInvalid)
	if sess.count() != 0 {
		t.Errorf("%d session requests with no valid profile", sess.count())
	}
}

// TestFailures: a refusal is session_refused, anything else
// rolesanywhere_unavailable; after a failure the SDK's retries are answered
// from it, without asking again, until the scheduled retry.
func TestFailures(t *testing.T) {
	for _, tc := range []struct {
		err    error
		reason string
	}{
		{&rolesanywhere.Error{Status: http.StatusForbidden, Message: "AccessDeniedException"}, ReasonSessionRefused},
		{&rolesanywhere.Error{Status: http.StatusTooManyRequests}, ReasonUnavailable},
		{&rolesanywhere.Error{Status: http.StatusBadGateway}, ReasonUnavailable},
		{errors.New("dial unix: connection refused"), ReasonUnavailable},
	} {
		sess := newSessions(time.Hour)
		sess.setNext(func() (rolesanywhere.Credentials, error) { return rolesanywhere.Credentials{}, tc.err })
		s := newService(t, sess)
		chain, key := svid(t)
		s.Set("a", testProfile(role), nil, chain, key)
		waitCalls(t, sess, 1)
		waitFetched(t, s, "a")
		for range 3 {
			// The control plane's rescans set the same again.
			s.Set("a", testProfile(role), nil, chain, key)
			wantReason(t, get(t, s, "a", http.MethodGet, CredentialsPath), http.StatusServiceUnavailable, tc.reason)
		}
		if sess.count() != 1 {
			t.Errorf("%v: %d session requests, want 1 until the retry", tc.err, sess.count())
		}
	}
}

// TestProfileChange: a new profile drops the session of the old role and
// asks for one of the new at once.
func TestProfileChange(t *testing.T) {
	sess := newSessions(time.Hour)
	s := newService(t, sess)
	chain, key := svid(t)
	s.Set("a", testProfile(role), nil, chain, key)
	waitCalls(t, sess, 1)
	waitFetched(t, s, "a")
	hold := make(chan struct{})
	sess.setHold(hold)
	s.Set("a", testProfile(other), nil, chain, key)
	waitCalls(t, sess, 2)
	s.RequestWait = 50 * time.Millisecond
	wantReason(t, get(t, s, "a", http.MethodGet, CredentialsPath), http.StatusServiceUnavailable, ReasonSessionPending)
	close(hold)
	waitFetched(t, s, "a")
	sess.mu.Lock()
	roles := sess.roles
	sess.mu.Unlock()
	if len(roles) != 2 || roles[1] != other {
		t.Errorf("roles asked for %v", roles)
	}
}

// TestSocket: the agent's socket, 0660, serves the endpoint; Set without a
// profile removes it.
func TestSocket(t *testing.T) {
	s := newService(t, newSessions(time.Hour))
	chain, key := svid(t)
	s.Set("a", testProfile(role), nil, chain, key)
	path := s.Socket("a")
	fi, err := os.Stat(path)
	if err != nil {
		sockettest.SkipIfRefused(t, func() error { _, err := net.Listen("unix", path+".probe"); return err }())
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o660 || fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("socket mode %v", fi.Mode())
	}
	waitFetched(t, s, "a")
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
	resp, err := client.Get("http://127.0.0.1" + CredentialsPath)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
	s.Set("a", nil, nil, chain, key)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the socket is still there: %v", err)
	}
	if got := s.Agents(); len(got) != 0 {
		t.Errorf("agents %v after the opt-out", got)
	}
}

// TestRemoveStopsTheRefresh: a removed agent's scheduled refresh asks for
// nothing.
func TestRemoveStopsTheRefresh(t *testing.T) {
	sess := newSessions(DefaultRefreshBefore + 100*time.Millisecond)
	s := newService(t, sess)
	chain, key := svid(t)
	s.Set("a", testProfile(role), nil, chain, key)
	waitCalls(t, sess, 1)
	waitFetched(t, s, "a")
	s.Remove("a")
	time.Sleep(300 * time.Millisecond)
	if sess.count() != 1 {
		t.Errorf("%d session requests after the removal, want 1", sess.count())
	}
}

// TestWithRolesAnywhere: the service through the real client, against the
// fake endpoint that checks the signature and the chain.
func TestWithRolesAnywhere(t *testing.T) {
	f := rolesanywheretest.NewFake(t, "us-east-1")
	s := newService(t, &rolesanywhere.Client{HTTP: f.Client()})
	chain, key := svid(t)
	s.Set("a", testProfile(role), nil, chain, key)
	waitFetched(t, s, "a")
	if f.Err() != nil {
		t.Fatalf("the fake refused: %v", f.Err())
	}
	if w := get(t, s, "a", http.MethodGet, CredentialsPath); w.Code != http.StatusOK {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
	if len(f.Chain()) != 1 {
		t.Errorf("X-Amz-X509-Chain carried %d certificates, want the intermediate", len(f.Chain()))
	}
}
