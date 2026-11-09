//go:build !linux

package tflite

import "errors"

// GPU è disponibile solo su Linux (Vulkan via internal/vk).
type GPU struct{ Profile bool }

// NewGPU su questo sistema restituisce sempre un errore.
func NewGPU() (*GPU, error) { return nil, errors.New("tflite: GPU (Vulkan) gestita solo su Linux") }

func (b *GPU) Name() string                { return "GPU" }
func (b *GPU) Device() string              { return "" }
func (b *GPU) SetImage(*Image) error       { return errors.New("tflite: GPU non disponibile") }
func (b *GPU) Load(*Graph) (Runner, error) { return nil, errors.New("tflite: GPU non disponibile") }
func (b *GPU) Close()                      {}
