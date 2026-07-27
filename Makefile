.PHONY: build test test-short vet fmt check binary e2e release-check release-snapshot landing-serve landing-og

# Build metadata stamped into the binary (internal/version). A release build
# gets these from goreleaser instead; here they describe the working copy.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION_PKG = github.com/ekalinin/anygrade/internal/version
LDFLAGS = -X $(VERSION_PKG).Version=$(VERSION) \
          -X $(VERSION_PKG).Commit=$(COMMIT) \
          -X $(VERSION_PKG).Date=$(DATE)

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
	go build -ldflags "$(LDFLAGS)" -o anygrade ./cmd/anygrade

e2e:
	go test -tags e2e ./e2e -count=1 -timeout 600s

# Validate .goreleaser.yaml without building anything.
release-check:
	goreleaser check

# Full release build into dist/ without publishing: same archives, checksums
# and ldflags the tag push would produce.
release-snapshot:
	goreleaser release --snapshot --clean

# Serve the static landing page locally (http://localhost:8000/).
landing-serve:
	python3 -m http.server -d landing 8000

# Rasterize the social preview: landing/og-image.svg -> landing/og-image.png (1200x630).
# Requires librsvg (macOS: brew install librsvg).
landing-og:
	rsvg-convert -w 1200 -h 630 landing/og-image.svg -o landing/og-image.png
