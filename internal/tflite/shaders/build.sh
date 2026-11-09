#!/bin/sh
# Ricompila gli shader in SPIR-V (go generate ./internal/tflite).
set -e
cd "$(dirname "$0")"
mkdir -p ../spv
for f in *.comp; do
	glslc -O --target-env=vulkan1.1 -fshader-stage=compute -I . "$f" -o "../spv/${f%.comp}.spv"
done
