package identity

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"log/slog"
	"sync"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// Workload is redoma xds's own identity from the Workload API, in Broker
// mode: the SVID it presents to the Broker API (spiffe://<td>/host/sds) and
// the trust domain's bundle, which it checks the SPIRE agent with and
// serves Envoy. The Workload API is dialled in the background and waited
// for, so the control plane serves the agents' listeners while SPIRE is not
// there yet.
type Workload struct {
	trustDomain spiffeid.TrustDomain
	log         *slog.Logger

	ready chan struct{}
	once  sync.Once
	src   *workloadapi.X509Source // set before ready is closed
	err   error
}

var (
	_ SVIDSource    = (*Workload)(nil)
	_ BundleWatcher = (*Workload)(nil)
)

// errNotYet is the Workload API not having answered yet.
var errNotYet = errors.New("redoma xds has no SVID of its own yet: the Workload API has not answered")

// OpenWorkload starts fetching redoma xds's SVID from the Workload API at
// addr (unix:///...), until ctx ends.
func OpenWorkload(ctx context.Context, addr string, trustDomain spiffeid.TrustDomain, log *slog.Logger) *Workload {
	w := &Workload{trustDomain: trustDomain, log: log, ready: make(chan struct{})}
	go func() {
		// NewX509Source retries the Workload API until its first answer.
		src, err := workloadapi.NewX509Source(ctx, workloadapi.WithClientOptions(workloadapi.WithAddr(addr)))
		w.once.Do(func() {
			w.src, w.err = src, err
			close(w.ready)
		})
		if err != nil && ctx.Err() == nil {
			log.Error("redoma xds cannot get its own SVID from the Workload API", "addr", addr, "err", err)
		}
	}()
	return w
}

// source is the X509Source once there.
func (w *Workload) source() (*workloadapi.X509Source, error) {
	select {
	case <-w.ready:
		if w.err != nil {
			return nil, w.err
		}
		return w.src, nil
	default:
		return nil, errNotYet
	}
}

// GetX509SVID is redoma xds's SVID.
func (w *Workload) GetX509SVID() (*x509svid.SVID, error) {
	src, err := w.source()
	if err != nil {
		return nil, err
	}
	return src.GetX509SVID()
}

// GetX509BundleForTrustDomain is the bundle of td.
func (w *Workload) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	src, err := w.source()
	if err != nil {
		return nil, err
	}
	return src.GetX509BundleForTrustDomain(td)
}

// WatchX509Bundle calls update with the trust domain's bundle, PEM, once the
// Workload API has answered and each time it changes, until ctx ends.
func (w *Workload) WatchX509Bundle(ctx context.Context, update func([]byte)) {
	select {
	case <-ctx.Done():
		return
	case <-w.ready:
	}
	src, err := w.source()
	if err != nil {
		return
	}
	watchBundle(ctx, func() ([]byte, error) {
		b, err := src.GetX509BundleForTrustDomain(w.trustDomain)
		if err != nil {
			return nil, err
		}
		return bundlePEM(b), nil
	}, src.Updated(), update, w.log)
}

// Close closes the Workload API's stream, once it is there.
func (w *Workload) Close() error {
	src, err := w.source()
	if err != nil {
		return nil
	}
	return src.Close()
}

// watchBundle calls update with get's bundle now and after each value of
// updated that changed it, until ctx ends. A bundle get cannot read is
// logged and leaves the last one served.
func watchBundle(ctx context.Context, get func() ([]byte, error), updated <-chan struct{}, update func([]byte), log *slog.Logger) {
	var last []byte
	for {
		b, err := get()
		switch {
		case err != nil:
			log.Error("the trust domain's bundle from the Workload API", "err", err)
		case !bytes.Equal(b, last):
			last = b
			update(b)
		}
		select {
		case <-ctx.Done():
			return
		case <-updated:
		}
	}
}

// bundlePEM is a bundle's X.509 authorities, PEM.
func bundlePEM(b *x509bundle.Bundle) []byte {
	var out []byte
	for _, c := range b.X509Authorities() {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return out
}
