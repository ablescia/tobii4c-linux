// Dichiarazioni comuni a tutti gli shader del motore TFLite.
//
// binding 0: l'arena della GPU (pesi e tensori, float32, NHWC); gli offset
// nelle push constant sono in float.
// binding 1: il buffer dell'immagine condiviso con la CPU (parole a 32 bit).

layout(std430, set = 0, binding = 0) buffer Arena { float A[]; };
layout(std430, set = 0, binding = 1) readonly buffer Img { uint IMG[]; };

const uint NONE = 0xffffffffu;

// Attivazioni come ActivationFunctionType di TFLite, più la logistica.
float act(float x, uint m) {
	if (m == 1u) return max(x, 0.0);
	if (m == 2u) return clamp(x, -1.0, 1.0);
	if (m == 3u) return clamp(x, 0.0, 6.0);
	if (m == 4u) return tanh(x);
	if (m == 5u) return 1.0 / (1.0 + exp(-x));
	return x;
}

// Indice lineare del thread su una griglia di workgroup 2D (le griglie
// grandi superano i 65535 workgroup per asse).
uint linearID(uint local) {
	return gl_GlobalInvocationID.y * gl_NumWorkGroups.x * local + gl_GlobalInvocationID.x;
}
