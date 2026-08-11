.PHONY: build test vet fmt install clean verify

BIN := drea

build:
	go build -o $(BIN) ./cmd/drea

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

install:
	go install ./cmd/drea

clean:
	rm -f $(BIN)

verify:
	@test -z "$$(gofmt -l .)" || { echo "gofmt: unformatted files:"; gofmt -l .; exit 1; }
	go vet ./...
	go test ./...
	go test -race ./...
	go build ./...
