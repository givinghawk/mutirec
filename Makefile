APP      := defqon-stream-recorder
PKG      := ./cmd/web
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)

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
	docker build -t $(APP):$(VERSION) -t $(APP):latest .

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
