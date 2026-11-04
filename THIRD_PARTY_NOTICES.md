# Componenti di terze parti

## Inclusi in questo repository

| Componente | File | Licenza | Testo |
|---|---|---|---|
| Modello canonico del viso di [MediaPipe](https://github.com/google-ai-edge/mediapipe) (33 vertici e pesi della base di Procrustes), Copyright 2020 The MediaPipe Authors | `internal/headpose/canonical.go` | Apache 2.0 | [`LICENSES/Apache-2.0-MediaPipe.txt`](LICENSES/Apache-2.0-MediaPipe.txt) |
| Rete `face_landmarks_detector` di MediaPipe Face Landmarker, © Google LLC, convertita in ONNX (file non modificato da [senty-au/face_landmarks_detector-ONNX](https://huggingface.co/senty-au/face_landmarks_detector-ONNX)) | `internal/facemodel/face_landmarks_detector.onnx`, incorporato nel binario | Apache 2.0 | [`LICENSES/Apache-2.0-MediaPipe.txt`](LICENSES/Apache-2.0-MediaPipe.txt) |
| Catture USB dell'Eye Tracker 5 da [Krazubu/tobii-linux](https://github.com/Krazubu/tobii-linux), Copyright (c) 2026 Andrii Klaptsov | `internal/ttp/testdata/et5-*.hex` | MIT | [`internal/ttp/testdata/LICENSE`](internal/ttp/testdata/LICENSE) |

## Dipendenze Go (incluse nel binario compilato)

| Modulo | Licenza |
|---|---|
| [`github.com/google/gousb`](https://github.com/google/gousb) | Apache 2.0 |
| [`github.com/jezek/xgb`](https://github.com/jezek/xgb) | BSD 3-Clause |
| [`golang.org/x/image`](https://pkg.go.dev/golang.org/x/image) | BSD 3-Clause |
| [`golang.org/x/text`](https://pkg.go.dev/golang.org/x/text) | BSD 3-Clause |

Il binario si collega anche a libusb-1.0 (LGPL 2.1) e, con `-tags lsl`, a
[liblsl](https://github.com/sccn/liblsl) (MIT), entrambe come librerie
condivise di sistema.

## Riferimenti per il protocollo

Il protocollo TTP è stato ricostruito a partire dai progetti clean-room per
l'Eye Tracker 5 [Tropaion/Tobii_Linux](https://github.com/Tropaion/Tobii_Linux),
[Krazubu/tobii-linux](https://github.com/Krazubu/tobii-linux) e
[Aetherall/tobiifree](https://github.com/Aetherall/tobiifree).
