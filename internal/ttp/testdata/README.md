# Fixture di test

I file `et5-*.hex` sono messaggi TTP reali di un **Tobii Eye Tracker 5**
(stessa famiglia di protocollo del 4C), un messaggio per file in esadecimale,
envelope USB compreso.

Provenienza: [Krazubu/tobii-linux](https://github.com/Krazubu/tobii-linux),
`crates/tobii-proto/fixtures/` (commit `133b19b`). Sono catture dello Stream
Engine Windows 4.1.0.3, e il numero di serie è stato azzerato dall'autore
originale. Licenza MIT, Copyright (c) 2026 Andrii Klaptsov: testo completo in
[`LICENSE`](LICENSE).

Risposte e stream (device → host):

| file | contenuto |
|---|---|
| `et5-init-rsp-1000` | risposta a hello (versione protocollo 0x10008, identica sul 4C) |
| `et5-init-rsp-1200` | catalogo stream |
| `et5-init-rsp-1420` | stringhe di identità |
| `et5-init-rsp-1430` | display area |
| `et5-init-rsp-2110` | mounting dell'ET5 (segni guida, angolo della camera) |
| `et5-columns-rsp-1210-0x1770` | colonne dello stream algodbg |
| `et5-session1-gaze-frame` | frame gaze 43780, con i valori consegnati dallo Stream Engine |
| `et5-init-presence` | messaggio presence 0x504 |
| `et5-calib-rsp-1100-head` | primo pezzo della risposta a una lettura del blob di calibrazione (659 056 byte) |

Comandi (host → device), confrontati byte per byte con quelli generati dal
driver:

| file | contenuto |
|---|---|
| `et5-calib-cmd-1010-seq44` | start della calibrazione |
| `et5-calib-cmd-1030-seq48`, `-seq51` | collect dei punti (0,5; 0,5) e (0,5; 0,1) |
| `et5-calib-cmd-1070-seq49` | compute |
| `et5-calib-cmd-1100-seq43` | lettura del blob di calibrazione |
| `et5-change-display-cmd-1440` | scrittura della display area (597 × 336 mm) |
