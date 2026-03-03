.PHONY: build install run clean test lint

BINARY := agency
INSTALL_DIR := $(HOME)/.local/bin
GO := $(shell which go 2>/dev/null || echo /usr/local/go/bin/go)

build:
	mkdir -p bin
	$(GO) build -o bin/$(BINARY) ./cmd/agency

install: build
	mkdir -p $(INSTALL_DIR)
	cp bin/$(BINARY) $(INSTALL_DIR)/
	cp scripts/agency-spawn $(INSTALL_DIR)/
	chmod +x $(INSTALL_DIR)/agency-spawn
	@echo "Installed to $(INSTALL_DIR)/agency and $(INSTALL_DIR)/agency-spawn"

run: build
	./bin/$(BINARY)

test:
	$(GO) test ./...

lint:
	$(GO) vet ./...

clean:
	rm -rf bin/
