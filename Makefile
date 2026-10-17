# Image URL to use all building/pushing image targets
IMG ?= controller:latest
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= podman

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# The kind end-to-end suite is test/e2e/e2e.sh (see `make e2e` below).
.PHONY: test-e2e
test-e2e: e2e ## Alias for `make e2e` (the kind suite in test/e2e/e2e.sh).

# golangci-lint can't read export data from Go toolchains newer than it was built
# for, so lint with the go.mod version (what CI uses) regardless of the local Go.
LINT_GOTOOLCHAIN ?= go$(shell sed -n 's/^go //p' go.mod)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	GOTOOLCHAIN=$(LINT_GOTOOLCHAIN) "$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	GOTOOLCHAIN=$(LINT_GOTOOLCHAIN) "$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# Override BASE_IMAGE to build from another registry, e.g.
# make docker-build IMG=<img> BASE_IMAGE=docker.io/library/golang:1.26
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name functions-operator-builder
	$(CONTAINER_TOOL) buildx use functions-operator-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm functions-operator-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default | sed 's|--runtime-image=RUNTIME_IMG|--runtime-image=$(RUNTIME_IMG)|' | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.22.0

#ENVTEST_VERSION is the setup-envtest version. It is a separate tool module: the scaffold's default
# (controller-runtime's go.mod tag) has no tools/setup-envtest package at v0.25.0, so track the release branch.
ENVTEST_VERSION ?= release-0.25

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.13.1
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef

##@ Local kind environment

export KIND_EXPERIMENTAL_PROVIDER = podman

.PHONY: kind-up
kind-up: ## Create kind cluster with KEDA, HTTP add-on and MinIO.
	KIND="$(KIND)" ./hack/kind/up.sh

.PHONY: kind-down
kind-down: ## Delete the kind cluster.
	KIND="$(KIND)" ./hack/kind/down.sh

.PHONY: kind-load
kind-load: ## Build IMG (native arch) and load it into the kind cluster (usage: make kind-load IMG=<image>).
	$(CONTAINER_TOOL) build -t $(IMG) $(if $(KIND_DOCKERFILE),-f $(KIND_DOCKERFILE)) .
	tmp=$$(mktemp -d) && \
	$(CONTAINER_TOOL) save -o $$tmp/image.tar $(IMG) && \
	$(KIND) load image-archive $$tmp/image.tar --name functions; \
	rc=$$?; rm -rf $$tmp; exit $$rc

##@ Runtime image and samples

RUNTIME_IMG ?= localhost/functions-runtime:dev
BUILD ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
SAMPLE_VERSION ?= v1
ARCHES ?= amd64 arm64
NATIVE_ARCH ?= $(shell uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')

.PHONY: runtime-image
runtime-image: ## Build the multi-arch runtime image manifest (funcrt + fetcher).
	-$(CONTAINER_TOOL) manifest rm $(RUNTIME_IMG) 2>/dev/null
	$(CONTAINER_TOOL) build --platform $(shell echo $(ARCHES) | sed 's/\([a-z0-9]*\)/linux\/\1/g; s/ /,/g') --manifest $(RUNTIME_IMG) -f images/runtime/Dockerfile .

.PHONY: runtime-image-kind
runtime-image-kind: ## Build the runtime image for the native arch and load it into kind (manifest lists don't load cleanly).
	$(MAKE) kind-load IMG=$(RUNTIME_IMG) KIND_DOCKERFILE=images/runtime/Dockerfile

.PHONY: samples
samples: ## Build sample bootstrap binaries for all ARCHES into samples/out (usage: make samples BUILD=<id>).
	@for a in $(ARCHES); do \
	  mkdir -p samples/out/go-hello/linux-$$a samples/out/dotnet-hello/linux-$$a && \
	  (cd samples/go-hello && CGO_ENABLED=0 GOOS=linux GOARCH=$$a go build -tags lambda.norpc -ldflags "-X main.build=$(BUILD)" -o ../out/go-hello/linux-$$a/bootstrap .) || exit 1; \
	  $(CONTAINER_TOOL) build --platform linux/$$a --build-arg BUILD=$(BUILD) -f samples/dotnet-hello/Dockerfile.build -t localhost/dotnet-hello-build:$$a samples/dotnet-hello || exit 1; \
	  c=$$($(CONTAINER_TOOL) create --platform linux/$$a localhost/dotnet-hello-build:$$a /bootstrap) && \
	  $(CONTAINER_TOOL) cp $$c:/bootstrap samples/out/dotnet-hello/linux-$$a/bootstrap; rc=$$?; \
	  $(CONTAINER_TOOL) rm $$c >/dev/null; [ $$rc -eq 0 ] || exit $$rc; \
	done

.PHONY: upload-samples
upload-samples: ## Upload built samples to the kind MinIO (builds them with `make samples` if samples/out is missing).
	@[ -f samples/out/go-hello/linux-$(NATIVE_ARCH)/bootstrap ] && [ -f samples/out/dotnet-hello/linux-$(NATIVE_ARCH)/bootstrap ] || $(MAKE) samples
	hack/kind/upload-sample.sh go-hello $(SAMPLE_VERSION) samples/out/go-hello
	hack/kind/upload-sample.sh dotnet-hello $(SAMPLE_VERSION) samples/out/dotnet-hello

##@ Deploy to kind and end-to-end test

KIND_CONTEXT ?= kind-functions
KIND_IMG ?= localhost/functions-operator:dev
HEY_IMG ?= localhost/hey:v0.1.5
OPERATOR_NS ?= functions-operator-system
KK = "$(KUBECTL)" --context $(KIND_CONTEXT)

.PHONY: deploy-kind
deploy-kind: manifests kustomize ## Build and load operator, runtime and hey images into kind; deploy operator, S3 secret, samples (ns team-a) and KrakenD (ns gateway).
	$(MAKE) kind-load IMG=$(KIND_IMG)
	$(MAKE) runtime-image-kind
	$(MAKE) kind-load IMG=$(HEY_IMG) KIND_DOCKERFILE=hack/kind/hey/Dockerfile
	$(KK) create namespace $(OPERATOR_NS) --dry-run=client -o yaml | $(KK) apply -f -
	$(KK) -n $(OPERATOR_NS) create secret generic functions-s3-read \
	  --from-literal=endpoint=minio.minio.svc.cluster.local:9000 --from-literal=useSSL=false \
	  --from-literal=accessKey=minioadmin --from-literal=secretKey=minioadmin \
	  --dry-run=client -o yaml | $(KK) apply -f -
	"$(KUSTOMIZE)" build config/kind | sed 's|--runtime-image=RUNTIME_IMG|--runtime-image=$(RUNTIME_IMG)|' | $(KK) apply -f -
	@# The image tags are fixed (:dev), so restart to pick up freshly loaded images.
	$(KK) -n $(OPERATOR_NS) rollout restart deploy/functions-operator-controller-manager
	$(KK) -n $(OPERATOR_NS) rollout status deploy/functions-operator-controller-manager --timeout=180s
	$(KK) create namespace team-a --dry-run=client -o yaml | $(KK) apply -f -
	$(KK) apply -k config/samples
	$(KK) apply -f hack/kind/krakend.yaml
	$(KK) -n gateway rollout status deploy/krakend --timeout=180s

.PHONY: undeploy-kind
undeploy-kind: kustomize ## Remove samples, KrakenD and the operator from kind.
	-$(KK) delete -k config/samples --ignore-not-found
	-$(KK) delete -f hack/kind/krakend.yaml --ignore-not-found
	-"$(KUSTOMIZE)" build config/kind | $(KK) delete --ignore-not-found -f -

.PHONY: e2e
e2e: ## Run the kind end-to-end suite (needs kind-up, deploy-kind, upload-samples).
	KUBE_CONTEXT=$(KIND_CONTEXT) HEY_IMG=$(HEY_IMG) test/e2e/e2e.sh
