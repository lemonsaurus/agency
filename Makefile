.PHONY: build install install-claudejail install-claudejail-mac uninstall run clean test lint

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

install-claudejail:
	mkdir -p $(INSTALL_DIR)
	cp scripts/claudejail $(INSTALL_DIR)/
	chmod +x $(INSTALL_DIR)/claudejail
	@echo "Installed claudejail to $(INSTALL_DIR)/claudejail"

install-claudejail-mac:
	mkdir -p $(INSTALL_DIR)
	cp scripts/claudejail-mac $(INSTALL_DIR)/
	chmod +x $(INSTALL_DIR)/claudejail-mac
	@echo "Installed claudejail-mac to $(INSTALL_DIR)/claudejail-mac"
	@echo "First run will build the Docker image automatically (~1 min)"

uninstall:
	rm -f $(INSTALL_DIR)/agency
	rm -f $(INSTALL_DIR)/agency-spawn
	rm -f $(INSTALL_DIR)/claudejail
	rm -f $(INSTALL_DIR)/claudejail-mac
	@echo "Uninstalled agency from $(INSTALL_DIR)"

run: build
	./bin/$(BINARY)

test:
	$(GO) test ./...

lint:
	$(GO) vet ./...

clean:
	rm -rf bin/
