IMAGE_REPO ?= ghcr.io/martin31821/eks-webhack
IMAGE_TAG  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

ENVTEST_K8S_VERSION ?= 1.31
ENVTEST_ASSETS      ?= /tmp/envtest-bins/k8s

GOFLAGS ?=

.PHONY: all
all: build

# ── Build ────────────────────────────────────────────────────────────────────

.PHONY: build
build:
	go build $(GOFLAGS) ./...

.PHONY: binary
binary:
	CGO_ENABLED=0 go build $(GOFLAGS) -o bin/eks-webhack ./cmd

# ── Test ─────────────────────────────────────────────────────────────────────

.PHONY: test
test: envtest
	KUBEBUILDER_ASSETS=$(shell setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_ASSETS) -p path) \
		go test $(GOFLAGS) ./... -coverprofile cover.out

.PHONY: test-verbose
test-verbose: envtest
	KUBEBUILDER_ASSETS=$(shell setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_ASSETS) -p path) \
		go test $(GOFLAGS) ./... -v --ginkgo.v

# ── Lint / check ─────────────────────────────────────────────────────────────

.PHONY: vet
vet:
	go vet ./...

.PHONY: fmt
fmt:
	gofmt -w -s .

.PHONY: fmt-check
fmt-check:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "unformatted files:\n$$out"; exit 1; fi

# ── Tools ────────────────────────────────────────────────────────────────────

.PHONY: envtest
envtest:
	@which setup-envtest >/dev/null 2>&1 || go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
	@setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_ASSETS) -p path >/dev/null

# ── Container image ──────────────────────────────────────────────────────────

.PHONY: docker-build
docker-build:
	docker buildx build --load -t $(IMAGE_REPO):$(IMAGE_TAG) .

.PHONY: docker-push
docker-push:
	docker push $(IMAGE_REPO):$(IMAGE_TAG)

# ── Helm ─────────────────────────────────────────────────────────────────────

.PHONY: helm-lint
helm-lint:
	helm lint helm/eks-webhack

.PHONY: helm-template
helm-template:
	helm template eks-webhack helm/eks-webhack

.PHONY: helm-package
helm-package:
	helm package helm/eks-webhack --destination dist/

# ── Housekeeping ─────────────────────────────────────────────────────────────

.PHONY: clean
clean:
	rm -rf bin/ dist/ cover.out
