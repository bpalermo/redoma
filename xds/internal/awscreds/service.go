// Package awscreds serves AWS session credentials to the agents that opt in
// (an <etc>/aws/<agent>.conf, Read), for the AWS SDKs' container credentials
// provider: GET /v1/credentials answers the four fields the SDKs take.
//
// Each agent has a unix socket of its own, <Dir>/<agent>.sock, which only
// that agent's Envoy listener forwards to (lds.AWSListener), so the socket a
// request arrives on is the agent's identity: nothing in the request is.
// The sessions come from IAM Roles Anywhere, signed with the agent's X.509
// SVID, which never leaves this process; only the session credentials reach
// the container.
//
// The SDKs give the provider about a second per attempt, so a request is
// answered from memory: a session is asked for as soon as the agent's SVID
// is there (the prewarm), asked for again RefreshBefore its expiry, and kept
// until then. A request that finds no session waits RequestWait for one in
// flight, then answers 503.
package awscreds

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/bpalermo/redoma/xds/internal/rolesanywhere"
)

// CredentialsPath is the one path served. Any other, or any method but GET,
// is a 404.
const CredentialsPath = "/v1/credentials"

// HeaderReason carries why the answer is a 503 or a 429.
const HeaderReason = "Redoma-Reason"

// The reasons.
const (
	// ReasonConfigInvalid: the agent's <etc>/aws/<agent>.conf does not read
	// or validate. Retrying does not help until it is fixed.
	ReasonConfigInvalid = "config_invalid"
	// ReasonNoSVID: SPIRE has not issued the agent's SVID yet, or it cannot
	// sign.
	ReasonNoSVID = "no_svid"
	// ReasonSessionPending: a session is being asked for and did not come
	// within the request's wait; the SDK's retry finds it.
	ReasonSessionPending = "session_pending"
	// ReasonSessionRefused: Roles Anywhere refused the SVID for this trust
	// anchor, profile and role (a 4xx other than throttling).
	ReasonSessionRefused = "session_refused"
	// ReasonUnavailable: Roles Anywhere was unreachable, throttled, failed
	// or did not answer in time.
	ReasonUnavailable = "rolesanywhere_unavailable"
	// ReasonAgentBusy: the agent has PerAgentLimit requests in flight
	// already (a 429).
	ReasonAgentBusy = "agent_busy"
)

// messages explain the reasons, in the error body.
var messages = map[string]string{
	ReasonConfigInvalid:  "the agent's AWS settings do not read or validate",
	ReasonNoSVID:         "the agent has no SVID yet",
	ReasonSessionPending: "a session is being asked for; retry",
	ReasonSessionRefused: "IAM Roles Anywhere refused the agent's SVID for this trust anchor, profile and role",
	ReasonUnavailable:    "IAM Roles Anywhere did not answer",
	ReasonAgentBusy:      "too many requests in flight for this agent",
}

// Defaults of Service.
const (
	DefaultPerAgentLimit = 8
	DefaultRefreshBefore = 10 * time.Minute
	DefaultRequestWait   = 900 * time.Millisecond
	DefaultCallTimeout   = 10 * time.Second
	// minRemaining is the least a session served from memory has left: one
	// closer to its end is not handed out, so an SDK never gets a session
	// it would have to replace at once.
	minRemaining = time.Minute
	// A failed session request is retried after retryMin, doubling up to
	// retryMax. The first retry is prompt: the usual first failure is a
	// tunnel Envoy has not bound yet, which it does within moments.
	retryMin = time.Second
	retryMax = time.Minute
	// DefaultRefreshMin is the least a refresh is scheduled after a session
	// came: one shorter than RefreshBefore (a short session_duration, a
	// clock ahead) must not be asked for again in a loop.
	DefaultRefreshMin = 5 * time.Second
	// DefaultListenRetry is how long after a socket failed to open it is
	// tried again.
	DefaultListenRetry = 5 * time.Second
)

// Sessions asks for sessions (rolesanywhere.Client).
type Sessions interface {
	CreateSession(ctx context.Context, p rolesanywhere.Profile, cert rolesanywhere.Certificate) (rolesanywhere.Credentials, error)
}

// Service holds every opted-in agent's socket and session. Set it up, then
// Start it; Set and Remove follow the agents; Close ends it.
type Service struct {
	// Dir holds the sockets (/run/redoma/xds/aws).
	Dir string
	// Sessions asks Roles Anywhere.
	Sessions Sessions
	Log      *slog.Logger
	// Zero values are the defaults above; Now is time.Now when nil.
	PerAgentLimit int
	RefreshBefore time.Duration
	RequestWait   time.Duration
	CallTimeout   time.Duration
	ListenRetry   time.Duration
	RefreshMin    time.Duration
	Now           func() time.Time

	mu     sync.Mutex
	agents map[string]*state
	ctx    context.Context
	cancel context.CancelFunc
}

// state is one agent's.
type state struct {
	id     string
	ctx    context.Context
	cancel context.CancelFunc
	srv    *http.Server
	slots  chan struct{}

	profile    *rolesanywhere.Profile
	profileErr error
	chainPEM   []byte
	keyPEM     []byte
	cert       *rolesanywhere.Certificate
	certErr    error
	// gen counts profile changes: a session asked for under an earlier
	// profile is another role's, and is dropped.
	gen      uint64
	creds    *rolesanywhere.Credentials
	fetch    *fetch
	timer    *time.Timer
	failures int
	// lastErr is the last request's error while a retry is scheduled.
	lastErr error
}

// fetch is a session request in flight; done is closed when it returns.
type fetch struct {
	done chan struct{}
	err  error
}

// Start makes Dir (0750: Envoy reaches it through its group) and readies
// the service.
func (s *Service) Start() error {
	if err := os.MkdirAll(s.Dir, 0o750); err != nil {
		return err
	}
	if err := os.Chmod(s.Dir, 0o750); err != nil {
		return err
	}
	if s.Log == nil {
		s.Log = slog.Default()
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.PerAgentLimit <= 0 {
		s.PerAgentLimit = DefaultPerAgentLimit
	}
	if s.RefreshBefore <= 0 {
		s.RefreshBefore = DefaultRefreshBefore
	}
	if s.RequestWait <= 0 {
		s.RequestWait = DefaultRequestWait
	}
	if s.CallTimeout <= 0 {
		s.CallTimeout = DefaultCallTimeout
	}
	if s.ListenRetry <= 0 {
		s.ListenRetry = DefaultListenRetry
	}
	if s.RefreshMin <= 0 {
		s.RefreshMin = DefaultRefreshMin
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agents = map[string]*state{}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return nil
}

// Socket is agent id's socket.
func (s *Service) Socket(id string) string {
	return filepath.Join(s.Dir, id+".sock")
}

// Set records what agent id has: its profile (nil with a nil err: the agent
// has not opted in, and is removed), or the error that its file gave, and
// its SVID, PEM chain and PKCS #8 key (nil while SPIRE has not issued it).
// The first Set of an agent opens its socket. A new SVID, or a new profile,
// with no session or one that is due, asks for a session at once: the
// prewarm. Set never blocks on Roles Anywhere.
func (s *Service) Set(id string, profile *rolesanywhere.Profile, profileErr error, chainPEM, keyPEM []byte) {
	if profile == nil && profileErr == nil {
		s.Remove(id)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agents == nil {
		return
	}
	st, ok := s.agents[id]
	if !ok {
		st = s.open(id)
	}
	changed := s.setProfile(st, profile, profileErr)
	changed = s.setSVID(st, chainPEM, keyPEM) || changed || !ok
	if changed && s.due(st) {
		s.startFetch(st)
	}
}

// setProfile records the agent's profile, or its file's error, and reports
// whether it changed. A new one drops the session of the old. Called with
// s.mu held.
func (s *Service) setProfile(st *state, profile *rolesanywhere.Profile, profileErr error) bool {
	if sameProfile(st.profile, profile) && errText(st.profileErr) == errText(profileErr) {
		return false
	}
	st.profile, st.profileErr = profile, profileErr
	st.gen++
	st.creds = nil
	st.failures, st.lastErr = 0, nil
	st.stopTimer()
	if profileErr != nil {
		s.Log.Error("AWS settings unusable; the agent's credential requests get a 503", "agent", st.id, "err", profileErr)
	}
	return true
}

// setSVID records the agent's SVID and reports whether it changed. Called
// with s.mu held.
func (s *Service) setSVID(st *state, chainPEM, keyPEM []byte) bool {
	if bytes.Equal(chainPEM, st.chainPEM) && bytes.Equal(keyPEM, st.keyPEM) {
		return false
	}
	st.chainPEM, st.keyPEM = chainPEM, keyPEM
	st.cert, st.certErr = nil, nil
	if chainPEM == nil {
		return true
	}
	c, err := rolesanywhere.ParseCertificate(chainPEM, keyPEM)
	if err != nil {
		st.certErr = err
		s.Log.Error("the agent's SVID cannot sign a session request", "agent", st.id, "err", err)
		return true
	}
	st.cert = &c
	return true
}

// Remove closes agent id's socket and forgets its session.
func (s *Service) Remove(id string) {
	s.mu.Lock()
	st, ok := s.agents[id]
	delete(s.agents, id)
	s.mu.Unlock()
	if ok {
		s.close(st)
	}
}

// Close removes every agent.
func (s *Service) Close() {
	s.mu.Lock()
	agents := s.agents
	s.agents = nil
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(agents)) {
		s.close(agents[id])
	}
}

// Agents are the agents with a socket, sorted.
func (s *Service) Agents() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.agents))
}

// open makes agent id's state and opens its socket. Called with s.mu held.
func (s *Service) open(id string) *state {
	ctx, cancel := context.WithCancel(s.ctx)
	st := &state{id: id, ctx: ctx, cancel: cancel, slots: make(chan struct{}, s.PerAgentLimit)}
	s.agents[id] = st
	s.serve(st)
	return st
}

// serve opens the agent's socket and serves it. A socket that cannot be
// opened is logged and tried again after ListenRetry, for as long as the
// agent is there; meanwhile Envoy's requests fail and the agent's SDK
// retries. Called with s.mu held.
func (s *Service) serve(st *state) {
	id, ctx := st.id, st.ctx
	l, err := listen(s.Socket(id))
	if err != nil {
		s.Log.Error("the agent's AWS credentials socket; trying again", "agent", id, "err", err, "retry_in", s.ListenRetry)
		time.AfterFunc(s.ListenRetry, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.agents[id] == st && ctx.Err() == nil && st.srv == nil {
				s.serve(st)
			}
		})
		return
	}
	st.srv = &http.Server{
		Handler:           s.handler(st),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		if err := st.srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.Log.Error("the agent's AWS credentials socket stopped", "agent", id, "err", err)
		}
	}()
	s.Log.Info("AWS credentials socket", "agent", id, "socket", s.Socket(id))
}

func (s *Service) close(st *state) {
	st.cancel()
	s.mu.Lock()
	st.stopTimer()
	s.mu.Unlock()
	if st.srv != nil {
		st.srv.Close()
	}
	if err := os.Remove(s.Socket(st.id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.Log.Error("removing the agent's AWS credentials socket", "agent", st.id, "err", err)
	}
}

func (st *state) stopTimer() {
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
}

// due reports whether a session should be asked for now: there is a profile
// and an SVID, none is in flight, and there is no session or it is within
// RefreshBefore of its end. Called with s.mu held.
func (s *Service) due(st *state) bool {
	if st.profile == nil || st.cert == nil || st.fetch != nil {
		return false
	}
	return st.creds == nil || !s.Now().Add(s.RefreshBefore).Before(st.creds.Expiration)
}

// startFetch asks for a session in the background. Its result is kept only
// if the agent and its profile are still the ones it was asked for. Called
// with s.mu held.
func (s *Service) startFetch(st *state) {
	f := &fetch{done: make(chan struct{})}
	st.fetch = f
	st.stopTimer()
	gen, profile, cert := st.gen, *st.profile, *st.cert
	go func() {
		ctx, cancel := context.WithTimeout(st.ctx, s.CallTimeout)
		defer cancel()
		start := s.Now()
		creds, err := s.Sessions.CreateSession(ctx, profile, cert)
		s.mu.Lock()
		defer s.mu.Unlock()
		defer close(f.done)
		f.err = err
		if st.fetch == f {
			st.fetch = nil
		}
		if s.agents[st.id] != st {
			return
		}
		if st.gen != gen {
			// The profile changed while this was in flight: what it brought
			// is another role's. Ask for the new profile's session now.
			if s.due(st) {
				s.startFetch(st)
			}
			return
		}
		if err != nil {
			st.failures, st.lastErr = st.failures+1, err
			wait := min(retryMin<<min(st.failures-1, 8), retryMax)
			s.Log.Error("AWS session request failed", "agent", st.id, "err", err, "retry_in", wait, "latency", s.Now().Sub(start))
			s.schedule(st, wait)
			return
		}
		st.creds, st.failures, st.lastErr = &creds, 0, nil
		s.Log.Info("AWS session", "agent", st.id, "role", profile.RoleARN, "expires", creds.Expiration, "latency", s.Now().Sub(start))
		s.schedule(st, refreshIn(creds.Expiration.Sub(s.Now()), s.RefreshBefore, s.RefreshMin))
	}()
}

// refreshIn is when to ask for the next session, with left of this one:
// before its end, or halfway through what is left when that is no more
// than before; never sooner than floor.
func refreshIn(left, before, floor time.Duration) time.Duration {
	if left > before {
		return max(left-before, floor)
	}
	return max(left/2, floor)
}

// schedule asks for a session again after wait. Called with s.mu held.
func (s *Service) schedule(st *state, wait time.Duration) {
	st.stopTimer()
	st.timer = time.AfterFunc(max(wait, 0), func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.agents[st.id] == st && st.ctx.Err() == nil && s.due(st) {
			s.startFetch(st)
		}
	})
}

// handler serves agent st.
func (s *Service) handler(st *state) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != CredentialsPath {
			http.NotFound(w, r)
			return
		}
		select {
		case st.slots <- struct{}{}:
			defer func() { <-st.slots }()
		default:
			w.Header().Set("Retry-After", "1")
			s.refuse(w, st, http.StatusTooManyRequests, ReasonAgentBusy)
			return
		}
		start := s.Now()
		creds, reason := s.credentials(r.Context(), st)
		if reason != "" {
			s.refuse(w, st, http.StatusServiceUnavailable, reason)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(struct {
			AccessKeyID     string `json:"AccessKeyId"`
			SecretAccessKey string `json:"SecretAccessKey"`
			Token           string `json:"Token"`
			Expiration      string `json:"Expiration"`
		}{creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken, creds.Expiration.UTC().Format(time.RFC3339)})
		s.Log.Debug("AWS credentials served", "agent", st.id, "expires", creds.Expiration, "latency", s.Now().Sub(start))
	})
}

// refuse answers status, the reason in HeaderReason and in a JSON body of
// the shape the AWS SDKs read an endpoint's error from, {"code","message"}.
func (s *Service) refuse(w http.ResponseWriter, st *state, status int, reason string) {
	w.Header().Set(HeaderReason, reason)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{reason, messages[reason]})
	s.Log.Warn("AWS credentials refused", "agent", st.id, "status", status, "reason", reason)
}

// credentials is the session to serve, or why there is none: one from
// memory with at least minRemaining left, or the one a request in flight
// (started here if none is) brings within RequestWait.
func (s *Service) credentials(ctx context.Context, st *state) (rolesanywhere.Credentials, string) {
	s.mu.Lock()
	if c, ok := s.fresh(st); ok {
		s.mu.Unlock()
		return c, ""
	}
	switch {
	case st.profileErr != nil:
		s.mu.Unlock()
		return rolesanywhere.Credentials{}, ReasonConfigInvalid
	case st.cert == nil:
		s.mu.Unlock()
		return rolesanywhere.Credentials{}, ReasonNoSVID
	}
	if st.fetch == nil && st.lastErr != nil && st.timer != nil {
		// Failed a moment ago, and retried on a schedule: the SDK's retries
		// do not each ask Roles Anywhere again.
		err := st.lastErr
		s.mu.Unlock()
		return rolesanywhere.Credentials{}, reasonOf(err)
	}
	if st.fetch == nil {
		s.startFetch(st)
	}
	f := st.fetch
	s.mu.Unlock()

	wait := time.NewTimer(s.RequestWait)
	defer wait.Stop()
	select {
	case <-f.done:
	case <-wait.C:
		return rolesanywhere.Credentials{}, ReasonSessionPending
	case <-ctx.Done():
		return rolesanywhere.Credentials{}, ReasonSessionPending
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.fresh(st); ok {
		return c, ""
	}
	if f.err == nil {
		// The session came for a profile replaced meanwhile.
		return rolesanywhere.Credentials{}, ReasonSessionPending
	}
	return rolesanywhere.Credentials{}, reasonOf(f.err)
}

// reasonOf is the reason a failed session request gives.
func reasonOf(err error) string {
	var e *rolesanywhere.Error
	if errors.As(err, &e) && e.Refused() {
		return ReasonSessionRefused
	}
	return ReasonUnavailable
}

// fresh is the session in memory if it has minRemaining left. Called with
// s.mu held.
func (s *Service) fresh(st *state) (rolesanywhere.Credentials, bool) {
	if st.creds == nil || st.profile == nil || !s.Now().Add(minRemaining).Before(st.creds.Expiration) {
		return rolesanywhere.Credentials{}, false
	}
	return *st.creds, true
}

func sameProfile(a, b *rolesanywhere.Profile) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprint(err)
}

// socketMode is an agent's socket's: its owner, redoma xds, and its group,
// which the host Envoy joins to connect (as to the xDS socket,
// server.SocketMode).
const socketMode = 0o660

// listen opens the unix socket at path, replacing a stale one left by an
// earlier run, with socketMode. Until the chmod it has the umask's mode,
// owner-only under the unit (UMask=0077).
func listen(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, socketMode); err != nil {
		l.Close()
		return nil, fmt.Errorf("chmod %s: %w", path, err)
	}
	return l, nil
}
