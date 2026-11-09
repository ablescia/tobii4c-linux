# Dati di test

`litert_reference.json`: uscite delle sei reti di MediaPipe Pose (detector e
landmark del corpo intero nelle varianti lite, full e heavy; detector e
landmark del mezzo busto di MediaPipe 0.8.3) calcolate con LiteRT
(`ai-edge-litert`, XNNPACK) sull'ingresso deterministico di `testInput` in
`tflite_test.go`, ricalcolato in numpy con le stesse formule. Per ogni uscita
ci sono il numero di valori, il passo di campionamento, il massimo in valore
assoluto e circa 400 valori campionati.
