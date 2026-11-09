# Dati di test

- `portrait.jpg`: ritratto ufficiale dell'astronauta Frank L. Culbertson
  (NASA, S90-38573), ridotto a 512 × 640.
- `console.jpg`: un controllore di volo alla console del Mission Control
  durante STS-51A (NASA, 51A-90016), ridotto a 640 × 422.

Fonte: [NASA Image and Video Library](https://images.nasa.gov/). Le immagini
della NASA non sono soggette a copyright negli Stati Uniti (pubblico dominio);
usarle non implica l'approvazione della NASA.

- `mediapipe_reference.json`: i 33 punti (pixel, z, visibilità, presenza) e le
  coordinate 3D che MediaPipe 0.10.14 (Pose Landmarker lite, full e heavy, in
  modalità IMAGE) dà sulle due immagini, decodificate con `image/jpeg` e
  convertite in RGB come fa il warp di `internal/tflite` (crominanza del pixel
  più vicino), salvate in PNG senza perdite prima di passarle a MediaPipe.
