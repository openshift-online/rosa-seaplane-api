.PHONY: help build test test-unit test-integration lint clean \
	build-seaplane-db build-operator build-api build-api-codegen \
	test-seaplane-db test-operator test-operator-int test-api test-api-int test-api-codegen test-clientset \
	test-e2e test-e2e-api test-e2e-authz test-e2e-cli test-e2e-platform-monitoring test-e2e-zoa test-e2e-sdk test-e2e-rosa-cli \
	test-e2e test-e2e-api test-e2e-cli test-e2e-platform-monitoring test-e2e-sdk \
	fmt vet verify verify-mod deps mod-tidy \
	manifests generate generate-deepcopy generate-clientset verify-clientset setup-envtest \
	codegen-passthrough codegen-registry codegen-verify codegen verify-codegen \
    codegen-passthrough-clobber \
	codegen-conversion verify-conversion \
	generate-openapi verify-openapi swagger-ui \
	image-api image-operator image-push-api image-push-operator

# ── Configuration ────────────────────────────────────────────────────────

IMAGE_REPO_API      ?= quay.io/openshift-online/rosa-seaplane-api
IMAGE_REPO_OPERATOR ?= quay.io/openshift-online/seaplane-operator
IMAGE_TAG           ?= latest
GIT_SHA             := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
GOOS                ?= linux
GOARCH              ?= amd64
PLATFORMS           ?= linux/amd64,linux/arm64

TEST_OUTPUT_DIR     ?= $(or $(ARTIFACT_DIR),./test-results)

AWS_PROFILE ?=
AWS_REGION  ?=
FOCUS       ?=

ROSA_REPO_URL          ?= https://github.com/openshift/rosa
ROSA_REPO_BRANCH       ?= seaplane-v2
ROSA_MAKE_TARGET       ?= e2e-seaplane
ROSA_BUILD_TARGET      ?= install
ROSA_FOCUS      ?=
ROSA_SKIP       ?=
ROSA_LABEL_FILTER ?=

CONTAINER_ENGINE ?= $(shell command -v podman 2>/dev/null || command -v docker 2>/dev/null)

TOOLS_DIR        := ./hack/tools
TOOLS_BIN_DIR    := $(TOOLS_DIR)/bin
GOLANGCI_LINT    := $(abspath $(TOOLS_BIN_DIR)/golangci-lint)
CONTROLLER_GEN   := $(abspath $(TOOLS_BIN_DIR)/controller-gen)
CLIENT_GEN       := $(abspath $(TOOLS_BIN_DIR)/client-gen)
BRIDGE_GEN         := $(abspath $(TOOLS_BIN_DIR)/bridge-gen)
PATHBIND_GEN       := $(abspath $(TOOLS_BIN_DIR)/pathbind-gen)
SETUP_ENVTEST    := $(abspath $(TOOLS_BIN_DIR)/setup-envtest)
GINKGO           := $(abspath $(TOOLS_BIN_DIR)/ginkgo)
COVERAGE         ?=

# ── SDK generation ───────────────────────────────────────────────────────
SDK_MODULE        ?= github.com/openshift-online/rosa-seaplane-api
SDK_API_PKG       ?= $(SDK_MODULE)/api
SDK_INPUT         ?= v1alpha1/public
SDK_CLIENTSET     ?= generated
SDK_OUTPUT_DIR    ?= $(abspath clientset)
SDK_OUTPUT_PKG    ?= $(SDK_MODULE)/clientset
BRIDGE_INPUT_DIR        ?= $(abspath api/v1alpha1/public)
BRIDGE_OUTPUT_DIR       ?= $(abspath clientset/transport)
BRIDGE_OUTPUT_PKG       ?= transport
PLATFORM_OUTPUT_DIR   ?= $(abspath clientset/platform)
PLATFORM_OUTPUT_PKG   ?= platform
TYPED_PKG_IMPORT      ?= $(SDK_MODULE)/clientset/generated/typed/v1alpha1/public
API_PKG_IMPORT        ?= $(SDK_MODULE)/api/v1alpha1/public
SDK_HEADER_FILE       ?= $(abspath hack/clientset/license-boilerplate.go.txt)

# ── Code generation ───────────────────────────────────────────────────────
OPENAPI_GENERATED ?= api/v1alpha1/public/generated-schemas.json
OPENAPI_SPEC      ?= api/v1alpha1/public/openapi.yaml
SWAGGER_UI_PORT ?= 8282

$(GOLANGCI_LINT): $(TOOLS_DIR)/go.mod
	cd $(TOOLS_DIR); go build -tags=tools -o $(abspath $(TOOLS_BIN_DIR))/golangci-lint github.com/golangci/golangci-lint/v2/cmd/golangci-lint

$(CONTROLLER_GEN): $(TOOLS_DIR)/go.mod
	cd $(TOOLS_DIR); go build -tags=tools -o $(abspath $(TOOLS_BIN_DIR))/controller-gen sigs.k8s.io/controller-tools/cmd/controller-gen

$(SETUP_ENVTEST): $(TOOLS_DIR)/go.mod
	cd $(TOOLS_DIR); go build -tags=tools -o $(abspath $(TOOLS_BIN_DIR))/setup-envtest sigs.k8s.io/controller-runtime/tools/setup-envtest

$(CLIENT_GEN): $(TOOLS_DIR)/go.mod
	cd $(TOOLS_DIR); go build -tags=tools -o $(abspath $(TOOLS_BIN_DIR))/client-gen k8s.io/code-generator/cmd/client-gen

$(BRIDGE_GEN): hack/clientset/cmd/bridge-gen/main.go
	cd hack/clientset/cmd/bridge-gen && go build -o $(BRIDGE_GEN) .

$(PATHBIND_GEN): $(wildcard clientset/cmd/pathbind-gen/*.go clientset/cmd/pathbind-gen/templates/*.go.tmpl)
	cd clientset && go build -o $(PATHBIND_GEN) ./cmd/pathbind-gen

$(GINKGO): $(TOOLS_DIR)/go.mod
	cd $(TOOLS_DIR); go build -tags=tools -o $(abspath $(TOOLS_BIN_DIR))/ginkgo github.com/onsi/ginkgo/v2/ginkgo

# ── Help ─────────────────────────────────────────────────────────────────

help:
	@echo "Usage: make <target>"
	@echo ""
	@echo "Build:"
	@echo "  build                Build all components"
	@echo "  build-api            Platform API server"
	@echo "  build-operator       Seaplane operator (manager + compactor)"
	@echo "  build-seaplane-db  Seaplane DB library"
	@echo "  build-api-codegen    API codegen tools (build-time generators)"
	@echo ""
	@echo "Test:"
	@echo "  test                 All tests (unit + integration)"
	@echo "  test-unit            Unit tests: API + operator + codegen + clientset (no external services)"
	@echo "  test-clientset       Clientset unit tests (transport, platform)"
	@echo "  test-integration     Integration tests: seaplane-db + operator (podman) + API handlers"
	@echo "  test-api-int         API handler integration tests (build tag: integration)"
	@echo "  test-e2e-api         E2E API"
	@echo "  test-e2e-authz       Local HTTP authorization (Podman PostgreSQL 16, no AWS credentials)"
	@echo "  test-e2e-cli         E2E CLI"
	@echo "  test-e2e-sdk         E2E SDK (Go clientset lifecycle)"
	@echo "  test-e2e-rosa-cli    E2E rosa CLI (clones rosa repo, builds CLI, runs seaplane tests)"
	@echo "                       Supports ROSA_FOCUS, ROSA_SKIP, ROSA_LABEL_FILTER"
	@echo "  test-e2e-platform-monitoring  E2E monitoring"
	@echo ""
	@echo "Code Quality:"
	@echo "  lint                 golangci-lint on all modules"
	@echo "  fmt                  Format Go source"
	@echo "  vet                  go vet on all modules"
	@echo "  verify-mod           Verify go.mod tidiness"
	@echo ""
	@echo "Code Generation:"
	@echo "  manifests            Generate CRD manifests (controller-gen + CEL strip)"
	@echo "  generate             Run all code generators in one pass"
	@echo "  generate-deepcopy    Generate deepcopy methods only"
	@echo "  verify               Fail if any generated output is out of date"
	@echo "  generate-clientset   Generate typed client SDK from CRD types"
	@echo "  verify-clientset     Fail if generated clientset is out of date"
	@echo "  codegen-passthrough  Generate passthrough types from HyperShift"
	@echo "  codegen-registry     Generate field metadata registry from markers"
	@echo "  codegen-verify       Verify codegen outputs compile"
	@echo "  codegen              Run full codegen pipeline (passthrough + registry + verify)"
	@echo "  verify-codegen       Fail if codegen outputs are out of date"
	@echo "  codegen-conversion   Generate REST types and conversion functions from CRD types"
	@echo "  verify-conversion    Fail if conversion outputs are out of date"
	@echo "  generate-openapi     Generate and merge typed schemas into OpenAPI spec"
	@echo "  verify-openapi       Fail if OpenAPI spec is out of date with codegen"
	@echo "  swagger-ui           Run Swagger UI locally (default port 8282)"
	@echo "  setup-envtest        Install envtest binaries (etcd, kube-apiserver)"
	@echo "  deps                 Download and tidy all modules"
	@echo ""
	@echo "Images:"
	@echo "  image-api            Platform API image"
	@echo "  image-operator       Seaplane operator image"

# ── Build ────────────────────────────────────────────────────────────────

build: build-seaplane-db build-operator build-api

build-seaplane-db:
	cd seaplane-db && go build ./...

build-operator:
	cd seaplane-operator && go build -o ../bin/manager ./cmd/manager
	cd seaplane-operator && go build -o ../bin/compactor ./cmd/compactor

build-api:
	cd seaplane-api && go build -o ../bin/rosa-seaplane-api ./cmd

build-api-codegen:
	cd hack/api-codegen && go build -o ../../bin/passthrough-gen ./cmd/passthrough-gen
	cd hack/api-codegen && go build -o ../../bin/marker-scanner ./cmd/marker-scanner
	cd hack/api-codegen && go build -o ../../bin/openapi-gen ./cmd/openapi-gen
	cd hack/api-codegen && go build -o ../../bin/openapi-merge ./cmd/openapi-merge
	cd hack/api-codegen && go build -o ../../bin/conversion-gen ./cmd/conversion-gen
	cd hack/api-codegen && go build -o ../../bin/crd-variants ./cmd/crd-variants
	cd hack/api-codegen && go build -o ../../bin/featuregate-info ./cmd/featuregate-info
	cd hack/api-codegen && go build -o ../../bin/verify-configuration ./cmd/verify-configuration

# ── Test ─────────────────────────────────────────────────────────────────

test: test-unit test-integration

test-unit: test-api test-operator test-api-codegen test-clientset

test-integration: test-seaplane-db test-operator-int test-api-int

test-api-int:
	cd seaplane-api && go test -v -race -count=1 -tags integration ./pkg/handlers/... ./pkg/authz/...

test-api:
	cd seaplane-api && go test -v -race -count=1 $(if $(COVERAGE),-covermode=atomic -coverprofile=../coverage-seaplane-api.out,) $$(go list ./... | grep -v '/test/e2e')
	$(if $(filter html,$(COVERAGE)),cd seaplane-api && go tool cover -html=../coverage-seaplane-api.out -o ../coverage-seaplane-api.html,)

test-api-codegen:
	cd hack/api-codegen && go test -v -race -count=1 $(if $(COVERAGE),-covermode=atomic -coverprofile=../../coverage-api-codegen.out,) ./...
	$(if $(filter html,$(COVERAGE)),cd hack/api-codegen && go tool cover -html=../../coverage-api-codegen.out -o ../../coverage-api-codegen.html,)

test-clientset:
	cd clientset && go test -v -race -count=1 $(if $(COVERAGE),-covermode=atomic -coverprofile=../coverage-clientset.out,) ./...
	$(if $(filter html,$(COVERAGE)),cd clientset && go tool cover -html=../coverage-clientset.out -o ../coverage-clientset.html,)

test-operator: $(SETUP_ENVTEST)
	@ASSETS=$$($(SETUP_ENVTEST) use -p path --bin-dir $(ENVTEST_BIN_DIR)) && \
		echo "envtest assets: $$ASSETS" && \
		cd seaplane-operator && KUBEBUILDER_ASSETS="$$ASSETS" go test -v -race -count=1 $(if $(COVERAGE),-covermode=atomic -coverprofile=../coverage-seaplane-operator.out,) ./internal/...
	$(if $(filter html,$(COVERAGE)),cd seaplane-operator && go tool cover -html=../coverage-seaplane-operator.out -o ../coverage-seaplane-operator.html,)

test-seaplane-db:
	cd seaplane-db && go test -v -race -count=1 ./...

test-operator-int:
	cd seaplane-operator && go test -v -race -count=1 ./test/...

test-e2e: test-e2e-api

test-e2e-authz: build-api
	cd test && AUTHZ_HTTP_REQUIRED=1 AUTHZ_HTTP_API_BINARY=$(CURDIR)/bin/rosa-seaplane-api go test -count=1 -timeout=10m -run '^TestAuthzHTTP$$' -v ./e2e-api

test-e2e-api: $(GINKGO)
	E2E_BASE_URL="$${BASE_URL}" E2E_ACCOUNT_ID="$${E2E_ACCOUNT_ID}" \
	E2E_RHOBS_API_URL="$${RHOBS_API_URL}" \
	$(GINKGO) -vv \
		$(if $(E2E_LABEL_FILTER),--label-filter="$(E2E_LABEL_FILTER)") \
		--junit-report=junit-api.xml --output-dir=$(TEST_OUTPUT_DIR) \
		./test/e2e-api

test-e2e-cli: $(GINKGO)
	E2E_BASE_URL="$${BASE_URL}" E2E_ACCOUNT_ID="$${E2E_ACCOUNT_ID}" \
	E2E_RHOBS_API_URL="$${RHOBS_API_URL}" \
	E2E_ALERTMANAGER_URL="$${ALERTMANAGER_URL}" \
	ROSACTL_BIN="$${ROSACTL_BIN}" AWS_REGION="$${AWS_REGION}" \
	OCP_IMAGE="$${OCP_IMAGE}" \
	$(GINKGO) -vv --junit-report=junit-cli.xml \
		$(if $(E2E_LABEL_FILTER),--label-filter="$(E2E_LABEL_FILTER)") \
		--output-dir=$(TEST_OUTPUT_DIR) ./test/e2e-cli

test-e2e-platform-monitoring: $(GINKGO)
	E2E_BASE_URL="$${BASE_URL}" E2E_ACCOUNT_ID="$${E2E_ACCOUNT_ID}" \
	E2E_RHOBS_API_URL="$${RHOBS_API_URL}" \
	$(GINKGO) -vv --junit-report=junit-platform-monitoring.xml \
		--output-dir=$(TEST_OUTPUT_DIR) ./test/e2e-platform-monitoring

test-e2e-sdk: $(GINKGO)
	E2E_BASE_URL="$${BASE_URL}" \
	E2E_ACCOUNT_ID="$${E2E_ACCOUNT_ID}" \
	E2E_CUSTOMER_ACCOUNT_ID="$${E2E_CUSTOMER_ACCOUNT_ID}" \
	CUSTOMER_AWS_PROFILE="$${CUSTOMER_AWS_PROFILE}" \
	RC_AWS_PROFILE="$${RC_AWS_PROFILE}" \
	AWS_REGION="$${AWS_REGION}" \
	ROSACTL_BIN="$${ROSACTL_BIN}" \
	OCP_IMAGE="$${OCP_IMAGE}" \
	$(GINKGO) -vv --timeout=3h --junit-report=junit-sdk.xml \
		--output-dir=$(TEST_OUTPUT_DIR) ./test/e2e-sdk

test-e2e-rosa-cli:
	@ROSA_TMPDIR=$$(mktemp -d) && \
	trap "rm -rf $$ROSA_TMPDIR" EXIT && \
	echo "Temporary directory: $$ROSA_TMPDIR" && \
	echo "Cloning rosa repo from $(ROSA_REPO_URL)@$(ROSA_REPO_BRANCH)..." && \
	git clone --depth=1 --branch $(ROSA_REPO_BRANCH) $(ROSA_REPO_URL) $$ROSA_TMPDIR && \
	echo "Building rosa CLI..." && \
	cd $$ROSA_TMPDIR && $(MAKE) $(ROSA_BUILD_TARGET) && \
	export PATH="$$PWD:$$PATH" && \
	echo "Running rosa cli E2E tests..." && \
	name=$${CLUSTER_NAME:-hf-e2e-$$(date +%s)} && \
	export SEAPLANE_URL="$${SEAPLANE_URL}" && \
	export CLUSTER_NAME="$$name" && \
	export OPERATOR_ROLES_PREFIX="$${OPERATOR_ROLES_PREFIX:-$$name}" && \
	export AWS_DEFAULT_REGION="$${AWS_DEFAULT_REGION:-$${AWS_REGION}}" && \
	export GOTOOLCHAIN=auto && \
	export TEST_PROFILE="$${TEST_PROFILE}" && \
	export TEST_PROFILE_DIR="$$ROSA_TMPDIR/tests/ci/data/profiles" && \
	export WORKSPACE="$$ROSA_TMPDIR" && \
	if [ -n "$(ROSA_SKIP)$(ROSA_LABEL_FILTER)" ]; then \
		echo "Running with custom ginkgo filters..." && \
		$(GINKGO) run -v --timeout 3h \
			--junit-report=junit-rosa-cli.xml \
			--output-dir=$(TEST_OUTPUT_DIR) \
			$(if $(ROSA_SKIP),--skip="$(ROSA_SKIP)") \
			$(if $(ROSA_LABEL_FILTER),--label-filter="$(ROSA_LABEL_FILTER)") \
			./tests/e2e/; \
	else \
		echo "Running default rosa e2e-seaplane target..." && \
		$(MAKE) \
			TEST_PROFILE="$${TEST_PROFILE}" \
			TEST_PROFILE_DIR="$$ROSA_TMPDIR/tests/ci/data/profiles" \
			WORKSPACE="$$ROSA_TMPDIR" \
			$(ROSA_MAKE_TARGET); \
	fi


# ── Code Quality ─────────────────────────────────────────────────────────

fmt:
	cd seaplane-db && go fmt ./...
	cd seaplane-operator && go fmt ./...
	cd seaplane-api && go fmt ./...
	cd hack/api-codegen && go fmt ./...
	cd clientset && go fmt ./...
	cd hack/clientset/cmd/bridge-gen && go fmt ./...

vet:
	cd seaplane-db && go vet ./...
	cd seaplane-operator && go vet ./...
	cd seaplane-api && go vet ./...
	cd hack/api-codegen && go vet ./...
	cd clientset && go vet ./...
	cd hack/clientset/cmd/bridge-gen && go vet ./...

lint: $(GOLANGCI_LINT)
	cd seaplane-db && $(GOLANGCI_LINT) run --config ../.golangci.yml --timeout 5m ./...
	cd seaplane-operator && $(GOLANGCI_LINT) run --config ../.golangci.yml --timeout 5m ./...
	cd seaplane-api && $(GOLANGCI_LINT) run --config ../.golangci.yml --timeout 5m ./...
	cd hack/api-codegen && $(GOLANGCI_LINT) run --config ../../.golangci.yml --timeout 5m ./...
	cd clientset && $(GOLANGCI_LINT) run --config ../.golangci.yml --timeout 5m ./...
	cd hack/clientset/cmd/bridge-gen && $(GOLANGCI_LINT) run --config $(abspath .golangci.yml) --timeout 5m ./...

# All Go modules in the repo (used by verify and MintMaker/Renovate post-upgrade).
override MOD_TIDY_DIRS := seaplane-db api seaplane-operator seaplane-api test clientset hack/tools hack/api-codegen
MOD_TIDY_FILES := $(foreach d,$(MOD_TIDY_DIRS),$(d)/go.mod $(d)/go.sum)

mod-tidy:
	@set -e; for d in $(MOD_TIDY_DIRS); do \
		echo "go mod tidy: $$d"; \
		(cd "$$d" && go mod tidy); \
	done

verify-mod: mod-tidy
	git diff --exit-code $(MOD_TIDY_FILES)

deps:
	@set -e; for d in $(MOD_TIDY_DIRS); do \
		(cd "$$d" && go mod download && go mod tidy); \
	done

# ── Code Generation ──────────────────────────────────────────────────────

CRD_VARIANTS     := $(abspath bin/crd-variants)
CRD_BASES_DIR    := seaplane-operator/config/crd/bases

manifests: codegen-passthrough codegen-conversion $(CONTROLLER_GEN) build-api-codegen
	cd seaplane-operator && $(CONTROLLER_GEN) crd:allowDangerousTypes=true paths="../api/v1alpha1" output:crd:dir=config/crd/bases
	$(CRD_VARIANTS) --strip-passthrough-cel --api-dir api/v1alpha1 --crd-dir $(CRD_BASES_DIR)

generate-deepcopy: codegen-passthrough $(CONTROLLER_GEN)
	$(CONTROLLER_GEN) object paths="./api/..."

generate-clientset: codegen-conversion $(CLIENT_GEN) $(BRIDGE_GEN)
	cd api && $(CLIENT_GEN) \
		--input-base "$(SDK_API_PKG)" \
		--input "$(SDK_INPUT)" \
		--clientset-name "$(SDK_CLIENTSET)" \
		--output-dir "$(SDK_OUTPUT_DIR)" \
		--output-pkg "$(SDK_OUTPUT_PKG)" \
		--go-header-file "$(SDK_HEADER_FILE)"
	$(BRIDGE_GEN) \
		--mode platform \
		--input-dir "$(BRIDGE_INPUT_DIR)" \
		--output-dir "$(PLATFORM_OUTPUT_DIR)" \
		--output-pkg "$(PLATFORM_OUTPUT_PKG)" \
		--typed-pkg-import "$(TYPED_PKG_IMPORT)" \
		--typed-client-prefix "V1alpha1Public" \
		--api-pkg-import "$(API_PKG_IMPORT)" \
		--go-header-file "$(SDK_HEADER_FILE)"

verify-clientset: generate-clientset
	git diff --exit-code -- clientset/generated/ clientset/platform/

codegen-passthrough: codegen-registry
	cd api && ../bin/passthrough-gen \
		-import-path github.com/openshift/hypershift/api/hypershift/v1beta1 \
		-types HostedClusterSpec,NodePoolSpec \
		-output-dir v1alpha1 \
		-package v1alpha1 \
		-registry ../hack/api-codegen/pkg/registry/field_metadata.json
	rm -f api/v1alpha1/zz_generated.passthrough.go.raw
	$(MAKE) codegen-registry

codegen-passthrough-clobber:
	rm -f api/v1alpha1/zz_generated.passthrough.go
	cd api && ../bin/passthrough-gen \
  		-import-path github.com/openshift/hypershift/api/hypershift/v1beta1 \
  		-types HostedClusterSpec,NodePoolSpec \
  		-output-dir v1alpha1 \
  		-package v1alpha1 \
		-registry ../hack/api-codegen/pkg/registry/field_metadata.json

codegen-registry: build-api-codegen
	./bin/marker-scanner \
		-input-dirs api/v1alpha1 \
		-output-file hack/api-codegen/pkg/registry/field_metadata.go \
		$(if $(VERBOSE),-verbose)

codegen-verify: codegen-registry
	cd api && go build ./...
	cd seaplane-api && go build ./...

codegen: codegen-verify

verify-codegen: codegen
	git diff --exit-code api/v1alpha1/zz_generated.deepcopy.go
	git diff --exit-code hack/api-codegen/pkg/registry/

# generate runs all code generators in dependency order.
# manifests and generate-deepcopy depend on codegen-passthrough so regenerated
# passthrough types complete before downstream generators run, including -j.
generate-pathbind-draft: $(PATHBIND_GEN) codegen-registry generate-openapi
	$(PATHBIND_GEN) \
		--mode=init \
		--registry=hack/api-codegen/pkg/registry/field_metadata.json \
		--openapi=api/v1alpha1/public/openapi.yaml \
		--output=clientset/pathbind/pathbind-draft.yaml

# codegen-passthrough must complete before conversion-gen, manifests, and
# generate-deepcopy so all downstream generators see the regenerated markers.
generate: codegen-passthrough codegen-conversion generate-deepcopy manifests generate-clientset generate-openapi generate-pathbind-draft

verify-pathbind-draft: generate-pathbind-draft
	git diff --exit-code clientset/pathbind/pathbind-draft.yaml

verify: verify-codegen verify-conversion verify-clientset verify-openapi verify-pathbind-draft verify-mod

CONVERSION_OUTPUT_DIR   ?= seaplane-api/pkg/conversion/v1alpha1
CONVERSION_OUTPUT_PKG   ?= github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/conversion
CONVERSION_CRD_PKG      ?= github.com/openshift-online/rosa-seaplane-api/api/v1alpha1
CONVERSION_REST_DIR     ?= api/v1alpha1/public
CONVERSION_REST_PKG     ?= github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public

codegen-conversion: codegen-passthrough codegen-registry build-api-codegen
	./bin/conversion-gen \
		--api-version=v1alpha1 \
		--crd-package=$(CONVERSION_CRD_PKG) \
		--input-dirs=./api/v1alpha1 \
		--output-dir=$(CONVERSION_OUTPUT_DIR) \
		--output-package=$(CONVERSION_OUTPUT_PKG) \
		--rest-output-dir=$(CONVERSION_REST_DIR) \
		--rest-package=$(CONVERSION_REST_PKG)

verify-conversion: codegen-conversion
	cd api && go build ./...
	cd seaplane-api && go build ./...
	git diff --exit-code $(CONVERSION_REST_DIR)/ $(CONVERSION_OUTPUT_DIR)/ seaplane-api/pkg/conversion/types.go

generate-openapi: codegen-conversion
	cd hack/api-codegen && go build -o ../../bin/openapi-gen ./cmd/openapi-gen
	./bin/openapi-gen \
		-input-dirs ./api/v1alpha1/public \
		-output-file $(OPENAPI_GENERATED)
	# TODO: auto-discover schemas via +seaplane:public-api=true marker so new CRDs
	# are included automatically — requires extending openapi-merge to scan
	# generated-schemas.json description fields for the marker text.
	./bin/openapi-merge \
		-spec $(OPENAPI_SPEC) \
		-generated $(OPENAPI_GENERATED) \
		-schemas Cluster,ClusterList,ClusterSpec,ClusterStatus,NodePool,NodePoolList,NodePoolSpec,NodePoolStatus,OidcConfig,OidcConfigList,OidcConfigStatus,DNSReservation,DNSReservationList,DNSReservationSpec,DNSReservationStatus,PlacementReference,Capabilities,HostedClusterSpecPassthrough,NodePoolSpecPassthrough,ClusterConfiguration,MachineConfigSpec,PlatformSpec,NodePoolPlatform

verify-openapi: generate-openapi
	git diff --exit-code $(OPENAPI_SPEC)

swagger-ui:
	@echo "Swagger UI available at http://localhost:$(SWAGGER_UI_PORT)"
	$(CONTAINER_ENGINE) run --rm -p $(SWAGGER_UI_PORT):8080 \
		-e SWAGGER_JSON=/spec/openapi.yaml \
		-v $(CURDIR)/$(OPENAPI_SPEC):/spec/openapi.yaml:ro \
		swaggerapi/swagger-ui

ENVTEST_BIN_DIR ?= $(shell pwd)/.envtest

setup-envtest: $(SETUP_ENVTEST)
	$(SETUP_ENVTEST) use --bin-dir $(ENVTEST_BIN_DIR)

# ── Images ───────────────────────────────────────────────────────────────

image-api:
	$(CONTAINER_ENGINE) build -f seaplane-api/Containerfile \
		--platform $(GOOS)/$(GOARCH) \
		-t $(IMAGE_REPO_API):$(IMAGE_TAG) .
	$(CONTAINER_ENGINE) tag $(IMAGE_REPO_API):$(IMAGE_TAG) $(IMAGE_REPO_API):$(GIT_SHA)

image-operator:
	$(CONTAINER_ENGINE) build -f seaplane-operator/Containerfile \
		--platform $(GOOS)/$(GOARCH) \
		-t $(IMAGE_REPO_OPERATOR):$(IMAGE_TAG) .
	$(CONTAINER_ENGINE) tag $(IMAGE_REPO_OPERATOR):$(IMAGE_TAG) $(IMAGE_REPO_OPERATOR):$(GIT_SHA)

image-push-api: image-api
	$(CONTAINER_ENGINE) push $(IMAGE_REPO_API):$(IMAGE_TAG)
	$(CONTAINER_ENGINE) push $(IMAGE_REPO_API):$(GIT_SHA)

image-push-operator: image-operator
	$(CONTAINER_ENGINE) push $(IMAGE_REPO_OPERATOR):$(IMAGE_TAG)
	$(CONTAINER_ENGINE) push $(IMAGE_REPO_OPERATOR):$(GIT_SHA)

# ── Clean ────────────────────────────────────────────────────────────────

clean:
	rm -rf bin/
	rm -f coverage.out coverage.html coverage-*.out coverage-*.html
	rm -rf test-results/

clean-test-containers:
	@$(CONTAINER_ENGINE) ps -a --format "{{.Names}}" \
	  | grep -E "^pgctl-" \
	  | xargs -r $(CONTAINER_ENGINE) rm -f \
	  && echo "test containers removed" || true

# ── All ──────────────────────────────────────────────────────────────────

all: deps fmt vet lint test build
