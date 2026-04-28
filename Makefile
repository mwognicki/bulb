GO      ?= go
BIN_DIR ?= bin
BIN     ?= $(BIN_DIR)/bulb

.PHONY: all build test vet lint tidy clean

all: vet test build

build:
	$(GO) build -o $(BIN) ./cmd/bulb

test:
	$(GO) test -race -count=1 ./...

vet:
	$(GO) vet ./...

lint:
	golangci-lint run

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(BIN_DIR)
