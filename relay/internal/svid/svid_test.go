package svid_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/grpc"

	"github.com/bpalermo/redoma/relay/internal/mtls/mtlstest"
	"github.com/bpalermo/redoma/relay/internal/svid"
)

// fakeAPI is a Workload API that sends whatever is put on its channel.
type fakeAPI struct {
	workload.UnimplementedSpiffeWorkloadAPIServer
	updates chan *workload.X509SVIDResponse
}

func (f *fakeAPI) FetchX509SVID(_ *workload.X509SVIDRequest, st grpc.ServerStreamingServer[workload.X509SVIDResponse]) error {
	for {
		select {
		case <-st.Context().Done():
			return nil
		case u := <-f.updates:
			if err := st.Send(u); err != nil {
				return err
			}
		}
	}
}

func response(t *testing.T, ca *mtlstest.CA, id string) *workload.X509SVIDResponse {
	t.Helper()
	der, key := ca.IssueDER(t, id)
	return &workload.X509SVIDResponse{Svids: []*workload.X509SVID{{SpiffeId: id, X509Svid: der, X509SvidKey: key, Bundle: ca.DER()}}}
}

// TestOpenFollowsTheWorkloadAPI: the source waits for its first SVID and
// follows a rotation live. The fake listens on loopback TCP, which the
// tests' sandbox allows; the relay's address is a unix socket.
func TestOpenFollowsTheWorkloadAPI(t *testing.T) {
	ca := mtlstest.NewCA(t, "redoma.internal")
	api := &fakeAPI{updates: make(chan *workload.X509SVIDResponse, 2)}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	workload.RegisterSpiffeWorkloadAPIServer(g, api)
	go func() { _ = g.Serve(l) }()
	t.Cleanup(g.Stop)

	api.updates <- response(t, ca, "spiffe://redoma.internal/host/relay")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	src, err := svid.Open(ctx, "tcp://"+l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	first, err := src.GetX509SVID()
	if err != nil || first.ID.String() != "spiffe://redoma.internal/host/relay" {
		t.Fatalf("first SVID %v, %v", first, err)
	}
	if _, err := src.GetX509BundleForTrustDomain(spiffeid.RequireTrustDomainFromString("redoma.internal")); err != nil {
		t.Errorf("bundle: %v", err)
	}

	api.updates <- response(t, ca, "spiffe://redoma.internal/host/relay")
	deadline := time.Now().Add(10 * time.Second)
	for {
		s, _ := src.GetX509SVID()
		if s.Certificates[0].SerialNumber.Cmp(first.Certificates[0].SerialNumber) != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the rotation did not reach the source")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
