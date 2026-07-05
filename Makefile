APP          := mutirec
PKG          := ./cmd/web
BASE_VERSION := $(shell cat VERSION 2>/dev/null || echo 0.0.0)
COMMIT       := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DIRTY        := $(shell git diff --quiet 2>/dev/null || echo -dirty)
VERSION      := v$(BASE_VERSION)+$(COMMIT)$(DIRTY)
LDFLAGS      := -s -w -X main.version=$(VERSION)

.DEFAULT_GOAL := build

## build: compile the web recorder binary
.PHONY: build
build:
	go build -ldflags "$(LDFLAGS)" -o $(APP) $(PKG)

## run: run the web recorder locally
.PHONY: run
run: build
	./$(APP)

## test: run unit tests
.PHONY: test
test:
	go test ./...

## vet: static analysis
.PHONY: vet
vet:
	go vet ./...

## fmt: format sources
.PHONY: fmt
fmt:
	gofmt -s -w .

## tidy: ensure dependencies are clean
.PHONY: tidy
tidy:
	go mod tidy

## check: fmt + vet + test
.PHONY: check
check: fmt vet test

## docker: build the container image
.PHONY: docker
docker:
	docker build --build-arg VERSION=$(VERSION) -t $(APP):$(VERSION) -t $(APP):latest .

## compose-up: start the local Docker Compose stack
.PHONY: compose-up
compose-up:
	docker compose up -d

## compose-down: stop the local Docker Compose stack
.PHONY: compose-down
compose-down:
	docker compose down

## clean: remove build artifacts
.PHONY: clean
clean:
	rm -rf $(APP) bin dist coverage.txt

.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //; s/:/ -> /'

## version: print the computed version string (v<base>+<commit>[-dirty])
.PHONY: version
version:
	@echo $(VERSION)
