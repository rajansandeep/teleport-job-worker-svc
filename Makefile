MODULE := github.com/rajansandeep/teleport-job-worker-svc
PROTO_DIR := proto

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

.PHONY: test
test:
	go test -race ./...
.PHONY: build-cli
build-cli:
	go build -o ./jobworker-cli ./cmd/jobworker-cli
