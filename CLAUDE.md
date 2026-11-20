# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

User-space driver (pure Go, no kernel module, no proprietary Tobii binaries) for the **Tobii Eye Tracker 4C** (`2104:0127`, IS4 generation). It speaks the reconstructed TTP protocol directly over the USB bulk endpoints via `google/gousb` (libusb-1.0), extracts gaze/eye/pupil/head data at 90 Hz, and forwards it over local UDP and/or Lab Streaming Layer. Head pitch (not derivable from the eyes alone) is estimated from the sensor's IR images with a MediaPipe face-landmark model run by a pure-Go ONNX interpreter. The scope is deliberately narrow: streaming for other programs, gaze calibration, head-pose camera calibration, systemd user service.

## Language conventions

The README, commit messages, and all code comments are written in **Italian** (the README opens with a short English abstract). Match this: write new comments and commit messages in Italian, keep Go identifiers and slog messages in English as is standard. README verification tables cite real measurements with dates — do not alter measured figures unless re-measured.

## Commands

```sh
make build            # go build -trimpath -o bin/tobii4c ./cmd/tobii4c
make build-lsl        # same, with -tags lsl (needs liblsl: lsl_c.h + liblsl.so)
make test             # go vet ./... && go test ./...  (no device required)
make bench            # go test -bench . -benchmem ./internal/...
make race             # go build -race -o bin/tobii4c-race
make install          # → $PREFIX/bin (default ~/.local); install-lsl builds with LSL; uninstall
make install-service  # systemd/tobii4c.service → ~/.config/systemd/user, binary path from $PREFIX; uninstall-service
make install-udev / uninstall-udev
```

Single test / package:
```sh
go test ./internal/ttp/ -run TestName -v
go test -race ./internal/calib/      # calib flow test uses the simulator, runs under -race
go test -run '^$' -bench . -benchmem ./internal/nn/
```

Go ≥ 1.22. The LSL sink (`internal/sink/lsl*.go`) and its `lsl-recv` command are behind the `lsl` build tag and use cgo; without the tag `lsl_stub.go` is compiled. The nn package has AVX2/FMA assembly (`simd_amd64.s`) with a pure-Go fallback (`simd_other.go`).

## Running / debugging on the device

The driver needs exclusive USB access, so **stop the systemd user service first** (`systemctl --user stop tobii4c`) or device commands fail with `device or resource busy`. Other user services can hold the tracker too (e.g. a `tobii-ws` bridge that spawns `tobii4c run`). USB access without root comes from the udev rule (`uaccess` ACL requires an *active local session*, not SSH-only). Subcommands (all in `cmd/tobii4c/`): `run`, `calibrate`, `headpose-calibrate`, `calibration save|apply|restore`, `display`, `info`, `recv` (test UDP receiver), `lsl-recv`, `udev`. `calibrate -simulate` runs the calibration UI without hardware (use Xvfb for headless).

The 4C forgets display area and calibration when unpowered, so `run -display-mm saved -calibration saved` (used by the systemd unit) rewrites them from `~/.config/tobii4c` on every connection; with `saved`, a missing file is a logged warning, not an error.

Unit tests never touch the device; they replay real ET5 USB captures in `internal/ttp/testdata/` (MIT-licensed) and assert bit-exact decode against Stream Engine output.

## Architecture

The core is a lock-free concurrent pipeline in `internal/tracker`, one goroutine per stage connected by channels, designed for zero allocations and zero GC during steady-state streaming:

```
reader → framer → worker×N → emitter → sinks (UDP / LSL / stdout)
```

- **reader**: the only goroutine touching the USB IN endpoint (`0x83`); keeps N transfers (default 16×64 KiB) queued in the kernel. Just reads and forwards.
- **framer**: reassembles TTP messages (fragmentation, coalescing, resync, chunked responses), routes responses to waiting requests by `seq`+`op`, copies gaze frames into pre-allocated pool buffers, tracks presence state and stamps it onto gaze frames in USB arrival order, hands IR images to the head-pose estimator without blocking. Drops frames (`host_drops`) rather than accumulating latency when saturated.
- **worker**: decode TLV → XDS row → fixed-size `gaze.Sample` (passed by value, no allocation).
- **emitter**: restores order via a seq-indexed ring, detects device frame-counter gaps (`device_drops`), writes to sinks, then returns the frame buffer to the pool. Also passes each sample to the head-pose estimator before the sinks.
- **supervisor**: watchdog, keep-alive, ordered shutdown, hotplug reconnection with exponential backoff.

Buffers return to their pool only *after* emission, which bounds in-flight frames to the ring size — a key invariant; preserve it when changing pool/ring sizing.

### Protocol facts that shape the code

- TTP header and TLV fields are **big-endian**; coordinates are fixed-point `i64 / 2^42` (not floats). Only the USB envelope length is little-endian. The **UDP output** is little-endian `float32`.
- USB bulk endpoints `0x83` IN / `0x05` OUT; session via vendor control transfers `0x41` (open) / `0x42` (close) on interface 0.
- The 4C has **no head-pose stream** — position/yaw/roll are derived from the two 3D corneal centers; **pitch is not observable** from two points, so it's estimated from the IR `image` stream (0x501) via the face model. Head pose is emitted in **screen coordinates** (like the Stream Engine's `head_pose`).

### Package map

| Package | Role |
|---|---|
| `internal/ttp` | framing, reassembly, TLV, XDS rows, commands, `DisplayArea` math — pure Go, tested against real captures |
| `internal/gaze` | `Sample`, column-key→field keymap, derived head pose, presence |
| `internal/usbdev` | gousb transport: kernel-driver detach, claim, vendor session |
| `internal/tracker` | the concurrent pipeline, handshake, watchdog, reconnection |
| `internal/sink` | UDP (T4CG v2 packet), LSL (cgo, `lsl` tag), stdout |
| `internal/calib` | gaze calibration: fixation, collect, compute, backup/restore, accuracy metrics, simulator |
| `internal/calibui`, `internal/x11ui` | full-screen calibration and head-pose calibration UIs; pure-Go X11 override-redirect window (`jezek/xgb`) on one RandR monitor, EDID, antialiased drawing, glyph cache |
| `internal/irimg` | 0x501 image-stream decode |
| `internal/nn` | minimal pure-Go ONNX interpreter (subset of ops) with AVX2/FMA asm kernels |
| `internal/headpose` | face landmarks, canonical-model fit, camera calibration, screen-space pitch |
| `internal/facemodel` | the 4.9 MB MediaPipe ONNX model, `go:embed`-ed into the binary |
| `internal/store` | `~/.config/tobii4c`: calibration, display geometry, camera calibration, backups |
| `cmd/tobii4c` | CLI (one file per subcommand group) |

The head-pose code must not write face images or landmarks to disk: only `camera.json` and `headpose-user.json` are persisted.

## Interfaces other programs rely on

External programs (Kite, a gaze keyboard; `tobii-ws`, a WebSocket bridge) depend on the driver, so treat these as a public API and keep them backward compatible:

- the **T4CG v2 UDP packet** (below);
- `tobii4c run` flags `-udp`, `-log-format json`, `-stats`, `-headpose on|off`, `-headpose-images-always`, `-headpose-rate`, `-headpose-rotation`, `-calibration`, `-display-mm`;
- JSON log messages of `run`: `udp output`, `usb device opened` (`bus`, `addr`, `serial`), `device identity` (`strings`), `display area (device)` / `display area set` (`area`, format of `ttp.DisplayArea.String()`), `calibration applied` / `calibration apply failed`, `subscribed` (`stream` starting with `0x500`), `usb device disconnected` / `session ended, reconnecting` (`err`); fatal errors as `errore: …` on stderr, exit code 1;
- `tobii4c calibrate -log-format json [-monitor NAME]` and `tobii4c headpose-calibrate [-monitor NAME]`, with log messages `calibration started`, `validation point` (`error_deg`, `precision_deg`), `calibration finished`, `calibration aborted` (`restored_previous`); SIGINT cancels and restores the previous calibration;
- the files in `~/.config/tobii4c` (`display.json` fields `width_mm`, `height_mm`, `offset_x_mm`, `monitor`; `calibration.bin`; `camera.json`; `headpose-user.json`) and the unit name `tobii4c.service`.

The README section "Integrare il driver in un'applicazione" documents the same list.

## Output formats

- **UDP T4CG v2**: 192-byte little-endian packet per sample; first 120 bytes are v1-compatible, new fields appended. Full layout and the `struct.unpack` format string are in the README ("Formato UDP T4CG v2") and mirrored in `internal/sink` + `examples/recv.py`. Both v1 and v2 readers exist. Always honor the validity-flag bits — unseen fields are zeroed (or −1 for pupils/openness), not omitted.
- **LSL**: 27 float32 channels; invalid values are NaN.

When changing the packet layout, update in lockstep: the writer in `internal/sink`, the round-trip test, `examples/recv.py`, the `recv` command, and the README table.
