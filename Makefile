GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# 编译次数：git commit 计数（单调递增，每个 commit 唯一）。
# 唯一版本 = VERSION + "+b" + BUILD_NUMBER，例：v1.1.1+b123。
# 镜像 tag 用 "版本-编译次数"（docker tag 不允许 '+'），例：1.1.1-123。
BUILD_NUMBER ?= $(shell git rev-list --count HEAD 2>/dev/null || echo 0)
IMAGE_TAG    ?= $(shell echo "$(VERSION)" | sed 's/^v//' )-$(BUILD_NUMBER)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.buildNumber=$(BUILD_NUMBER)

.PHONY: build test vet e2e image image-multi deploy clean version

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o goacos .

version:
	@echo "VERSION=$(VERSION) BUILD_NUMBER=$(BUILD_NUMBER) UNIQUE=$(VERSION)+b$(BUILD_NUMBER) IMAGE_TAG=goacos:$(IMAGE_TAG)"

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

# full end-to-end acceptance (boots a throwaway MySQL 8 container)
e2e:
	bash scripts/e2e.sh

# native-arch image; tags both the unique version and a moving "local" alias
image:
	docker build --build-arg VERSION=$(VERSION) --build-arg BUILD_NUMBER=$(BUILD_NUMBER) \
	  -t goacos:$(IMAGE_TAG) -t goacos:local .

# both architectures locally (amd64 runs under emulation)
image-multi:
	docker buildx build --platform linux/amd64,linux/arm64 \
	  --build-arg VERSION=$(VERSION) --build-arg BUILD_NUMBER=$(BUILD_NUMBER) \
	  -t goacos:$(IMAGE_TAG) -t goacos:multi .

# dynamic-discovery deploy against whatever MySQL is reachable
deploy:
	bash deploy/deploy.sh --image goacos:$(IMAGE_TAG) --force

clean:
	rm -f goacos
