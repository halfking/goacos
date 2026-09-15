GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet e2e image image-multi deploy clean

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o goacos .

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

# full end-to-end acceptance (boots a throwaway MySQL 8 container)
e2e:
	bash scripts/e2e.sh

# native-arch image
image:
	docker build --build-arg VERSION=$(VERSION) -t goacos:local .

# both architectures locally (amd64 runs under emulation)
image-multi:
	docker buildx build --platform linux/amd64,linux/arm64 \
	  --build-arg VERSION=$(VERSION) -t goacos:multi .

# dynamic-discovery deploy against whatever MySQL is reachable
deploy:
	bash deploy/deploy.sh --image goacos:local --force

clean:
	rm -f goacos
