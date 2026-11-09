# tobii4c — build e installazione
BIN      ?= bin/tobii4c
PREFIX   ?= $(HOME)/.local
GOFLAGS  ?= -trimpath

.PHONY: all build build-lsl webcam-face webcam-body shaders test bench race install install-lsl install-udev uninstall-udev clean

all: build

build:
	go build $(GOFLAGS) -o $(BIN) ./cmd/tobii4c

# Richiede liblsl (header lsl_c.h e liblsl.so): https://github.com/sccn/liblsl/releases
build-lsl:
	go build $(GOFLAGS) -tags lsl -o $(BIN) ./cmd/tobii4c

# Esempio: il modello del viso del driver sulle immagini di una webcam.
webcam-face:
	go build $(GOFLAGS) -o bin/webcam-face ./examples/webcam-face

# Esempio: lo scheletro (corpo intero o mezzo busto) con MediaPipe Pose sulla GPU.
webcam-body:
	go build $(GOFLAGS) -o bin/webcam-body ./examples/webcam-body

# Ricompila gli shader Vulkan di internal/tflite (serve glslc, pacchetto shaderc).
shaders:
	go generate ./internal/tflite

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

install-udev:
	sudo install -m 0644 udev/70-tobii-4c.rules /etc/udev/rules.d/70-tobii-4c.rules
	sudo udevadm control --reload-rules
	sudo udevadm trigger --action=add --subsystem-match=usb --attr-match=idVendor=2104

uninstall-udev:
	sudo rm -f /etc/udev/rules.d/70-tobii-4c.rules
	sudo udevadm control --reload-rules

clean:
	rm -rf bin
