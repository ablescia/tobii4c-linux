# tobii4c — build e installazione
BIN      ?= bin/tobii4c
PREFIX   ?= $(HOME)/.local
GOFLAGS  ?= -trimpath
# Dati installati (modello del viso per il pitch della testa)
DATADIR  ?= $(if $(XDG_DATA_HOME),$(XDG_DATA_HOME),$(HOME)/.local/share)/tobii4c
FACE_MODEL_URL    = https://huggingface.co/senty-au/face_landmarks_detector-ONNX/resolve/main/onnx/model.onnx
FACE_MODEL_SHA256 = 7d6e82dee82a1dca5fbddb282b3cc74571833a530de317fc22ae325c3358beeb

.PHONY: all build build-lsl test bench race install install-udev uninstall-udev headpose-model clean

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

# Modello MediaPipe Face Landmarker (rete dei landmark, Apache 2.0, © Google)
# convertito in ONNX: serve al pitch della testa (tobii4c run -headpose).
# Lo esegue l'interprete in Go puro del driver: nessuna libreria nativa.
headpose-model:
	@mkdir -p $(DATADIR)
	curl -fL --retry 2 -o $(DATADIR)/face_landmarks_detector.onnx.part $(FACE_MODEL_URL)
	echo "$(FACE_MODEL_SHA256)  $(DATADIR)/face_landmarks_detector.onnx.part" | sha256sum -c -
	mv $(DATADIR)/face_landmarks_detector.onnx.part $(DATADIR)/face_landmarks_detector.onnx
	@echo "installato: $(DATADIR)/face_landmarks_detector.onnx"

install-udev:
	sudo install -m 0644 udev/70-tobii-4c.rules /etc/udev/rules.d/70-tobii-4c.rules
	sudo udevadm control --reload-rules
	sudo udevadm trigger --action=add --subsystem-match=usb --attr-match=idVendor=2104

uninstall-udev:
	sudo rm -f /etc/udev/rules.d/70-tobii-4c.rules
	sudo udevadm control --reload-rules

clean:
	rm -rf bin
