.PHONY: build test test-short vet fmt check binary e2e landing-serve landing-og

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

# Serve the static landing page locally (http://localhost:8000/).
landing-serve:
	python3 -m http.server -d landing 8000

# Rasterize the social preview: landing/og-image.svg -> landing/og-image.png (1200x630).
# Requires librsvg (macOS: brew install librsvg).
landing-og:
	rsvg-convert -w 1200 -h 630 landing/og-image.svg -o landing/og-image.png
