// Package facemodel incorpora nel binario la rete dei landmark del viso usata
// per il pitch della testa: "face_landmarks_detector" del bundle
// face_landmarker.task di MediaPipe Face Landmarker (float16, versione 1),
// convertita in ONNX da SourceURL. Licenza Apache 2.0, © Google LLC: vedi
// THIRD_PARTY_NOTICES.md e LICENSES/Apache-2.0-MediaPipe.txt.
package facemodel

import _ "embed"

// ONNX è il file del modello (4,9 MB), identico a quello di SourceURL.
//
//go:embed face_landmarks_detector.onnx
var ONNX []byte

const (
	// SHA256 è l'impronta di ONNX, controllata dai test.
	SHA256 = "7d6e82dee82a1dca5fbddb282b3cc74571833a530de317fc22ae325c3358beeb"
	// SourceURL è la provenienza del file.
	SourceURL = "https://huggingface.co/senty-au/face_landmarks_detector-ONNX/resolve/main/onnx/model.onnx"
)
