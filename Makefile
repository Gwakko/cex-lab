.PHONY: check vet test fuzz demo

check: vet test

vet:
	gofmt -l . | (! grep .) || (echo "gofmt needed"; exit 1)
	go vet ./...

test:
	go test -race -count=1 ./...

fuzz:
	go test -run '^$$' -fuzz FuzzExchange -fuzztime 30s ./exchange/

demo:
	go run ./cmd/demo
