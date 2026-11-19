.PHONY: check vet test fuzz bench demo demo-serve

check: vet test

vet:
	gofmt -l . | (! grep .) || (echo "gofmt needed"; exit 1)
	go vet ./...

test:
	go test -race -count=1 ./...

fuzz:
	go test -run '^$$' -fuzz FuzzExchange -fuzztime 30s ./internal/trading/

bench:
	go test -run '^$$' -bench . ./internal/platform/telemetry/ ./internal/app/instrument/

demo:
	go run ./cmd/demo

demo-serve:
	go run ./cmd/demo -addr 127.0.0.1:9090 -wait 60s
