GO      ?= go
BIN     ?= bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -buildid= -X main.version=$(VERSION)

.PHONY: all build test race lint vet staticcheck fmt scan tidy clean verify-image

all: lint test build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/member-api ./api/cmd/member-api

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

# staticcheck is run at a pinned version via `go run` (fetched on first use).
staticcheck:
	$(GO) run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./...

lint: vet staticcheck

fmt:
	test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }

scan:
	gitleaks git --redact .

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(BIN)

# Check an image's signed build provenance before deploying it (README,
# "Verifying an image"): built by this repository's CI from main on a
# GitHub-hosted runner. IMAGE=ghcr.io/vettid/member-service@sha256:<digest>
verify-image:
	@case "$(IMAGE)" in ghcr.io/vettid/member-service@sha256:*) ;; *) echo 'IMAGE=ghcr.io/vettid/member-service@sha256:<digest>' >&2; exit 2;; esac
	gh attestation verify oci://$(IMAGE) --repo vettid/member-service \
	  --signer-workflow vettid/member-service/.github/workflows/ci.yml \
	  --source-ref refs/heads/main --deny-self-hosted-runners
