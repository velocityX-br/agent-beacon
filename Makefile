BINARY := agent-beacon
PKG := github.com/local/agent-beacon
BIN_DIR := bin
DIST_DIR := dist

# Version stamped into the binary (main.version). Falls back to git describe.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE ?= agent-beacon:$(VERSION)

GOFLAGS := -trimpath
LDFLAGS := -s -w -X main.version=$(VERSION)

# Cross-compile matrix (os/arch pairs).
PLATFORMS := \
	linux/amd64 \
	linux/arm64 \
	darwin/amd64 \
	darwin/arm64 \
	windows/amd64

.PHONY: all build vet test run-server clean tidy dist docker docker-run

all: build

build:
	@mkdir -p $(BIN_DIR)
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/agent-beacon

vet:
	go vet ./...

test:
	go test ./...

tidy:
	go mod tidy

run-server: build
	$(BIN_DIR)/$(BINARY) server --address :8080

# Cross-compile all platforms into dist/. Windows gets a .exe suffix.
dist:
	@mkdir -p $(DIST_DIR)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		out=$(DIST_DIR)/$(BINARY)-$${os}-$${arch}$${ext}; \
		echo "-> $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $$out ./cmd/agent-beacon || exit 1; \
	done

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

docker-run: docker
	docker run --rm -p 8080:8080 $(IMAGE)

clean:
	rm -rf $(BIN_DIR) $(DIST_DIR)
