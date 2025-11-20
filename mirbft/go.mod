module github.com/hyperledger-labs/mirbft

go 1.18

require (
	github.com/c9s/goprocinfo v0.0.0-20210130143923-c95fcf8c64a8
	github.com/golang/protobuf v1.5.0
	github.com/rs/zerolog v1.34.0
	go.dedis.ch/kyber/v3 v3.1.0
	google.golang.org/grpc v1.66.2
	google.golang.org/protobuf v1.34.2
	gopkg.in/yaml.v2 v2.4.0
)

require (
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.19 // indirect
	go.dedis.ch/fixbuf v1.0.3 // indirect
	golang.org/x/crypto v0.24.0 // indirect
	golang.org/x/net v0.26.0 // indirect
	golang.org/x/sys v0.21.0 // indirect
	golang.org/x/text v0.16.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240604185151-ef581f913117 // indirect
)

replace github.com/hyperledger-labs/mirbft => /tmp/ISS_com_Multipaxos/mirbft
replace github.com/hyperledger-labs/ISS_com_Multipaxos/mirbft => /tmp/ISS_com_Multipaxos/mirbft

