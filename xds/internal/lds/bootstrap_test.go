package lds

import (
	"encoding/json"
	"os"
	"testing"

	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	dfpclusterv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/clusters/dynamic_forward_proxy/v3"
	dfpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/dynamic_forward_proxy/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"gopkg.in/yaml.v3"

	"github.com/bpalermo/redoma/xds/internal/modelauth"
)

// fromYAML reads a YAML file into m, as Envoy reads it: YAML to JSON, then
// the proto's JSON mapping, every typed config resolved by its type.
func fromYAML(t *testing.T, path string, m proto.Message) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	js, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if err := protojson.Unmarshal(js, m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// TestExampleBootstrap: examples/envoy.yaml is a bootstrap, takes listeners
// and clusters over ADS from the xDS socket, and has the clusters the
// listeners route to, the egress one with the egress listeners' DNS cache
// (Envoy refuses two configs of one cache name).
func TestExampleBootstrap(t *testing.T) {
	var b bootstrapv3.Bootstrap
	fromYAML(t, "../../../examples/envoy.yaml", &b)

	dyn := b.GetDynamicResources()
	if dyn.GetAdsConfig().GetApiType().String() != "GRPC" || dyn.GetLdsConfig().GetAds() == nil || dyn.GetCdsConfig().GetAds() == nil {
		t.Errorf("dynamic_resources %v: want SotW ADS for listeners and clusters", dyn)
	}
	clusters := map[string]*clusterv3.Cluster{}
	for _, c := range b.GetStaticResources().GetClusters() {
		clusters[c.GetName()] = c
	}
	xdsCluster := dyn.GetAdsConfig().GetGrpcServices()[0].GetEnvoyGrpc().GetClusterName()
	pipe := clusters[xdsCluster].GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetPipe().GetPath()
	if pipe != "/run/redoma-xds/xds.sock" {
		t.Errorf("the ADS cluster %q connects to %q, not redoma xds's default socket", xdsCluster, pipe)
	}

	anthropic := clusters[anthropicCluster]
	var tls tlsv3.UpstreamTlsContext
	if err := anthropic.GetTransportSocket().GetTypedConfig().UnmarshalTo(&tls); err != nil || tls.GetSni() != "api.anthropic.com" {
		t.Errorf("cluster %q: TLS %v (%v), want SNI api.anthropic.com", anthropicCluster, &tls, err)
	}

	var dfp dfpclusterv3.ClusterConfig
	if err := clusters[egressCluster].GetClusterType().GetTypedConfig().UnmarshalTo(&dfp); err != nil {
		t.Fatalf("cluster %q: %v", egressCluster, err)
	}
	if want := egressCache(t); !proto.Equal(dfp.GetDnsCacheConfig(), want) {
		t.Errorf("cluster %q's DNS cache %v, the egress listeners' %v", egressCluster, dfp.GetDnsCacheConfig(), want)
	}
}

// egressCache is the DNS cache config of an egress listener's forward proxy
// filter.
func egressCache(t *testing.T) proto.Message {
	t.Helper()
	for _, l := range Listeners("/run/netns/x", "x", modelauth.APIKey, nil) {
		if l.GetName() != ListenerName("x", "egress") {
			continue
		}
		var h hcmv3.HttpConnectionManager
		if err := l.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(&h); err != nil {
			t.Fatal(err)
		}
		for _, f := range h.GetHttpFilters() {
			var c dfpv3.FilterConfig
			if f.GetTypedConfig().UnmarshalTo(&c) == nil {
				return c.GetDnsCacheConfig()
			}
		}
	}
	t.Fatal("no egress listener with a forward proxy filter")
	return nil
}

// TestExampleSecret: examples/anthropic-key.yaml is an SDS file of the
// secret the model listeners read.
func TestExampleSecret(t *testing.T) {
	var file struct {
		Resources []json.RawMessage `json:"resources"`
	}
	raw, err := os.ReadFile("../../../examples/anthropic-key.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	js, _ := json.Marshal(doc)
	if err := json.Unmarshal(js, &file); err != nil || len(file.Resources) != 1 {
		t.Fatalf("%v: want one resource", err)
	}
	var a anypb.Any
	if err := protojson.Unmarshal(file.Resources[0], &a); err != nil {
		t.Fatal(err)
	}
	var s tlsv3.Secret
	if err := a.UnmarshalTo(&s); err != nil || s.GetName() != anthropicKeySecret || s.GetGenericSecret() == nil {
		t.Errorf("secret %v (%v), want the generic secret %q", &s, err, anthropicKeySecret)
	}
}
