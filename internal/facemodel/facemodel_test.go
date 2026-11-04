package facemodel

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestEmbeddedChecksum(t *testing.T) {
	sum := sha256.Sum256(ONNX)
	if got := hex.EncodeToString(sum[:]); got != SHA256 {
		t.Fatalf("embedded model SHA-256 %s, want %s", got, SHA256)
	}
}
