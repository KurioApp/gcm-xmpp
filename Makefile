SOURCES := $(shell find . -name '*.go' -type f -not -path './vendor/*'  -not -path '*/mocks/*')
PACKAGES := $(shell go list ./... | grep -v /vendor/)

PACKAGE := $(shell go list)
GOOS := $(shell go env GOOS)
GOARCH = $(shell go env GOARCH)
OBJ_DIR := $(GOPATH)/pkg/$(GOOS)_$(GOARCH)/$(PACKAGE)

# Dependencies Management
.PHONY: vendor-prepare
vendor-prepare:
	@echo "Installing dep"
	@go get -u -v github.com/golang/dep/cmd/dep

Gopkg.lock: Gopkg.toml
	@dep ensure -update

.PHONY: vendor-update
vendor-update:
	@dep ensure -update

vendor: Gopkg.lock
	@dep ensure

.PHONY: vendor-optimize
vendor-optimize: vendor
	@dep prune

.PHONY: clean-vendor
clean-vendor:
	@rm -rf vendor

# Linter
.PHONY: lint-prepare
lint-prepare:
	@echo "Installing gometalinter"
	@go get -u github.com/alecthomas/gometalinter
	@gometalinter --install

.PHONY: lint
lint: vendor
	@gometalinter --cyclo-over=20 --deadline=2m $(PACKAGES)

# Testing
.PHONY: test
test: vendor
	@go test -short $(PACKAGES)

# Build and Installation
.PHONY: install
install: vendor
	@go install $(PACKAGES)

.PHONY: uninstall
uninstall:
	@echo "Removing binaries and libraries"
	@go clean -i $(PACKAGES)
	@if [ -d $(OBJ_DIR) ]; then \
		rm -rf $(OBJ_DIR); \
	fi
