.PHONY: build install install-claudejail run clean test lint

BINARY := agency
INSTALL_DIR := $(HOME)/.local/bin
FIREJAIL_PROFILE_DIR := $(HOME)/.config/firejail
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

install-claudejail:
	mkdir -p $(INSTALL_DIR) $(FIREJAIL_PROFILE_DIR)
	cp scripts/claudejail $(INSTALL_DIR)/
	chmod +x $(INSTALL_DIR)/claudejail
	cp scripts/claudejail.profile $(FIREJAIL_PROFILE_DIR)/
	@echo "Installed claudejail to $(INSTALL_DIR)/claudejail"
	@echo "Installed firejail profile to $(FIREJAIL_PROFILE_DIR)/claudejail.profile"

run: build
	./bin/$(BINARY)

test:
	$(GO) test ./...

lint:
	$(GO) vet ./...

clean:
	rm -rf bin/
