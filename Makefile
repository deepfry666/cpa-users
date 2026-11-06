# Builds the plugin as a C-ABI dynamic library for CLIProxyAPI.
# The output file name is the plugin ID, and CPA looks for it under
# plugins/<goos>/<goarch>/ relative to its working directory.

GOOS   ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
OUT    ?= dist

ifeq ($(GOOS),windows)
EXT := dll
else ifeq ($(GOOS),darwin)
EXT := dylib
else
EXT := so
endif

# Pin a conservative CPU baseline so prebuilt libraries do not SIGILL on older hosts.
ifeq ($(GOARCH),amd64)
export GOAMD64 ?= v1
export CGO_CFLAGS ?= -march=x86-64
else ifeq ($(GOARCH),arm64)
export GOARM64 ?= v8.0
export CGO_CFLAGS ?= -march=armv8-a
endif

TARGET := $(OUT)/plugins/$(GOOS)/$(GOARCH)/cpa-users.$(EXT)

.PHONY: build test clean

build:
	@mkdir -p $(dir $(TARGET))
	CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -buildmode=c-shared -ldflags "-s -w" -o $(TARGET) .
	@rm -f $(basename $(TARGET)).h
	@echo "built $(TARGET)"

test:
	go test -count=1 ./...

clean:
	rm -rf $(OUT)
