MODULE := github.com/rajansandeep/teleport-job-worker-svc
PROTO_DIR := proto

.PHONY: all
all: build-server build-cli certs

.PHONY: proto
proto:
	protoc \
		-I $(PROTO_DIR) \
		--go_out=. \
		--go_opt=module=$(MODULE) \
		--go-grpc_out=. \
		--go-grpc_opt=module=$(MODULE) \
		$(PROTO_DIR)/jobworker/v1/jobworker.proto

.PHONY: certs
certs:
	./scripts/generate-certs.sh certs

.PHONY: build-server
build-server:
	go build -o ./jobworker-server ./cmd/jobworker-server

.PHONY: build-cli
build-cli:
	go build -o ./jobworker-cli ./cmd/jobworker-cli

.PHONY: test
test:
	go test -race ./...

.PHONY: test-unit
test-unit:
	go test -race ./internal/...

.PHONY: test-integration
test-integration:
	go test -race ./integration/...

.PHONY: e2e
e2e: build-server build-cli certs
	./scripts/e2e-all.sh
