GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)

.PHONY: all
all: gen build test

# Code generation. gen/ (including gen/openapi/arena.v1.yaml) is a build
# artifact; CI checks freshness with `make gen-check`.
.PHONY: gen
gen: tools
	buf lint
	buf generate

.PHONY: gen-check
gen-check: gen
	git diff --exit-code gen/

.PHONY: tools
tools:
	@command -v $(GOBIN)/buf >/dev/null || go install github.com/bufbuild/buf/cmd/buf@latest
	@command -v $(GOBIN)/protoc-gen-go >/dev/null || go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	@command -v $(GOBIN)/protoc-gen-connect-go >/dev/null || go install connectrpc.com/connect/cmd/protoc-gen-connect-go@latest
	@command -v $(GOBIN)/protoc-gen-connect-openapi >/dev/null || go install github.com/sudorandom/protoc-gen-connect-openapi@latest

.PHONY: build
build:
	go build ./...

.PHONY: test
test:
	go test ./...

# compose.yaml: DynamoDB Local + Valkey + floci for running arena locally.
.PHONY: compose-up
compose-up:
	docker compose up -d --wait

.PHONY: compose-down
compose-down:
	docker compose down -v

# Integration tests start their own containers via testcontainers (no
# compose needed); they are build-tagged so `make test` never needs docker.
.PHONY: test-integration
test-integration:
	go test -tags integration -count=1 -race -timeout 10m ./test/integration/...

.PHONY: tf-validate
tf-validate:
	terraform -chdir=deploy/terraform init -backend=false -input=false >/dev/null
	terraform -chdir=deploy/terraform validate
