OS ?= $(shell go env GOOS)
ARCH ?= $(shell go env GOARCH)

BUILD_DATE ?= $(shell date -Is)

SHA ?= $(shell git rev-parse HEAD)
TAG ?= $(SHA)
COMMIT_SHA ?= $(SHA)

LDFLAGS := -X github.com/iliaditalia/scw-l2announce-lb-controller/l2lb.version=$(TAG) \
           -X github.com/iliaditalia/scw-l2announce-lb-controller/l2lb.gitCommit=$(COMMIT_SHA) \
           -X github.com/iliaditalia/scw-l2announce-lb-controller/l2lb.buildDate=$(BUILD_DATE)

.PHONY: default
default: test compile

.PHONY: clean
clean:
	go clean -i -x ./...

.PHONY: test
test:
	go test -timeout=1m -v -race -short ./...

.PHONY: fmt
fmt:
	find . -type f -name "*.go" | xargs gofmt -s -w -l

.PHONY: compile
compile:
	go build -v -ldflags "$(LDFLAGS)" -o scw-l2announce-lb-controller ./cmd/scw-l2announce-lb-controller
