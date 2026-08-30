.PHONY: build test test-short vet fmt check binary e2e e2e-docker vulncheck release-check release-snapshot landing-build landing-serve landing-og fonts

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

# The docker-gated e2e scenarios plus everything `make e2e` runs: the extra tag
# only adds files. Needs a reachable daemon (macOS: colima start).
e2e-docker:
	go test -tags 'e2e docker' ./e2e -count=1 -timeout 600s

# Reachability-based vulnerability scan: it reports what the code actually
# calls, so a finding here is work rather than an advisory. Pinned rather than
# @latest so a scanner release cannot turn a green build red on its own, and
# installed on demand rather than added to go.mod - it is not a build
# dependency and the module graph is deliberately small.
GOVULNCHECK_VERSION ?= v1.6.0

vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

# Validate .goreleaser.yaml without building anything.
release-check:
	goreleaser check

# Full release build into dist/ without publishing: same archives, checksums
# and ldflags the tag push would produce.
release-snapshot:
	goreleaser release --snapshot --clean

# Landing build: the sources carry {{VERSION}} / {{VERSION_NUM}} placeholders,
# rendered here into dist/landing. The release workflow passes the published tag;
# locally the latest tag is used, or "latest" when the repo has none yet.
LANDING_OUT ?= dist/landing
LANDING_VERSION ?= $(shell git describe --tags --abbrev=0 2>/dev/null || echo latest)
# Archive names carry the version without the leading "v" (see .goreleaser.yaml).
LANDING_VERSION_NUM = $(patsubst v%,%,$(LANDING_VERSION))

landing-build:
	rm -rf $(LANDING_OUT)
	mkdir -p $(LANDING_OUT)
	cp -R landing/. $(LANDING_OUT)/
	# A pipe, not `sed -i`: the -i syntax differs between BSD and GNU sed.
	sed -e 's|{{VERSION}}|$(LANDING_VERSION)|g' \
	    -e 's|{{VERSION_NUM}}|$(LANDING_VERSION_NUM)|g' \
	    landing/index.html > $(LANDING_OUT)/index.html
	@! grep -n '{{' $(LANDING_OUT)/index.html || \
		{ echo "landing-build: unsubstituted placeholder above"; false; }

# Serve the built landing page locally (http://localhost:8000/).
landing-serve: landing-build
	python3 -m http.server -d $(LANDING_OUT) 8000

# Rasterize the social preview: landing/og-image.svg -> landing/og-image.png (1200x630).
# Requires librsvg (macOS: brew install librsvg).
landing-og:
	rsvg-convert -w 1200 -h 630 landing/og-image.svg -o landing/og-image.png

# Refetch the vendored display face from Google Fonts. Author-time only:
# the .woff2 files are committed, so neither the build nor the server ever
# needs network access. Re-run when bumping the font version.
FONT_UA := Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36
FONT_DIR := internal/web/static/fonts

fonts:
	@mkdir -p $(FONT_DIR)
	@css=$$(curl -sf -A "$(FONT_UA)" \
	  "https://fonts.googleapis.com/css2?family=Geologica:wght@400..700&display=swap"); \
	for sub in latin cyrillic; do \
	  url=$$(printf '%s\n' "$$css" | awk -v s="/* $$sub */" '$$0==s{f=1} f&&/url\(/{print;exit}' \
	    | sed -E 's/.*url\((.*)\) format.*/\1/'); \
	  test -n "$$url" || { echo "no $$sub subset found"; exit 1; }; \
	  curl -sf -A "$(FONT_UA)" "$$url" -o $(FONT_DIR)/geologica-$$sub.woff2; \
	  echo "fetched $(FONT_DIR)/geologica-$$sub.woff2"; \
	done
