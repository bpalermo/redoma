module github.com/bpalermo/redoma

go 1.27.2

tool (
	github.com/uudashr/gocognit/cmd/gocognit
	google.golang.org/grpc/cmd/protoc-gen-go-grpc
	honnef.co/go/tools/cmd/staticcheck
)

require (
	github.com/envoyproxy/go-control-plane v0.14.0
	github.com/envoyproxy/go-control-plane/envoy v1.40.0
	github.com/google/go-cmp v0.7.0
	github.com/spf13/cobra v1.10.2
	github.com/spiffe/go-spiffe/v2 v2.9.0
	github.com/spiffe/spire-api-sdk v1.15.3
	google.golang.org/grpc v1.84.0
	google.golang.org/grpc/cmd/protoc-gen-go-grpc v1.6.2
	google.golang.org/protobuf v1.36.12
	gopkg.in/yaml.v3 v3.0.1
)

require (
	cel.dev/expr v0.25.2 // indirect
	github.com/BurntSushi/toml v1.4.1-0.20240526193622-a339e1f7089c // indirect
	github.com/cncf/xds/go v0.0.0-20260202195803-dba9d589def2 // indirect
	github.com/envoyproxy/go-control-plane/ratelimit v0.1.0 // indirect
	github.com/envoyproxy/protoc-gen-validate v1.3.3 // indirect
	github.com/go-jose/go-jose/v4 v4.1.5 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/planetscale/vtprotobuf v0.6.1-0.20240319094008-0393e58bdf10 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	github.com/uudashr/gocognit v1.2.2 // indirect
	golang.org/x/exp/typeparams v0.0.0-20231108232855-2478ac86f678 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	golang.org/x/tools v0.52.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260720211330-0afa2a65878a // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260720211330-0afa2a65878a // indirect
	honnef.co/go/tools v0.8.2 // indirect
)
