# smsd — native Linux SMS-over-adb tray daemon
#
# Common targets:
#   make            build the binary into ./bin/smsd
#   make test       run unit tests
#   make run        build and run in the foreground
#   make install    install binary + systemd unit into $(PREFIX)
#   make clean      remove build artifacts

APP        := smsd
PKG        := ./cmd/smsd
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PREFIX     ?= /usr/local
BINDIR     := $(PREFIX)/bin
# systemd user units live under the data dir, not /etc.
UNITDIR    ?= $(PREFIX)/lib/systemd/user

# Static, cgo-free build for a single portable native binary.
export CGO_ENABLED := 0
GOFLAGS    ?=
LDFLAGS    := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet run install uninstall clean tidy

all: build

build:
	@mkdir -p bin
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(APP) $(PKG)
	@echo "built bin/$(APP) ($(VERSION))"

test:
	go test ./...

vet:
	go vet ./...

run: build
	./bin/$(APP)

tidy:
	go mod tidy

# DESTDIR is honoured for packaging (PKGBUILD sets it).
install: build
	install -Dm755 bin/$(APP) $(DESTDIR)$(BINDIR)/$(APP)
	install -Dm644 systemd/smsd.service $(DESTDIR)$(UNITDIR)/smsd.service
	@echo "installed. enable with: systemctl --user enable --now smsd.service"

uninstall:
	rm -f $(DESTDIR)$(BINDIR)/$(APP)
	rm -f $(DESTDIR)$(UNITDIR)/smsd.service

clean:
	rm -rf bin
