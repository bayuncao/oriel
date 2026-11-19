module github.com/bayuncao/oriel

go 1.27.4

require (
	github.com/coder/acp-go-sdk v0.13.5
	github.com/coder/websocket v1.8.15
	github.com/egoist/mygo v0.8.3
	github.com/flynn/noise v1.1.0
	github.com/klauspost/compress v1.20.4
	golang.org/x/net v0.62.0
)

require (
	github.com/BurntSushi/toml v1.4.1-0.20240526193622-a339e1f7089c // indirect
	github.com/ebitengine/purego v0.11.3 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/exp/typeparams v0.0.0-20231108232855-2478ac86f678 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/sync v0.25.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.44.0 // indirect
	golang.org/x/tools v0.52.0 // indirect
	honnef.co/go/tools v0.8.2 // indirect
)

tool (
	github.com/egoist/mygo/cmd/mygo
	honnef.co/go/tools/cmd/staticcheck
)
