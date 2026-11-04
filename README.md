# tobii4c: driver user-space per Tobii Eye Tracker 4C su Linux

Driver in Go, senza componenti kernel e senza i binari proprietari Tobii, per il
**Tobii Eye Tracker 4C** (`2104:0127`, "EyeChip", generazione IS4). Parla il
protocollo TTP direttamente sugli endpoint bulk USB tramite `google/gousb`
(libusb-1.0). Estrae a 90 Hz sguardo, posizione degli occhi (nel sistema del
tracker, in quello dello schermo e nel track box), pupille, apertura delle
palpebre, posa della testa e presenza dell'utente, e li inoltra via **UDP
locale** (pacchetto binario little-endian) e/o **Lab Streaming Layer**. Il
pitch della testa, che i soli occhi non permettono di ricavare, viene stimato
dalle immagini IR del sensore con un modello di landmark del viso eseguito in
Go puro (vedi [Pitch della testa](#pitch-della-testa)).

## Stato: verificato sul dispositivo reale

Misure del 2026-11-03 su un 4C con firmware `IS4 4.7.0-d374125`
(`IS4_Large_Peripheral`), Ryzen 7 2700X, kernel 6.12:

| Metrica | Valore misurato |
|---|---|
| Frequenza dello stream gaze | **90,0 Hz** (op 0x672 dichiara 90/90) |
| Latenza della pipeline (ricezione USB → sink scritto) | p50 **45 µs**, p99 ~80 µs, max < 100 µs |
| Età del campione al ricevitore UDP | 50–140 µs |
| Decodifica di un frame (1692 B, 39 colonne) | 1,2 µs, **0 allocazioni** |
| Cicli di GC durante 12 s di streaming | **0** |
| Campioni persi / fuori ordine (1065 via UDP, 801 con 8 worker) | **0 / 0** |
| Data race (`go build -race`, 8 worker, canale non bufferizzato) | nessuna |
| Hotplug (de-autorizzazione USB simulata) | disconnessione rilevata in 3 ms, riconnessione 32 ms dopo il ritorno del dispositivo |
| LSL (inlet reale `tobii4c lsl-recv`, liblsl 1.17.7) | 27 canali, 1410 campioni in 15 s = 90,07 Hz, nessuna pausa, ritardo mediano 0,14 ms; pitch valido nel 100% dei campioni con l'utente davanti |
| Calibrazione a 7 punti (`tobii4c calibrate`, 3 sessioni reali) | accuratezza media verificata **0,61°–0,92°** su 5 punti di verifica |

## Note sul protocollo e sul dispositivo

1. **Origine del protocollo.** Gli installer per Linux esistenti per il 4C,
   come `Eitol/tobii_eye_tracker_linux_installer` e
   `johngebbie/tobii_4C_for_linux`, installano i binari proprietari Tobii
   (`tobiiusbservice`, `libtobii_stream_engine.so`). Questo driver non li usa:
   parla il protocollo ricostruito dai progetti clean-room per l'Eye Tracker 5
   [Tropaion/Tobii_Linux](https://github.com/Tropaion/Tobii_Linux) (wiki),
   [Krazubu/tobii-linux](https://github.com/Krazubu/tobii-linux) e
   [Aetherall/tobiifree](https://github.com/Aetherall/tobiifree), e ne ha
   verificato la validità sul 4C: stesso framing, stessi op, stesse 39
   colonne gaze (i nomi li restituisce il firmware stesso, vedi `tobii4c info`).
2. **Il protocollo è big-endian.** Header TTP e campi TLV sono big-endian. Le
   coordinate sono fixed-point `i64 / 2^42` (tipo `0x04`), non float. L'unico
   campo little-endian è la lunghezza nell'envelope USB. L'output UDP del
   driver invece è little-endian `float32`.
3. **L'errore `EyeCmdErr ... 0x42e` che compare in Talon** è l'op 1070
   `CALIBRATE_POINTS_APPLY`: un errore di calibrazione, non di
   inizializzazione.
4. **Endpoint bulk, non interrupt**: `0x83` IN, `0x05` OUT. `0x04` OUT esiste ma
   nessuna implementazione nota lo usa, e qui non viene mai scritto. Le
   interfacce 1 e 2 sono una camera UVC 640×480 con formato vendor, non usata.
5. **Il 4C non ha uno stream di head pose.** Il catalogo del firmware elenca
   solo `gaze`, `image`, `presence`, `image_collection`, `algodbg`, `log`. La
   posa della testa viene quindi **derivata** dai centri corneali 3D: posizione
   (punto medio degli occhi, mm), yaw e roll (dalla linea interoculare). Il
   **pitch non è osservabile** da due punti: lo stima il driver dalle immagini
   dello stream `image` (punto 11), altrimenti resta marcato non valido.
   La posa è nel **sistema dello schermo**, come la `head_pose` dello Stream
   Engine: il firmware invia ogni centro corneale sia nel sistema del tracker
   (0x02/0x08) sia dal centro della display area (0x22/0x24, "Gaze Origin
   Display"). Il secondo è il `gaze_origin` di Tobii. È la trasformazione
   tracker → schermo con la display area configurata, verificata a meno di 2 µm
   su una cattura ET5 (`ttp.DisplayArea.ToDisplay`).
6. **Non serve un keep-alive di protocollo**: dopo il subscribe il dispositivo
   trasmette finché la sessione resta aperta. Il driver ha comunque un watchdog
   (stallo → resubscribe → riconnessione) e un ping di salute opzionale che
   misura l'RTT.
7. **Nessuna autenticazione realm** (query realm = 0). A differenza dell'ET5, il
   4C **non si riavvia** alla chiusura della sessione e **conserva la display
   area** (sul dispositivo testato: 652×324 mm già configurati).
8. **La CPU dell'host non è il collo di bottiglia**: a 90 Hz la decodifica
   occupa ~0,01% di un core. Il pool di worker (con riordino per sequenza)
   quindi non riduce la latenza, che dipende dal polling USB e
   dall'elaborazione nel dispositivo.
9. **Lo stream `presence` (0x504) è a eventi**: un messaggio alla
   sottoscrizione e poi solo ai cambi di stato. I valori sono quelli dell'enum
   dello Stream Engine: 1 = assente (osservato sul 4C senza nessuno davanti),
   2 = presente (cattura ET5 con l'utente davanti), 0 = sconosciuto.
10. **La colonna 0x1d/0x1e è la validità del track box** ("Eye 3D Relative
    Position Validity"), non dello sguardo 2D del singolo occhio, che non ha
    una validità propria e segue lo stato di tracciamento dell'occhio.
11. **Lo stream `image` (0x501) è l'immagine intera della camera IR**: riga XDS
    da 307 350 byte a 30 Hz con timestamp (stesso orologio del gaze), 8 bit
    per pixel, 640 × 480, in scala di grigi. Sottoscriverlo non rallenta il
    gaze (resta a 90 Hz). `image_collection` (0x508) viene rifiutato
    (`UNAUTHORIZED`). La colonna "Eye 2D position relative to sensor area"
    (0x19/0x1a) vale −1 anche con l'utente tracciato (cattura ET5), quindi
    per sapere dove sono gli occhi nell'immagine il driver usa il modello del
    viso.

## Architettura

```
          rawCh (cap 1)              jobs (cap 64)             results
┌────────┐ []byte crudi ┌────────┐ frame + seq  ┌──────────┐  Sample   ┌──────────┐
│ reader │─────────────→│ framer │─────────────→│ worker×N │──────────→│ emitter  │→ UDP / LSL / stdout
│ 1 gor. │← rawFree     │ 1 gor. │              │ TLV→XDS  │           │ riordino │
└────────┘              └────────┘              └──────────┘           └──────────┘
 bloccata solo su        riassembla i messaggi,  zero-alloc,           ordine per seq,
 USB IN 0x83 (8 URB      risposte → richieste    Row riusata           drop detection sul
 in coda nel kernel)     in attesa, stream → job                       frame counter, statistiche
                                                    frameFree ←──────────────┘ (buffer restituito dopo l'emissione)
```

- **reader**: l'unica goroutine che tocca l'endpoint IN. Usa un `ReadStream`
  di gousb con 8 transfer da 16 KiB sempre in coda nel kernel, così il
  dispositivo trova buffer pronti anche mentre il frame precedente viene
  passato avanti. Non fa altro che leggere e inoltrare.
- **framer**: riassembla i messaggi (frammentazione, accorpamento,
  risincronizzazione, risposte a pezzi), consegna le risposte alle richieste in
  attesa (match su `seq` e `op`), copia i frame gaze in buffer pre-allocati.
  Tiene lo stato dello stream presence e lo assegna a ogni frame gaze
  nell'ordine di arrivo dall'USB.
  Se la pipeline è satura scarta il frame (`host_drops`) invece di accumulare
  latenza.
- **worker**: decodificano TLV → riga XDS → `gaze.Sample` (struct a dimensione
  fissa, passata per valore). Nessuna allocazione.
- **emitter**: ripristina l'ordine con un ring indicizzato per `seq`, rileva i
  buchi nel frame counter del dispositivo (`device_drops`) e scrive sui sink.
  Il buffer del frame torna al pool solo dopo l'emissione: così i frame in volo
  non superano mai la dimensione del ring.
- **head pose** (facoltativo): il framer consegna le immagini 0x501 a una
  goroutine separata senza mai bloccare (se è occupata l'immagine è
  scartata); l'emitter le passa ogni campione prima dei sink, per registrare
  gli occhi e applicare l'ultimo pitch stimato. Vedi
  [Pitch della testa](#pitch-della-testa).
- **supervisor**: watchdog sullo stream, keep-alive, arresto ordinato
  (unsubscribe e chiusura sessione a pipeline ancora attiva). Riconnessione con
  backoff esponenziale su hotplug.

| Pacchetto | Contenuto |
|---|---|
| `internal/ttp` | framing, riassemblaggio, TLV, righe XDS, comandi. Puro Go, testato su catture reali |
| `internal/gaze` | `Sample`, mappa chiavi → campi, posa della testa derivata, presenza |
| `internal/usbdev` | trasporto gousb: detach del driver kernel, claim, sessione vendor 0x41/0x42 |
| `internal/tracker` | pipeline concorrente, handshake, watchdog, riconnessione |
| `internal/sink` | UDP (T4CG v2), LSL (cgo, build tag `lsl`), stampa |
| `internal/calib` | logica di calibrazione: fissazione, raccolta, compute, backup e ripristino, metriche di accuratezza, simulatore |
| `internal/calibui` | interfaccia a schermo intero: menu, posizionamento del sensore, posizione, calibrazione, verifica |
| `internal/x11ui` | finestra X11 in Go puro (`jezek/xgb`): RandR, EDID, tastiera, disegno antialiasing, testo |
| `internal/irimg` | decodifica dello stream immagine 0x501, PGM |
| `internal/nn` | interprete ONNX minimale in Go puro (Conv, depthwise, PRelu, Add, MaxPool, Pad, Reshape, Transpose, Sigmoid), kernel AVX2/FMA in assembly |
| `internal/headpose` | landmark del viso, fit del modello canonico, calibrazione automatica della camera, pitch nel sistema dello schermo |
| `internal/store` | `~/.config/tobii4c`: calibrazione, geometria dello schermo, calibrazione della camera, backup |
| `cmd/tobii4c` | CLI |

## Protocollo in breve

```
envelope 8 B (LE)        header TTP 24 B (BE)                         payload
[dir 0 0 0][len u32]     [marker][seq][status][op][error][plen]        00 00 + TLV [type u8][len u32 BE][value]
 dir 0x00 host→dev        marker 0x51 req, 0x52 resp, 0x53 stream, 0x4e notifica
 dir 0x01 dev→host        len OUT = byte dopo l'envelope; len IN = messaggio intero
```

Sessione: control transfer vendor `0x41` (open) e `0x42` (close) su interfaccia 0.
Handshake usato: `0x3e8` hello, `0x58c` identità, `0x640` realm, `0x4b0` catalogo
stream, `0x4ba` colonne gaze, `0x83e` mounting, `0x596` display area, `0x4c4`
subscribe `0x500`.

Calibrazione (sequenza dello Stream Engine Windows, catture di Krazubu/tobii-linux):
`1100` lettura del blob attivo (backup), `1010` start, `1060` clear, poi per i
batch {1, 3, 3} punti `1030` collect (x, y, maschera occhi 3) e `1070` compute,
infine `1100` lettura del nuovo blob e `1020` stop. Un blob si riscrive con
`1110`. I comandi più lunghi di 4087 byte partono in più bulk OUT, ciascuna col
proprio envelope.

## Build

```sh
sudo apt install golang libusb-1.0-0-dev pkg-config   # Go ≥ 1.22
make build          # → bin/tobii4c
make test           # vet + unit test (non richiedono il dispositivo)
make bench
```

In alternativa, senza clonare (servono comunque `libusb-1.0-0-dev` e
`pkg-config`; il binario va in `$(go env GOPATH)/bin`):

```sh
go install github.com/ablescia/tobii4c-linux/cmd/tobii4c@latest
```

Se il repository è privato, Go deve saltare il proxy pubblico e usare le tue
credenziali Git (chiave SSH o token):
`GOPRIVATE=github.com/ablescia/* go install …`.

Per il pitch della testa serve il modello del viso (4,9 MB, verificato con
SHA-256; nessuna libreria nativa):

```sh
make headpose-model # → ~/.local/share/tobii4c/face_landmarks_detector.onnx
```

Con LSL serve liblsl. Debian non la pacchettizza; il `.deb` ufficiale per Ubuntu
24.04 funziona anche su Debian 13 (provato con la 1.17.7):

```sh
curl -LO https://github.com/sccn/liblsl/releases/download/v1.17.7/liblsl-1.17.7-noble_amd64.deb
echo "9cac7570c2b256077b86c011ffa44b7a40344f2271cc353120c387fb9134ddf1  liblsl-1.17.7-noble_amd64.deb" | sha256sum -c -
sudo apt install ./liblsl-1.17.7-noble_amd64.deb
make build-lsl      # equivale a: go build -tags lsl ./cmd/tobii4c
./bin/tobii4c run -lsl &            # outlet "Tobii4C"
./bin/tobii4c lsl-recv -duration 10s # ricevitore di prova: canali, frequenza, NaN, ritardo
```

## Permessi senza root (udev)

Come Talon, il driver non richiede root: una regola udev assegna il nodo
`/dev/bus/usb/BBB/DDD` all'utente a ogni hotplug. La regola è in
`udev/70-tobii-4c.rules` ed è incorporata nel binario:

```sh
./bin/tobii4c udev | sudo tee /etc/udev/rules.d/70-tobii-4c.rules
sudo udevadm control --reload-rules
sudo udevadm trigger --action=add --subsystem-match=usb --attr-match=idVendor=2104
# oppure: make install-udev
```

La regola usa `TAG+="uaccess"`: systemd-logind concede un'ACL `rw` all'utente
della sessione locale attiva. Il prefisso `70-` è obbligatorio, perché
`uaccess` viene applicato da `73-seat-late.rules`. Nel file ci sono, commentate,
le alternative `GROUP="plugdev", MODE="0660"` per sistemi headless o via SSH e
`MODE="0666"`, permissiva e sconsigliata su macchine multiutente. Verifica:

```sh
getfacl /dev/bus/usb/001/002    # deve comparire user:<utente>:rw-
```

Il detach del driver kernel avviene tramite `SetAutoDetach(true)` di gousb, che
chiama `libusb_detach_kernel_driver` sulle interfacce. Sull'interfaccia 0 di
solito non è legato nessun driver. Al rilascio libusb ricollega il driver
dell'interfaccia reclamata.

## Uso

```sh
./bin/tobii4c info                 # identità, catalogo stream, colonne, display area
./bin/tobii4c info -probe -json report.json
./bin/tobii4c run                  # sguardo, occhi, testa, presenza → UDP 127.0.0.1:5005
./bin/tobii4c run -print 500ms -stats 5s
./bin/tobii4c run -lsl -udp ""     # solo LSL (build con -tags lsl)
./bin/tobii4c recv                 # ricevitore UDP di prova (altro terminale)
python3 examples/recv.py           # ricevitore Python, solo libreria standard
./bin/tobii4c dump -streams 0x504 -frames 2 -duration 5s   # decodifica generica per reverse engineering
./bin/tobii4c dump -images /tmp/ir -frames 0 -duration 3s  # salva frame IR come PGM (due al secondo)
./bin/tobii4c face /tmp/ir/ir-123.pgm -out viso.png        # modello del viso su un'immagine, PNG annotato
./bin/tobii4c run -print 500ms -headpose-debug /tmp/hp      # pitch con immagini annotate ogni 2 s
./bin/tobii4c headpose-calibrate   # app guidata: calibrazione della camera, zero del viso, verifica
./bin/tobii4c calibrate            # configurazione a schermo intero (vedi sotto)
./bin/tobii4c display              # mounting e display area; -set 698x393 -offset-x 0 per scriverla
./bin/tobii4c calibration save     # blob attivo → ~/.config/tobii4c/calibration.bin
./bin/tobii4c calibration apply    # file → dispositivo (fa prima un backup)
./bin/tobii4c calibration restore  # rimette l'ultimo backup
```

Flag principali di `run`:

| Flag | Default | Significato |
|---|---|---|
| `-udp` | `127.0.0.1:5005` | destinazione dei datagrammi (vuoto = off) |
| `-lsl` | off | outlet LSL `Tobii4C`, 27 canali float32 |
| `-presence` | on | sottoscrive lo stream presence (0x504) e lo riporta in ogni campione |
| `-headpose` | auto | pitch della testa dalle immagini IR: `auto` se il modello è installato, `on`, `off` |
| `-headpose-rate` | 30 | inferenze al secondo al massimo (le immagini arrivano a 30 Hz) |
| `-headpose-workers` | 1 | goroutine dell'inferenza |
| `-headpose-debug` | — | cartella per un'immagine annotata ogni 2 s (landmark, ritaglio, occhi proiettati) |
| `-headpose-images-always` | off | non sospende lo stream immagine quando l'utente è assente |
| `-headpose-model`, `-headpose-camera` | vedi testo | modello ONNX e file della calibrazione della camera |
| `-workers` | 2 | goroutine di decodifica |
| `-raw-queue` | 1 | capacità del canale reader→framer (0 = non bufferizzato) |
| `-frame-pool` | 64 | frame in volo prima di scartare |
| `-in-flight`, `-transfer-size` | 8, 16384 | URB IN in coda nel kernel e relativa dimensione |
| `-stall` | 3s | senza frame: resubscribe, poi riconnessione |
| `-keepalive` | 5s | ping di salute (op 0x58c), RTT nei log debug |
| `-stats` | 10s | log periodico: Hz, latenza p50/p99/max, drop, resync |
| `-display-mm`, `-display-offset-x` | — | scrive la display area all'avvio, es. `698x393`, calcolata col mounting del tracker |
| `-calibration` | — | applica un blob a ogni connessione; `saved` = `~/.config/tobii4c/calibration.bin` |
| `-keymap` | — | mappa colonne JSON alternativa (`tobii4c keymap` stampa quella di default) |
| `-log-level`, `-log-format` | info, text | `slog`; `json` per l'ingestione strutturata |

Come servizio utente systemd: `make install` e poi `systemd/tobii4c.service`.

## Configurazione e calibrazione

`tobii4c calibrate` apre una finestra a schermo intero sul monitor primario
(`-monitor HDMI-0` per sceglierne un altro, `-monitors` per elencarli) e
ripropone i passi dell'applicazione ufficiale Tobii:

1. **Posiziona il sensore.** Due frecce sul bordo inferiore dello schermo vanno
   allineate ai due segni guida sul tracker. Il 4C dichiara (op 2110) 2 segni a
   184 mm e una camera inclinata di 20°. Se l'EDID del monitor è credibile, la
   dimensione viene da lì e le frecce si spostano insieme, altrimenti (o
   premendo M) la loro distanza in pixel dà il passo del pixel. La display
   area si calcola con la stessa formula dello Stream Engine
   (`tobii_calculate_display_area_basic`), verificata sulle catture Windows
   sotto il micrometro, e si salva in `~/.config/tobii4c/display.json`.
2. **Controlla la tua posizione.** Vista dal vivo degli occhi (a specchio),
   scala della distanza con la zona 50–80 cm, indicazioni per avvicinarsi,
   allontanarsi o centrarsi.
3. **Calibra lo sguardo.** Sono i 7 punti dell'app Windows nello stesso ordine,
   a batch {1, 3, 3} con un compute dopo ogni batch. Prima di ogni raccolta il
   programma aspetta che lo sguardo resti per 0,5 s entro 80 mm dal punto
   (ammiccamenti tollerati). Fa eccezione il primo punto: dopo il clear il 4C
   non fornisce sguardo finché il primo compute non produce una calibrazione,
   quindi lì si raccoglie dopo 0,9 s di sosta con gli occhi visibili. È anche
   il motivo per cui l'app Windows calcola subito dopo il primo punto. Se durante la raccolta gli occhi si perdono, il
   punto viene scartato (op 1080) e ripresentato, fino a 3 tentativi. Prima di
   iniziare la calibrazione attiva viene salvata in
   `~/.config/tobii4c/backups/`, e Esc o un errore la rimettono sul dispositivo.
4. **Verifica.** 5 punti diversi da quelli di calibrazione. Per ciascuno si
   calcolano l'accuratezza (angolo tra bersaglio e sguardo medio visto
   dall'occhio, in gradi) e la precisione (RMS campione-campione). Il
   risultato mostra bersagli, campioni ed errore per punto. Da lì si può
   ricalibrare, ripristinare la calibrazione precedente (P) o provare lo
   sguardo dal vivo (G).

La nuova calibrazione è attiva sul dispositivo subito dopo il compute e viene
salvata in `~/.config/tobii4c/calibration.bin`.

Misure sul 4C reale. Un collect blocca **~665 ms** (~1,31 s sui due angoli in
alto). Un compute dura 65 ms dopo 1 punto, ~830 ms dopo 4 e ~1,6 s dopo 7. Ogni
compute è seguito dalla notifica 3220 col nuovo id di calibrazione; al
ripristino torna l'id originale. Durante start, clear, compute e scrittura del
blob il 4C sospende lo stream dello sguardo (da 15 a 144 frame): il driver li
conta come `device_pauses` e non come frame persi. L'accuratezza si misura
sullo sguardo filtrato (0x1c, quello inviato via UDP), la precisione su quello
non filtrato (0x20), perché il filtro del firmware la farebbe sembrare ~0,01°.

**Perché una finestra nativa e non una pagina web.** Il Fullscreen API copre lo
schermo, ma all'ingresso il browser mostra per alcuni secondi il banner "premi
Esc per uscire", proprio dove cade il punto in alto al centro. Inoltre va a
schermo intero sul monitor dove si trova la finestra e gestisce Esc da sé. La
finestra X11 in Go puro (niente cgo, niente toolkit) si posiziona via RandR
esattamente sul monitor scelto, prende la tastiera e nasconde il cursore. Su
Wayland funziona tramite XWayland.

**Simulazione.** `tobii4c calibrate -simulate` esegue tutto il flusso senza
tracker: occhi sintetici che ondeggiano, sguardo con latenza saccadica,
rumore, errore sistematico che il compute riduce e ammiccamenti. Serve a
provare l'interfaccia, anche su un display virtuale:
`Xvfb :99 & DISPLAY=:99 tobii4c calibrate -simulate`.

## Pitch della testa

Il firmware del 4C non calcola la posa della testa, e dai due soli centri
corneali si ricavano posizione, yaw e roll ma non il pitch. Il driver lo stima
dalle immagini IR dello stream 0x501, come fa lo Stream Engine con l'ET5:

1. **Ritaglio del viso**: dai landmark del frame precedente (come MediaPipe),
   oppure dagli occhi del tracker proiettati con la camera calibrata, oppure,
   se non c'è altro, con una ricerca a griglia su tutta l'immagine. La ricerca
   costa una trentina di inferenze: non parte mai quando lo stream presence
   dice che davanti non c'è nessuno, e si dirada fino a una volta ogni 5 s se
   continua a fallire. Il contrasto del ritaglio viene normalizzato (le
   immagini IR sono scure).
2. **Landmark**: rete `face_landmarks_detector` di MediaPipe Face Landmarker
   (478 punti 3D, ritaglio 256 × 256; Apache 2.0, © Google), convertita in
   ONNX ed eseguita da `internal/nn`, un interprete in Go puro dei soli
   operatori che servono. Un'inferenza costa 113 milioni di
   moltiplicazioni-accumulo: 18–22 ms su un core del Ryzen 7 2700X, con
   AVX2/FMA (il codice Go puro resta come ripiego sulle altre CPU).
3. **Posa nella camera**: fit di Procrustes pesato tra i landmark e il modello
   canonico del viso di MediaPipe (33 punti rigidi, pesi di MediaPipe), prima
   in prospettiva debole per la scala, poi con i punti retroproiettati lungo
   i raggi della camera.
4. **Sistema dello schermo**: camera → tracker con la calibrazione della
   camera, tracker → schermo con la display area. Gli angoli sono di Eulero
   R = Ry(yaw)·Rx(pitch)·Rz(roll), destrorsi attorno agli assi dello schermo
   come la `head_pose` dello Stream Engine: il pitch è l'elevazione della
   direzione in cui guarda il viso, **positivo verso l'alto**. Filtro 1€ sul
   pitch.
5. **Applicazione**: l'emitter scrive il pitch nei campioni gaze vicini
   (entro 150 ms dall'immagine), con il bit 7 dei flag UDP. Yaw e roll restano
   quelli degli occhi, più precisi; quelli dell'immagine servono da controllo.

**Calibrazione della camera.** Il firmware non espone i parametri della
camera. Il driver li stima abbinando il centro di ciascuna iride trovato dal
modello al centro corneale 3D dato dal tracker. Poi risolve focale, punto
principale, rotazione e posizione della camera con un Levenberg-Marquardt
robusto, provando anche l'ipotesi di un'immagine speculare. La calibrazione
viene adottata quando l'errore di riproiezione è sotto 4 px e l'incertezza
della rotazione sotto 1°. È salvata in `~/.config/tobii4c/camera.json` insieme
al numero di serie, e le sessioni successive la ricaricano subito. Finché la
camera non è calibrata, il pitch resta non valido.

Il modo comodo è l'applicazione **`tobii4c headpose-calibrate`**, a schermo
intero come `calibrate`. Mostra sempre il tempo che resta, la distanza e la
posizione dal vivo e l'immagine della camera con i punti del viso. Il menu ha
tre scelte:
1. **calibrazione completa**: ci si muove con calma finché si riempiono le
   caselle della distanza (50–85 cm), di lato (±20 cm) e in altezza (±6 cm),
   ognuna con un secondo e mezzo di permanenza. Poi il calcolo, lo zero del
   viso, la verifica del pitch e la presenza. Va fatta la prima volta su ogni
   computer (oppure si copia `camera.json`) e con un altro 4C. Ripetere le fasi
   aggiunge campioni invece di ripartire da zero;
2. **solo il viso**: misura lo **zero personale del pitch**, cioè il pitch
   con la testa dritta mentre si guarda il centro dello schermo, e lo salva in
   `~/.config/tobii4c/headpose-user.json`. Poi verifica che alzando e
   abbassando il mento il pitch cambi nel verso giusto. Serve per un'altra
   persona o un'altra postura;
3. **solo verifica**: pitch e presenza, senza salvare nulla.

Ogni sessione registra landmark e posizioni degli occhi in
`~/.cache/tobii4c/headpose/<data>/`. `tobii4c headpose-calibrate -from DIR1,DIR2`
ricalcola la calibrazione da quelle registrazioni, senza interfaccia. Senza
l'applicazione, `tobii4c run` calibra la camera da solo mentre l'utente si
muove davanti al tracker; il log `head pose: camera calibration not yet
reliable` dice quanto manca.

Il modello del viso **non va addestrato**: è la rete di MediaPipe, già
addestrata da Google, che funziona bene anche sulle immagini IR. Né il
modello né la posa della testa stanno nel 4C, il cui firmware calcola solo
gli occhi.

Cose misurate sul 4C provato (sessioni del 2026-11-04):
- **Camera:** circa 6 cm verso la destra dell'utente rispetto all'origine del
  tracker; asse ottico parallelo all'asse Z del tracker (0,3–0,5°); focale di
  circa 690 px; immagine non speculare.
- **Ritardo:** l'immagine 0x501 ha un timestamp in ritardo di circa 25 ms
  rispetto ai campioni gaze che vedono gli stessi occhi, e il driver lo
  compensa.
- **Punto principale:** i dati raccolti davanti al tracker non distinguono
  uno spostamento del punto principale da una rotazione della camera. Con un
  prior largo il risolutore lo spostava di 260 px e ruotava la camera di 20°,
  e lo yaw dell'immagine differiva di 21° da quello degli occhi. Per questo il
  prior sul punto principale è stretto (5 px).
- **Calibrazione adottata:** errore di 2,8–3,2 px e incertezza di 0,3–0,7°.
  Yaw e roll dell'immagine coincidono con quelli degli occhi entro 1–3°.
- **Verifica del pitch:** +11° con il mento alzato e −21° con il mento
  abbassato.

**Controlli nel log** (`head pose`, ogni `-stats`): immagini ricevute ed
elaborate, percentuale con il viso, millisecondi per immagine, pitch, e
soprattutto `yaw_img_minus_eyes_deg` / `roll_img_minus_eyes_deg`. Questi due
valori sono la differenza tra yaw e roll visti dall'immagine e quelli ricavati
dagli occhi: vicini a zero vuol dire che camera, modello e display area sono
coerenti.

## Output UDP: formato T4CG v2

Un datagramma di **192 byte** per campione, tutto **little-endian**. I primi
120 byte hanno gli stessi offset della v1; i campi nuovi sono accodati.

| Offset | Tipo | Campo |
|---:|---|---|
| 0 | `char[4]` | magic `"T4CG"` |
| 4 | `u8` | versione (2) |
| 5 | `u8` | sorgente della posa della testa: 0 nessuna, 1 derivata, 2 dispositivo |
| 6 | `u16` | flag di validità (bit 0 gaze, 1 gaze_raw, 2/3 occhio L/R tracciato, 4/5 origine L/R, 6 posizione testa, 7 pitch, 8 yaw, 9 roll, 10/11 pupilla L/R, 12/13 gaze 2D L/R) |
| 8 | `u64` | `seq`: monotono, senza buchi tra campioni inoltrati, continuo tra riconnessioni |
| 16 | `i64` | `host_ns`: istante di ricezione USB (CLOCK_REALTIME) |
| 24 | `i64` | `device_us`: orologio del tracker |
| 32 | `f32×2` | sguardo combinato filtrato, normalizzato [0,1] sulla display area (0,0 = in alto a sinistra) |
| 40 | `f32×2` | sguardo combinato non filtrato ("responsive") |
| 48 | `f32×3` | posizione della testa, mm **dal centro dello schermo** (+X destra, +Y su, +Z verso l'utente) |
| 60 | `f32×3` | rotazione della testa, rad (pitch, yaw, roll) attorno agli assi dello schermo; il pitch è valido (bit 7) solo con la stima dalle immagini attiva |
| 72 / 84 | `f32×3` | centro corneale sinistro / destro, mm, sistema del tracker |
| 96 / 100 | `f32` | diametro pupilla sinistra / destra, mm |
| 104 / 112 | `f32×2` | punto di sguardo 2D sinistro / destro |
| 120 | `u32` | flag di validità v2 (bit 0/1 origine schermo L/R, 2/3 track box L/R, 4/5 apertura L/R) |
| 124 | `u8` | presenza: 0 sconosciuta, 1 assente, 2 presente (+ 3 byte a zero) |
| 128 | `i64` | `presence_us`: orologio del tracker all'ultimo messaggio di presenza (0 = nessuno) |
| 136 / 148 | `f32×3` | centro corneale sinistro / destro, mm dal centro dello schermo (= `gaze_origin` dello Stream Engine) |
| 160 / 172 | `f32×3` | posizione dell'occhio sinistro / destro nel track box, normalizzata [0,1] (= `eye_position_normalized`) |
| 184 / 188 | `f32` | apertura della palpebra sinistra / destra, mm |

```python
struct.unpack('<4sBBHQqq2f2f3f3f3f3fff2f2fIB3xq3f3f3f3fff', data)
```

Cosa cambia rispetto alla v1, oltre ai campi accodati:
- la posa della testa è nel sistema dello schermo, non più in quello del tracker;
- i bit 12/13 seguono il tracciamento dell'occhio, perché la colonna 0x1d/0x1e
  usata prima è la validità del track box.

`tobii4c recv` e `examples/recv.py` leggono sia la v1 sia la v2.

Per i pixel basta moltiplicare lo sguardo per la risoluzione del monitor
corrispondente alla display area. Valori fuori da [0,1] indicano uno sguardo
fuori schermo, mentre `(-1,-1)` è la sentinella del dispositivo per "nessuno
sguardo". Usate sempre i flag di validità: quando non vede un occhio, il
dispositivo invia comunque i campi, azzerati (o a −1 per pupille e apertura).
L'apertura ha la validità del firmware e non dipende dal tracciamento
dell'occhio.

LSL: 27 canali float32. I primi 12 sono quelli della v1, con validità
esplicita: `gaze_x, gaze_y, gaze_valid, head_x_mm, head_y_mm, head_z_mm,
head_pitch_rad, head_yaw_rad, head_roll_rad, head_valid, pupil_left_mm,
pupil_right_mm`. Seguono `presence` (0/1/2), `origin_{left,right}_{x,y,z}_mm`
(sistema dello schermo), `trackbox_{left,right}_{x,y,z}` e
`openness_{left,right}_mm`, che valgono NaN quando il dato non è valido, come
`head_pitch_rad` (`head_valid` vale per posizione, yaw e roll). Il
timestamp viene retrodatato all'istante di ricezione USB (`lsl_local_clock() −
latenza pipeline`).

## Logging

`log/slog` su stderr, testo o JSON. Eventi principali: `usb device opened`,
`subscribed`, `gaze columns match keymap` (o un avviso se il firmware dichiara
colonne diverse), `presence` (cambio di stato), `device frames lost` (buchi nel frame counter), `frame
dropped: pipeline saturated`, `ttp stream resync`, `usb device disconnected`,
`gaze stream stalled`, `stats` periodico. Con il pitch: `head pose model
loaded`, `head pose: camera calibrated` / `saved` / `not yet reliable`,
`head pose` periodico, `image stream paused: user away` / `image stream
resumed`.

## Test

- `internal/ttp`: catture reali dell'ET5 (MIT, vedi `internal/ttp/testdata/README.md`).
  Il frame gaze viene decodificato **bit-per-bit uguale** ai valori consegnati
  dallo Stream Engine Windows. Coprono anche riassemblaggio a chunk da 1 a N
  byte, risincronizzazione, risposte frammentate e 0 allocazioni.
- `internal/gaze`: mappatura, invalidazione dell'occhio non tracciato, posa
  della testa nel sistema dello schermo, track box con la sua validità,
  apertura delle palpebre, mappe colonne salvate dalle versioni precedenti,
  decodifica del messaggio presence reale dell'ET5.
- `internal/ttp`: `DisplayArea.ToDisplay` riproduce le colonne "Gaze Origin
  Display" del firmware a partire da quelle del tracker.
- `internal/sink`: round-trip del pacchetto v2, offset dichiarati, lettura dei
  pacchetti v1, canali LSL (NaN per i dati non validi), consegna UDP, 0 allocazioni.
- `internal/ttp` (calibrazione): i comandi collect, compute, read e start sono
  **byte per byte identici** alle catture Windows. Coprono anche un blob da
  659 KB riassemblato da risposte a pezzi, il mounting dell'ET5 e la display
  area ricalcolata come la scrive lo Stream Engine.
- `internal/calib`: flusso completo col simulatore (`-race`), annullamento con
  ripristino del backup, display area rifiutata a calibrazione aperta, occhi
  non rilevati, geometria del mounting del 4C.
- Sul 4C reale: mounting (2 segni a 184 mm, 20°), lettura del blob (335 880
  byte in 188 ms) e riscrittura con rilettura identica byte per byte. Poi
  display area 698 × 393 mm dal setup a frecce, tre calibrazioni complete,
  annullamenti con ripristino verificato dalla notifica 3220 e verifiche fra
  0,61° e 0,99°.
- La pipeline (`internal/tracker`) è verificata sul dispositivo reale (tabella
  in alto). Un unit test copre l'assegnazione della presenza ai frame gaze,
  con i messaggi reali dell'ET5 passati a framer e worker; altri, l'inoltro
  delle immagini allo stimatore e la chiamata di `Observe` prima dei sink.
- `internal/nn`: un grafo ONNX costruito nel test (Transpose, Conv 3×3 a
  passo 2 con pad asimmetrico, PRelu, depthwise, 1×1, Add, MaxPool, Pad dei
  canali, Sigmoid, Reshape, PRelu fuse) confrontato con un'implementazione di
  riferimento banale, con e senza AVX2 e con 1 e 3 goroutine. Il modello reale
  si carica; `make bench` ne misura l'inferenza.
- `internal/headpose`: Procrustes, angoli di Eulero e loro convenzioni,
  ritaglio; una testa sintetica in pose note proiettata con una camera nota
  (errore < 0,11°); calibrazione della camera da coppie sintetiche, con
  immagine diritta e speculare (errore di rotazione 0,25°); stimatore completo
  con un modello finto: si calibra da solo in 13 s di movimento simulato, poi
  il pitch ha un errore massimo di 0,27°. Con `TOBII4C_FACE_IMAGE` il modello
  vero gira su una foto e salva i landmark.
- Sul 4C reale: immagini a 30 Hz, gaze a 90 Hz con latenza p50 di 14–45 µs;
  con l'utente assente la ricerca del viso non parte. Con una persona davanti
  (2026-11-04): viso trovato nel 100% delle immagini, circa 27 elaborate al
  secondo; calibrazione della camera dall'applicazione guidata; pitch +11° /
  −21° con il mento alzato e abbassato; yaw e roll coerenti con gli occhi.
  Un test di regressione copre il bug trovato dal vivo: dopo una ricerca lenta
  il viso deve essere seguito, non ricercato.

## Limiti e prossimi passi

- Non è verificato se il 4C conservi la calibrazione dopo un'interruzione
  dell'alimentazione (la display area la conserva). Se no:
  `tobii4c run -calibration saved`.
- **Pitch della testa**: verificato sul 4C con una persona (vedi
  [Pitch della testa](#pitch-della-testa)). Resta da confrontare con la
  head pose del software Tobii originale, che su Linux non c'è. Costa circa
  due terzi di un core a 30 inferenze al secondo (riducibile con
  `-headpose-rate`). Quando il 4C perde gli occhi, abbassa gli illuminatori:
  il viso non si vede più e il pitch non è disponibile.
- **Apertura delle palpebre**: il firmware del 4C la invia sempre a −1 (come
  l'ET5), quindi su questo dispositivo non è disponibile.
- **Presenza**: sul 4C passa ad "assente" anche quando perde gli occhi per un
  momento (per esempio guardando in basso), non solo quando l'utente se ne va.
- **Con lo stream immagine attivo il 4C salta circa un frame gaze al minuto**
  (0,02%). Misure del 2026-11-04:

  | Configurazione | Durata | Frame persi |
  |---|---|---:|
  | senza immagini | 2 min | 0 |
  | immagini, senza stimatore | 8 min | 6 |
  | immagini con trasferimenti USB da 64 KB × 16 | 8 min | 2 |
  | immagini e stimatore | 8 min | 6 |

  Il frame manca dal dispositivo, non dal PC. Il log riporta il salto
  dell'orologio del tracker (`device_gap_us`, 22,2 ms, cioè un frame) e
  l'intervallo di arrivo sul PC (`host_gap_us`, 22,0–22,2 ms): la ricezione
  non si ferma mai, è il frame che non parte. Per ridurre l'esposizione il
  driver **sospende lo stream immagine dopo 3 s di assenza** dell'utente,
  quando le immagini sono comunque buie, e lo riprende appena l'utente torna.
  `-headpose-images-always` lo tiene sempre attivo.
- Restano non esportate alcune colonne dello stream gaze: punto di sguardo 3D
  per occhio (0x04/0x0a), centro del bulbo oculare (0x17/0x18), posizione 2D
  rispetto al sensore (0x19/0x1a), guida al posizionamento (0x25–0x28) e 0x11,
  senza nome. `tobii4c dump` le mostra.
- Testato su un solo esemplare 4C (firmware 4.7.0). Un firmware diverso con
  colonne diverse viene segnalato all'avvio e si gestisce con `-keymap`.

## Licenza

MIT, vedi [`LICENSE`](LICENSE). I componenti di terze parti inclusi nel
repository sono i dati del modello canonico del viso di MediaPipe (Apache 2.0)
e le catture dell'Eye Tracker 5 di Krazubu/tobii-linux (MIT). Sono elencati,
insieme alle dipendenze, in [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).

Tobii è un marchio di Tobii AB. Questo è un progetto indipendente, non
affiliato, sponsorizzato né approvato da Tobii. Il driver scrive sul
dispositivo calibrazione e display area (della calibrazione fa prima un
backup): usalo a tuo rischio.
