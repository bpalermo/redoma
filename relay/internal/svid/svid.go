// Package svid is where the relay gets its own X.509 SVID and the trust
// domain's bundle: the host SPIRE agent's Workload API, which attests the
// relay by its uid (963, redoma-relay) and serves spiffe://<td>/host/relay. Source is the interface the relay
// codes against; Open is the one it ships with.
package svid

import (
	"context"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// Source is the relay's identity: its SVID, served to Envoy, and the
// bundles the agents' certificates are verified against.
type Source interface {
	x509svid.Source
	x509bundle.Source
}

// Open returns a Source fed by the Workload API at addr. It waits for the
// first SVID (or ctx's end); after that it follows rotations live and
// redials when the agent restarts. Close it when done.
func Open(ctx context.Context, addr string) (*workloadapi.X509Source, error) {
	return workloadapi.NewX509Source(ctx, workloadapi.WithClientOptions(workloadapi.WithAddr(addr)))
}

var _ Source = (*workloadapi.X509Source)(nil)
