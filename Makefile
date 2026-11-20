# tobii4c — build e installazione
BIN      ?= bin/tobii4c
PREFIX   ?= $(HOME)/.local
GOFLAGS  ?= -trimpath
SYSTEMD_USER_DIR ?= $(HOME)/.config/systemd/user

.PHONY: all build build-lsl test bench race install install-lsl uninstall \
	install-service uninstall-service install-udev uninstall-udev clean

all: build

build:
	go build $(GOFLAGS) -o $(BIN) ./cmd/tobii4c

# Richiede liblsl (header lsl_c.h e liblsl.so): https://github.com/sccn/liblsl/releases
build-lsl:
	go build $(GOFLAGS) -tags lsl -o $(BIN) ./cmd/tobii4c

test:
	go vet ./...
	go test ./...

bench:
	go test -run '^$$' -bench . -benchmem ./internal/...

race:
	go build -race -o bin/tobii4c-race ./cmd/tobii4c

install: build
	install -Dm755 $(BIN) $(PREFIX)/bin/tobii4c

# Come install, ma con il supporto LSL (`install` ricompila sempre senza).
install-lsl: build-lsl
	install -Dm755 $(BIN) $(PREFIX)/bin/tobii4c

uninstall:
	rm -f $(PREFIX)/bin/tobii4c

# Unit utente di systemd, con il percorso del binario di $(PREFIX).
# Non la abilita: `systemctl --user enable --now tobii4c`.
install-service:
	install -d $(SYSTEMD_USER_DIR)
	sed 's#%h/.local/bin/tobii4c#$(PREFIX)/bin/tobii4c#' systemd/tobii4c.service > $(SYSTEMD_USER_DIR)/tobii4c.service
	systemctl --user daemon-reload
	@echo "installata $(SYSTEMD_USER_DIR)/tobii4c.service: avviala con  systemctl --user enable --now tobii4c"

uninstall-service:
	-systemctl --user disable --now tobii4c
	rm -f $(SYSTEMD_USER_DIR)/tobii4c.service
	systemctl --user daemon-reload

install-udev:
	sudo install -m 0644 udev/70-tobii-4c.rules /etc/udev/rules.d/70-tobii-4c.rules
	sudo udevadm control --reload-rules
	sudo udevadm trigger --action=add --subsystem-match=usb --attr-match=idVendor=2104

uninstall-udev:
	sudo rm -f /etc/udev/rules.d/70-tobii-4c.rules
	sudo udevadm control --reload-rules

clean:
	rm -rf bin
