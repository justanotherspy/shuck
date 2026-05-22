BINARY := shuck
GOLANGCI_LINT_VERSION := v2.12.2

.PHONY: all build test cover vet fmt tidy lint lint-install install clean

all: build

build:
	go build -o $(BINARY) .

test:
	go test -race ./...

cover:
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

vet:
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

lint:
	golangci-lint run ./...

lint-install:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

install:
	go install .

clean:
	rm -f $(BINARY) coverage.out
