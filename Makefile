RELEASE_VERSION ?= "dev"
RELEASE_REGISTRY ?= "quay.io/bbrowning/crc-cluster-bot"

build:
	go build -ldflags "-X k8s.io/client-go/pkg/version.gitVersion=$$(git describe --abbrev=8 --dirty --always)" -mod vendor -o crc-cluster-bot .
	podman build . -t $(RELEASE_REGISTRY):v$(RELEASE_VERSION)
.PHONY: build

release: build
	podman push $(RELEASE_REGISTRY):v$(RELEASE_VERSION)
.PHONY: release

update-deps:
	GO111MODULE=on go mod vendor
.PHONY: update-deps

run:
	./hack/run.sh
.PHONY: run
