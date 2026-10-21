GO ?= go
OFFLINE = env GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local

.PHONY: build test vet vendor

build:
	$(OFFLINE) CGO_ENABLED=0 $(GO) build -mod=vendor -buildvcs=false -trimpath -ldflags="-s -w" -o bin/mailspot ./cmd/mailspot
	$(OFFLINE) CGO_ENABLED=0 $(GO) build -mod=vendor -buildvcs=false -trimpath -ldflags="-s -w" -o bin/loadtest ./cmd/loadtest

test:
	$(OFFLINE) $(GO) test -mod=vendor -race ./...

vet:
	$(OFFLINE) $(GO) vet -mod=vendor ./...

# Regenerate after an intentional dependency update; may require network access.
vendor:
	$(GO) mod vendor
