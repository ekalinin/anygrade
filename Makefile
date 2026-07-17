.PHONY: build test test-short vet fmt check binary e2e

check: build vet fmt test

build:
	go build ./...

test:
	go test ./... -count=1 -timeout 300s

test-short:
	go test ./... -short -count=1

vet:
	go vet ./...

fmt:
	test -z "$$(gofmt -l cmd internal e2e)"

binary:
	go build -o anygrade ./cmd/anygrade

e2e:
	go test -tags e2e ./e2e -count=1 -timeout 600s
