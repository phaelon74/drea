.PHONY: build test vet fmt install clean

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
