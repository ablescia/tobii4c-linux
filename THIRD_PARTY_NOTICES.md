# Componenti di terze parti

## Inclusi in questo repository

| Componente | File | Licenza | Testo |
|---|---|---|---|
| Modello canonico del viso di [MediaPipe](https://github.com/google-ai-edge/mediapipe) (33 vertici e pesi della base di Procrustes), Copyright 2020 The MediaPipe Authors | `internal/headpose/canonical.go` | Apache 2.0 | [`LICENSES/Apache-2.0-MediaPipe.txt`](LICENSES/Apache-2.0-MediaPipe.txt) |
| Connessioni della mesh del viso di [MediaPipe](https://github.com/google-ai-edge/mediapipe) (`face_mesh_connections.py`: triangolazione e contorni), Copyright 2021 The MediaPipe Authors | `internal/headpose/mesh.go` | Apache 2.0 | [`LICENSES/Apache-2.0-MediaPipe.txt`](LICENSES/Apache-2.0-MediaPipe.txt) |
| Rete `face_landmarks_detector` di MediaPipe Face Landmarker, © Google LLC, convertita in ONNX (file non modificato da [senty-au/face_landmarks_detector-ONNX](https://huggingface.co/senty-au/face_landmarks_detector-ONNX)) | `internal/facemodel/face_landmarks_detector.onnx`, incorporato nel binario | Apache 2.0 | [`LICENSES/Apache-2.0-MediaPipe.txt`](LICENSES/Apache-2.0-MediaPipe.txt) |
| Foto NASA (S90-38573 e 51A-90016, NASA/JSC), ridotte: pubblico dominio | `internal/bodypose/testdata/*.jpg` | pubblico dominio | [`internal/bodypose/testdata/README.md`](internal/bodypose/testdata/README.md) |
| Catture USB dell'Eye Tracker 5 da [Krazubu/tobii-linux](https://github.com/Krazubu/tobii-linux), Copyright (c) 2026 Andrii Klaptsov | `internal/ttp/testdata/et5-*.hex` | MIT | [`internal/ttp/testdata/LICENSE`](internal/ttp/testdata/LICENSE) |

## Dipendenze Go (incluse nel binario compilato)

| Modulo | Licenza |
|---|---|
| [`github.com/google/gousb`](https://github.com/google/gousb) | Apache 2.0 |
| [`github.com/jezek/xgb`](https://github.com/jezek/xgb) | BSD 3-Clause |
| [`golang.org/x/image`](https://pkg.go.dev/golang.org/x/image) | BSD 3-Clause |
| [`golang.org/x/text`](https://pkg.go.dev/golang.org/x/text) | BSD 3-Clause |
| [`github.com/ebitengine/purego`](https://github.com/ebitengine/purego) | Apache 2.0 |

Il binario si collega anche a libusb-1.0 (LGPL 2.1) e, con `-tags lsl`, a
[liblsl](https://github.com/sccn/liblsl) (MIT), entrambe come librerie
condivise di sistema.

## Scaricati a runtime (non inclusi)

`examples/webcam-body` scarica al primo uso, in `~/.cache/tobii4c/models`, i
modelli di [MediaPipe Pose](https://github.com/google-ai-edge/mediapipe)
© Google LLC, licenza Apache 2.0
([`LICENSES/Apache-2.0-MediaPipe.txt`](LICENSES/Apache-2.0-MediaPipe.txt)):
`pose_landmarker_{lite,full,heavy}.task` da `storage.googleapis.com/mediapipe-models`
e `pose_detection.tflite`, `pose_landmark_upper_body.tflite` dal tag `0.8.3.2`
del repository. Le impronte SHA-256 attese sono in
`internal/bodypose/models.go`. I file di riferimento dei test
(`internal/tflite/testdata/litert_reference.json`,
`internal/bodypose/testdata/mediapipe_reference.json`) sono uscite di quei
modelli calcolate con LiteRT e MediaPipe.

Il binario carica a runtime `libvulkan.so.1` (loader Khronos, Apache 2.0) e il
driver Vulkan del sistema, se presenti.

## Riferimenti per il protocollo

Il protocollo TTP è stato ricostruito a partire dai progetti clean-room per
l'Eye Tracker 5 [Tropaion/Tobii_Linux](https://github.com/Tropaion/Tobii_Linux),
[Krazubu/tobii-linux](https://github.com/Krazubu/tobii-linux) e
[Aetherall/tobiifree](https://github.com/Aetherall/tobiifree).
