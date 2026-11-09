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
   Dalla linea degli occhi lo **yaw è sottostimato di circa 5 volte** (testa
   girata di 40°: 3–7°) e la parte mancante finisce nel roll. Per questo, con
   la stima dalle immagini attiva, anche yaw e roll vengono dal viso quando è
   tracciato (`-headpose-rotation`, sezione "Pitch della testa").
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
12. **Il firmware espone anche, in sola lettura** (verificato sul 4C il
    2026-11-07): il track box (op 0x578, 8 vertici: 300 × 200 mm a 450 mm,
    550 × 400 mm a 900 mm), proprietà (op 0x532) e stato (op 0x5d2) come
    coppie chiave → testo, e lo stream `log` (0x1772), che si sottoscrive senza
    autenticazione e invia righe di testo del firmware con il timestamp del
    tracker (es. `[ 73592.529468] (I) TDI thread: Changed run mode to 22`).
    `algodbg` (0x1770) arriva a 90 Hz con un blob vuoto. Le colonne 0x19/0x1a
    valgono −1 anche sul 4C con l'utente tracciato. La finestra
    [`telemetry`](#telemetria) mostra tutto.

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
  di gousb con 16 transfer da 64 KiB sempre in coda nel kernel, così il
  dispositivo trova buffer pronti anche mentre il frame precedente viene
  passato avanti: 1 MiB, più di tre immagini IR da 300 KB. Non fa altro che
  leggere e inoltrare.
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
| `internal/telemetry` | finestra di telemetria: raccolta dei dati dalla pipeline (hook, sink, callback di sessione) e cruscotto |
| `internal/tflite` | interprete TFLite in Go puro (flatbuffer, pesi float16 e sparsi): piano NHWC con fusioni, esecuzione sulla GPU (shader SPIR-V) o sulla CPU |
| `internal/vk` | binding Vulkan minimo per il calcolo, caricato a runtime con purego (senza cgo) |
| `internal/bodypose` | MediaPipe Pose: modelli (scaricati e verificati), detector, ritaglio, landmark, filtri, postura e angoli |
| `internal/webcam` | V4L2 in Go puro (ioctl e mmap), scelta della modalità, controlli, decoder MJPEG parallelo senza allocazioni, con la crominanza a richiesta (per `examples/webcam-face` e `examples/webcam-body`) |
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
# Debian / Ubuntu:
sudo apt install git make golang libusb-1.0-0-dev pkg-config   # Go ≥ 1.22
# Arch / Omarchy:
sudo pacman -S --needed git base-devel go libusb pkgconf       # Go ≥ 1.22
git clone https://github.com/ablescia/tobii4c-linux.git
cd tobii4c-linux
make build          # → bin/tobii4c
make test           # vet + unit test (non richiedono il dispositivo)
make bench
```

Su un'installazione di base di Debian 13 mancano sia `git` sia `make`.
`golang` porta Go 1.24 e, tra i pacchetti raccomandati, `gcc`, che serve a cgo
per libusb. Su Arch (e quindi su Omarchy) `base-devel` fornisce `make`, `gcc` e
`pkgconf`, mentre il pacchetto `libusb` include gli header di libusb-1.0; niente
nel driver dipende dalla distribuzione, solo queste dipendenze e, a runtime,
udev/systemd (presenti su entrambe).

In alternativa, senza clonare (servono comunque Go, libusb-1.0 con i suoi header
e pkg-config: `golang`/`libusb-1.0-0-dev`/`pkg-config` su Debian, `go`/`libusb`/
`pkgconf` su Arch):

```sh
go install github.com/ablescia/tobii4c-linux/cmd/tobii4c@latest
export PATH="$PATH:$(go env GOPATH)/bin"   # ~/go/bin, che né Debian né Arch mettono nel PATH
```

Se il repository è privato, Go deve saltare il proxy pubblico e scaricarlo
con `git` (va installato) e le tue credenziali (chiave SSH o token):
`GOPRIVATE=github.com/ablescia/* go install …`.

Per `examples/webcam-body` sulla GPU serve a runtime il loader di Vulkan con
un driver (`libvulkan1` e `mesa-vulkan-drivers` su Debian, `vulkan-icd-loader`
e `vulkan-radeon`/`vulkan-intel` su Arch); in compilazione no. Gli shader sono
già compilati in `internal/tflite/spv`: `glslc` (`shaderc`) serve solo per
rigenerarli dopo una modifica (`make shaders`).

Il modello del viso per il pitch della testa (4,9 MB) è nel repository
(`internal/facemodel`) e viene incorporato nel binario con `go:embed`: non va
scaricato né installato. Per ogni installazione restano da fare solo la
calibrazione della camera e lo zero personale (`tobii4c headpose-calibrate`).

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
`uaccess` viene applicato da `73-seat-late.rules`. Verifica (`getfacl` è nel
pacchetto `acl`, che un'installazione di base non ha):

```sh
sudo apt install acl             # Debian; su Arch/Omarchy `acl` è già presente (dipendenza di systemd)
sudo pacman -S usbutils          # solo Arch/Omarchy: fornisce `lsusb`, che su Debian c'è già
getfacl /dev/bus/usb/$(lsusb -d 2104:0127 | awk '{print $2"/"substr($4,1,3)}')
# deve comparire user:<utente>:rw-
```

`lsusb` serve solo a questa verifica (e al suggerimento che il driver stampa
quando il tracker è occupato): non è una dipendenza del driver. Senza di esso
il nodo si trova anche in sysfs, per esempio
`grep -l 0127 /sys/bus/usb/devices/*/idProduct`.

L'ACL c'è solo finché l'utente ha una **sessione locale attiva**: un desktop
(anche un compositor Wayland come Hyprland su Omarchy) o un login sulla console.
Una sessione SSH da sola non basta, e il driver risponde `libusb: bad access`.
Per un sistema headless o usato solo via SSH c'è nel file, commentata, la
variante `GROUP="plugdev", MODE="0660"`. Su Debian il primo utente è già nel
gruppo `plugdev` (verifica con `id`); su Arch/Omarchy il gruppo non esiste di
default e va creato, aggiungendovi l'utente (il nuovo gruppo ha effetto dal
login successivo):

```sh
sudo groupadd -f plugdev && sudo gpasswd -a "$USER" plugdev   # solo Arch/Omarchy: il gruppo non c'è
sudo sed -i 's/^#\(SUBSYSTEM.*0127.*GROUP="plugdev"\)/\1/' /etc/udev/rules.d/70-tobii-4c.rules
sudo udevadm control --reload-rules
sudo udevadm trigger --action=add --subsystem-match=usb --attr-match=idVendor=2104
```

Nel file c'è anche `MODE="0666"`, permissiva e sconsigliata su macchine
multiutente.

Il detach del driver kernel avviene tramite `SetAutoDetach(true)` di gousb, che
chiama `libusb_detach_kernel_driver` sulle interfacce. Sull'interfaccia 0 di
solito non è legato nessun driver. Al rilascio libusb ricollega il driver
dell'interfaccia reclamata.

## Lab Streaming Layer (facoltativo)

Con LSL serve liblsl. Debian non la pacchettizza, ma il `.deb` ufficiale per
Ubuntu 24.04 funziona anche su Debian 13 (provato con la 1.17.7). Su Arch e
Omarchy liblsl è nell'AUR (pacchetto `liblsl`): `yay -S liblsl` (o un altro
helper AUR) installa header e `.so`, e al suo posto si salta direttamente a
`make build-lsl`. Dalla cartella del repository, con la regola udev già
installata:

```sh
wget -P /tmp https://github.com/sccn/liblsl/releases/download/v1.17.7/liblsl-1.17.7-noble_amd64.deb
echo "9cac7570c2b256077b86c011ffa44b7a40344f2271cc353120c387fb9134ddf1  /tmp/liblsl-1.17.7-noble_amd64.deb" | sha256sum -c -
sudo apt install /tmp/liblsl-1.17.7-noble_amd64.deb
make build-lsl      # equivale a: go build -tags lsl ./cmd/tobii4c
./bin/tobii4c run -lsl &             # outlet "Tobii4C"
./bin/tobii4c lsl-recv -duration 10s # ricevitore di prova: canali, frequenza, NaN, ritardo
kill %1                              # ferma l'outlet, che tiene occupato il tracker
```

`wget` c'è anche su un'installazione di base, `curl` no. Per installare la
build con LSL in `~/.local/bin` usa `make install-lsl`: `make install`
ricompila sempre senza LSL.

## Uso

```sh
./bin/tobii4c info                 # identità, catalogo stream, colonne, display area
./bin/tobii4c info -probe -json /tmp/report.json
./bin/tobii4c run                  # sguardo, occhi, testa, presenza → UDP 127.0.0.1:5005
./bin/tobii4c run -print 500ms -stats 5s
./bin/tobii4c run -lsl -udp ""     # solo LSL (build con -tags lsl)
./bin/tobii4c recv                 # ricevitore UDP di prova (altro terminale)
python3 examples/recv.py           # ricevitore Python, solo libreria standard
go run ./examples/webcam-face      # modello del viso sulla webcam, senza 4C (vedi sotto)
./bin/tobii4c dump -streams 0x504 -frames 2 -duration 5s   # decodifica generica per reverse engineering
./bin/tobii4c dump -images /tmp/ir -frames 0 -duration 3s  # salva frame IR come PGM (due al secondo)
./bin/tobii4c face "$(ls /tmp/ir/*.pgm | head -1)" -out /tmp/viso.png  # modello del viso su un'immagine, PNG annotato
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
| `-headpose` | on | pitch della testa dalle immagini IR: `on`, `off` |
| `-headpose-rate` | 30 | inferenze al secondo al massimo (le immagini arrivano a 30 Hz) |
| `-headpose-workers` | 1 | goroutine dell'inferenza |
| `-headpose-debug` | — | cartella per un'immagine annotata ogni 2 s (landmark, ritaglio, occhi proiettati) |
| `-headpose-images-always` | off | non sospende lo stream immagine quando l'utente è assente |
| `-headpose-rotation` | `image` | yaw e roll della testa: `image` (dal viso quando è tracciato, dagli occhi altrimenti), `eyes` (sempre dagli occhi, come prima) |
| `-headpose-model`, `-headpose-camera` | vedi testo | modello ONNX alternativo (default: quello incorporato) e file della calibrazione della camera |
| `-workers` | 2 | goroutine di decodifica |
| `-raw-queue` | 1 | capacità del canale reader→framer (0 = non bufferizzato) |
| `-frame-pool` | 64 | frame in volo prima di scartare |
| `-in-flight`, `-transfer-size` | 16, 65536 | URB IN in coda nel kernel e relativa dimensione |
| `-stall` | 3s | senza frame: resubscribe, poi riconnessione |
| `-keepalive` | 5s | ping di salute (op 0x58c), RTT nei log debug |
| `-stats` | 10s | log periodico: Hz, latenza p50/p99/max, drop, resync |
| `-display-mm`, `-display-offset-x` | — | scrive la display area all'avvio, es. `698x393`, calcolata col mounting del tracker |
| `-calibration` | — | applica un blob a ogni connessione; `saved` = `~/.config/tobii4c/calibration.bin` |
| `-keymap` | — | mappa colonne JSON alternativa (`tobii4c keymap` stampa quella di default) |
| `-log-level`, `-log-format` | info, text | `slog`; `json` per l'ingestione strutturata |

Come servizio utente systemd:

```sh
make install                # → ~/.local/bin/tobii4c (make install-lsl per LSL)
install -Dm644 systemd/tobii4c.service ~/.config/systemd/user/tobii4c.service
systemctl --user daemon-reload
systemctl --user enable --now tobii4c
journalctl --user -u tobii4c -f   # log JSON
```

Debian mette `~/.local/bin` nel `PATH` dalla login successiva, se la cartella
esiste; su Arch/Omarchy dipende dalla shell e dal profilo, quindi conviene
verificarlo con `echo $PATH` (o usare il percorso completo). Il servizio
`tobii4c.service` è un'unit utente standard: `systemctl --user` funziona
identico su entrambe. Il servizio tiene occupato il tracker: prima di `info`, `calibrate` o
`headpose-calibrate` va fermato con `systemctl --user stop tobii4c`.
Altrimenti questi comandi rispondono `device or resource busy`. La
[telemetria](#telemetria) se ne accorge da sola e chiede di fermarlo.

## Telemetria

```sh
./bin/tobii4c telemetry              # finestra di telemetria col 4C
./bin/tobii4c telemetry -fullscreen  # a schermo intero (il tasto F alterna)
./bin/tobii4c telemetry -simulate -simulate-image "$(ls /tmp/ir/*.pgm | head -1)"  # senza 4C
```

Una sola finestra con, a sinistra, l'anteprima della camera IR (a specchio,
con guadagno automatico) e sotto la posa della testa e lo stato del modello
del viso. A destra, ordinato in schede, c'è tutto ciò che il driver riceve dal
4C:

| Scheda | Contenuto |
|---|---|
| Camera IR | i 478 punti del modello del viso (468 della mesh, 10 delle iridi, colorati per occhio); i contorni o la triangolazione di MediaPipe; il ritaglio del modello; i centri corneali del tracker proiettati con la camera calibrata; le posizioni sul sensore 0x19/0x1a, se il firmware le riempie |
| Testa | pitch, yaw e roll con la loro sorgente (immagine o occhi); posizione nel sistema dello schermo e del tracker; distanza tra le cornee; pitch prima di filtro e zero |
| Modello del viso | probabilità del viso, tempo d'inferenza, immagini ricevute, elaborate e scartate, ricerche, ritardo dell'immagine sul gaze; calibrazione della camera (focale, posizione, inclinazione, qualità); zero del pitch; scarto immagine − occhi |
| Sguardo sullo schermo | punto filtrato con la scia, punto reattivo, sguardo di ciascun occhio, nelle proporzioni della display area |
| Occhi nel track box | vista dall'alto e di lato con il volume restituito dal firmware, la display area e i centri corneali |
| Pupilla e palpebre | ultimi 10 s, per occhio |
| Presenza | stato, valore grezzo e cambi dello stream 0x504 |
| Stream gaze | le 39 colonne così come le invia il firmware, a coppie sinistro/destro e con la loro validità; quelle che il driver non esporta sono segnate |
| Stream del dispositivo | catalogo con stato della sottoscrizione, frequenza e dimensione dei messaggi; colonne di ogni stream; ultima riga degli stream senza scheda propria; notifiche |
| Dispositivo | identità, USB, protocollo e realm, frequenza dichiarata, occhi abilitati, display area, montaggio, track box, proprietà (op 0x532) e stato (op 0x5d2) |
| Comunicazione e pipeline | frequenza; frame persi dal 4C e scartati dall'host; errori e resync; trasferimenti e banda USB; latenza della pipeline; RTT del keep-alive; frame counter e orologio del 4C |
| Registro | le righe del log del firmware (stream 0x1772) e i messaggi del driver |

Tasti: `M` alterna il disegno del viso (punti, punti e contorni, punti e
triangolazione, solo immagine); `N` mostra le colonne con i nomi del firmware
invece delle descrizioni; `P` o spazio mette in pausa; `R` azzera i
contatori; `S` salva una schermata PNG; `F` o `F11` alterna lo schermo
intero; `Esc` o `Q` chiude.

Il driver gira dentro la finestra, che quindi ha bisogno del tracker libero.
All'avvio la telemetria cerca chi lo tiene occupato: `tobii4c.service` attivo,
o un altro servizio utente con il nodo USB aperto (per esempio uno che avvia
`tobii4c run`). Se lo trova, prima di collegarsi chiede nella finestra se
fermarlo (Invio) o uscire (Esc), e alla chiusura lo riavvia. Con
`-stop-service` lo ferma senza chiedere; con `-restart-service=false` non lo
riavvia. Un programma che non è un servizio va chiuso a mano: il registro ne
riporta il PID, e la telemetria si collega appena il tracker è libero. Con
`-udp 127.0.0.1:5005` i campioni continuano ad arrivare anche ai programmi
che leggono l'UDP.

Lo stream immagine resta attivo anche con l'utente assente, perché
l'anteprima è lo scopo della finestra (`-headpose-images-always=false` per
il comportamento di `run`). La finestra chiede di aprirsi massimizzata.
Omarchy ignora apposta queste richieste (`suppress_event = "maximize"`):
lì si usa `F` o `-fullscreen`. Su 1920 × 1080 tutto entra a scala 1; in una
finestra più piccola il cruscotto scala in proporzione.

| Flag | Default | Significato |
|---|---|---|
| `-fullscreen`, `-maximized` | off, on | stato iniziale della finestra |
| `-monitor`, `-size` | primario, monitor | monitor e dimensione iniziale (`LARGHEZZAxALTEZZA`) |
| `-udp` | — | inoltra anche i campioni via UDP |
| `-stop-service`, `-restart-service` | off, on | ferma senza chiedere il servizio che usa il tracker; riavvialo alla chiusura |
| `-firmware-log` | on | sottoscrive lo stream log (0x1772) |
| `-extra-streams` | — | altri stream da mostrare, es. `0x1770` |
| `-keepalive` | 2s | ping di salute, di cui mostra il tempo di risposta |
| `-screenshots`, `-autoshot` | `.`, 0 | cartella delle schermate; salvataggio periodico |
| `-duration` | 0 | chiude dopo questo tempo |
| `-simulate`, `-simulate-image` | off, — | dati simulati senza 4C; immagine passata al modello del viso |
| `-headpose-…` | come `run` | modello, camera, inferenze al secondo |

Verifica sul 4C (2026-11-07), a schermo intero su 1920 × 1080:
- gaze a 90,0 Hz e immagini a 30,0 Hz, nessun frame perso;
- banda USB 9,4 MB/s, RTT del keep-alive 0,32–0,34 ms;
- viso trovato nel 100% delle immagini, inferenza di circa 12,5 ms;
- i centri corneali del tracker, proiettati con la camera calibrata, cadono sui
  centri delle iridi trovati dal modello;
- il processo usa circa il 57% di un core: quasi tutto è il modello del viso,
  il disegno di un frame costa 0,8 ms;
- con un servizio utente che teneva il 4C, la telemetria lo ha trovato dal
  nodo USB, l'ha fermato e si è collegata.

## Viso dalla webcam (esempio)

```sh
go run ./examples/webcam-face                    # modalità più veloce della webcam, a schermo intero
go run ./examples/webcam-face -list              # modalità della webcam e quella che verrebbe scelta
go run ./examples/webcam-face -fps 30            # la risoluzione più grande a 30 fps (1920×1080 sulla C925e)
go run ./examples/webcam-face -size 640x480 -format yuyv -fov 70
go run ./examples/webcam-face -ctrl power_line_frequency=50_hz
go run ./examples/webcam-face -png /tmp/viso.png # senza finestra: schermata dopo 4 s
make webcam-face                                 # → bin/webcam-face
```

L'anteprima del viso della [telemetria](#telemetria), ma su una webcam
qualsiasi invece che sul sensore IR del 4C: stesso modello (MediaPipe Face
Landmarker incorporato in `internal/facemodel`), stesso interprete ONNX in
Go puro, stessa logica di inseguimento e ricerca dello stimatore. La webcam si
legge via V4L2 con ioctl e mmap, senza librerie C. Il 4C non serve e il
driver può restare in esecuzione.

Con `-device auto` prende la prima webcam USB della lista `-usb-id`
(Logitech C925e, poi Sunplus SPCA2281), altrimenti la prima che trova. Senza
altri flag elenca le modalità della webcam (`VIDIOC_ENUM_FRAMESIZES`,
`VIDIOC_ENUM_FRAMEINTERVALS`) e sceglie la **più veloce**, a parità di
frequenza la più grande e poi quella che costa meno convertire (GREY, YUYV,
MJPEG): sulla C925e è MJPEG 1280 × 720 a 60 fps. `-size`, `-format` e `-fps`
restringono la scelta (una risoluzione o una frequenza che la webcam non ha
diventano le più vicine); `-list` stampa le modalità e quella scelta.

Con l'esposizione automatica molte webcam allungano la posa quando c'è poca
luce e scendono sotto la frequenza chiesta: la C925e, chiesta a 60 fps in una
stanza, scende verso 30. Per questo `-fixed-fps` (attivo di default) spegne
`exposure_dynamic_framerate`: la frequenza resta quella chiesta, la posa si
accorcia e sale il guadagno. `-ctrl` imposta altri controlli coi nomi di
`v4l2-ctl` (es. `power_line_frequency=50_hz` per l'antisfarfallio con la rete
a 50 Hz). Tutti i controlli cambiati tornano com'erano all'uscita (anche con
`Ctrl-C`); nella scheda dei controlli sono in verde. Se il processo viene
ucciso con `SIGKILL` restano cambiati fino a quando la webcam non viene
scollegata: all'avvio sono stampati con il valore originale, da rimettere con
`v4l2-ctl -c nome=valore`.

A sinistra l'immagine in **bianco e nero**: in YUYV è il canale Y (la
luminanza) così come arriva dalla webcam, in MJPEG il piano Y del JPEG. Per
l'MJPEG c'è un decoder apposta (`internal/webcam/mjpeg.go`, lo stesso di
`webcam-body`, che lo usa anche per il colore) al posto di `image/jpeg`: qui
decodifica solo la luminanza (i blocchi del colore vengono letti ma non trasformati),
divide il frame ai marker di restart (la C925e ne mette uno ogni 10 MCU, 720
tratti per frame a 720p) tra più goroutine e non alloca memoria; i JPEG che non
gestisce (progressivi, a più scansioni…) passano a `image/jpeg`. Sopra
ci sono i 478 punti, i contorni o la triangolazione, le iridi e il ritaglio
del modello; sotto, gli ultimi 10 s di rotazione della testa, apertura degli
occhi e tempo del modello. A destra la telemetria:

| Scheda | Contenuto |
|---|---|
| Testa | pitch, yaw e roll rispetto all'asse della webcam (convenzioni di `Euler`); centro della testa e distanza dal fit del modello canonico; distanza dal diametro dell'iride (11,7 mm); inclinazione della linea degli occhi |
| Occhi | per occhio: centro e diametro dell'iride, EAR, distanza tra le palpebre in mm, posizione dell'iride nell'occhio; occhi aperti o chiusi, ammiccamenti, distanza tra le iridi |
| Bocca e viso | apertura e larghezza della bocca, MAR, riquadro e centro del viso, scala mm/pixel, luminanza e nitidezza del viso |
| Modello del viso | probabilità, modalità (inseguimento o ricerca a griglia), tempo per frame con media, p95 e massimo, frame elaborati e scartati, ricerche, ritaglio, ritardo cattura → risultato, costo e SHA-256 del modello |
| Webcam | nome, USB (id, produttore, velocità, porta, seriale), driver, formato e spazio colore, come è stata scelta la modalità, frequenza nominale e misurata, sequenza, frame persi, ritardo dall'inizio del frame, banda, conversione (tempo per frame, goroutine della decodifica), buffer, formati disponibili |
| Immagine | istogramma della luminanza con percentili, media, minimo e massimo, pixel neri e saturati |
| Controlli · V4L2 | tutti i controlli della webcam (luminosità, esposizione, bilanciamento…), riletti ogni secondo; in verde quelli cambiati all'avvio |
| Processo | CPU, memoria, heap e GC di Go, goroutine e thread, tempi di disegno della finestra |

Senza un viso il modello fa una ricerca a griglia (fino a una trentina di
inferenze), sempre più di rado finché non trova nulla. Mentre la ricerca è in
corso l'anteprima mostra l'ultimo frame della webcam, senza punti. Pose e
distanze dipendono dalla focale, ricavata da `-fov` (campo visivo
orizzontale, default 60°): non c'è una calibrazione, quindi le distanze
assolute valgono quanto quel valore.

Tasti: `M` alterna il disegno del viso; `V` l'anteprima a specchio; `P` o
spazio mette in pausa; `R` azzera i contatori; `S` salva una schermata PNG;
`F` o `F11` alterna lo schermo intero; `Esc` o `Q` chiude.

| Flag | Default | Significato |
|---|---|---|
| `-device`, `-usb-id` | auto, `046d:085b,1bcf:2281` | nodo V4L2; con `auto` la prima webcam USB con un id della lista, altrimenti la prima |
| `-size`, `-format`, `-fps` | auto, auto, 0 | modalità: la più veloce, poi la più grande; oppure risoluzione, formato (`yuyv`, `mjpeg`, `grey`) e frequenza chiesti |
| `-list` | off | elenca le modalità della webcam e quella scelta, poi esce |
| `-fixed-fps` | on | frequenza fissa anche con poca luce (`exposure_dynamic_framerate` spento) |
| `-ctrl` | — | controlli V4L2 `nome=valore,…` coi nomi di `v4l2-ctl`; ripristinati all'uscita |
| `-decoders`, `-buffers` | 0 (un quarto dei core, al più 8), 4 | goroutine della decodifica MJPEG; buffer mmap del driver |
| `-fov` | 60 | campo visivo orizzontale in gradi, per la focale |
| `-mirror` | on | anteprima a specchio |
| `-window`, `-monitor` | off, primario | finestra normale invece dello schermo intero; monitor |
| `-model`, `-workers` | incorporato, 0 (fino a 4) | modello ONNX e goroutine dell'inferenza |
| `-shots` | `.` | cartella delle schermate |
| `-png`, `-png-size`, `-duration` | —, 1920x1080, 0 | interfaccia salvata in PNG senza X; chiusura dopo un tempo |

Verifica con la Logitech C925e (2026-11-09, USB 2.0, Ryzen 9 5950X), senza
flag, cioè MJPEG 1280 × 720 a 60 fps:
- 59,0–60,0 fps misurati, nessun frame perso dal driver né illeggibile,
  144–147 KiB per frame (8,7–9,0 MB/s), 6 ms dall'inizio del frame
  all'applicazione;
- luminanza dal JPEG in 0,79–0,83 ms per frame con 8 goroutine;
- senza `-fixed-fps` la stessa webcam, chiesta a 60 fps, rallenta:
  `v4l2-ctl --stream-mmap` stampa da 58,5 a 35,5 fps, in calo, in 200 frame;
  con `exposure_dynamic_framerate` spento sale fino a 59,5;
- con `-fps 30` (1920 × 1080): 30,00 fps, nessun frame perso, luminanza in
  1,6 ms, viso seguito nel 100% dei frame con 6,6 ms d'inferenza in media
  (p95 7,4 ms, massimo 8,5 ms, 4 goroutine);
- il decoder su 240 frame veri a 720p e 60 a 1080p: rispetto a `image/jpeg`
  l'1,2–1,4% dei pixel differisce di un livello (arrotondamento dell'IDCT),
  nessuno di più. Per frame (`go test -run '^$' -bench MJPEG ./internal/webcam`):

| | 1280 × 720 | 1920 × 1080 |
|---|---|---|
| `image/jpeg` (colore compreso) | 8,6 ms · 1,9 MB e 739 allocazioni | 19,5 ms · 4,2 MB e 1607 allocazioni |
| solo Y, 1 goroutine | 3,0 ms · nessuna allocazione | 6,7 ms |
| solo Y, 4 goroutine | 0,95 ms | 2,0 ms |
| solo Y, 8 goroutine (default qui) | 0,57 ms | 1,2 ms |

Verifica con la Sunplus SPCA2281 (2026-11-08), 640 × 480 YUYV, con i default
di allora (`-workers 1`):
- viso trovato e seguito frame per frame, 12–13 ms d'inferenza, circa 25% di
  un core per tutto il processo;
- estrazione della luminanza in circa 176 µs per frame, nessun frame perso;
- in una stanza poco illuminata la webcam scende da sola a circa 17 fps
  (esposizione automatica): `v4l2-ctl --stream-mmap` misura lo stesso valore;
- ricerca a griglia senza viso in 290–360 ms.

## Scheletro dalla webcam (esempio)

```sh
go run ./examples/webcam-body                    # corpo intero sulla GPU, a schermo intero
go run ./examples/webcam-body -mode busto        # mezzo busto, da seduto
go run ./examples/webcam-body -variant heavy     # corpo intero col modello più preciso
go run ./examples/webcam-body -backend cpu       # senza GPU
go run ./examples/webcam-body -png /tmp/corpo.png  # senza finestra: schermata dopo 5 s
make webcam-body                                 # → bin/webcam-body
```

Lo scheletro di una persona da una webcam qualsiasi, con i modelli di Google
MediaPipe Pose eseguiti **sulla GPU** (Vulkan) da un interprete TFLite in Go
puro, senza librerie C né runtime di Google. Un solo programma per le due
tipologie di rilevamento, scelte con i **radio button** in alto a sinistra
(o con i tasti):

| Tipologia | Modello | Punti |
|---|---|---|
| **Corpo intero**, in piedi o seduto | MediaPipe Pose Landmarker (BlazePose GHUM 3D), varianti `lite`, `full`, `heavy` | 33, con le coordinate 3D in metri e la maschera della persona |
| **Mezzo busto**, da seduto | il modello "upper body" di MediaPipe Pose 0.8.3, addestrato sulla sola parte superiore del corpo | 25 (testa, spalle, braccia, mani, fianchi) e la maschera; niente 3D |

Accanto alla tipologia, la variante del corpo intero (`lite`, `full`,
`heavy`) e il calcolo (GPU o CPU). Il cambio vale dal frame successivo: i
modelli non ancora usati vengono caricati (e, la prima volta, scaricati)
mentre l'anteprima continua; se il caricamento fallisce resta la
configurazione di prima e la scheda dei comandi mostra l'errore.

I modelli sono i file ufficiali: i `.task` di Pose Landmarker da
`storage.googleapis.com/mediapipe-models` (float16, versione 1) e i due
`.tflite` del mezzo busto dal tag `0.8.3.2` del repository di MediaPipe.
Vengono scaricati al primo uso in `~/.cache/tobii4c/models` (`-models` per
un'altra cartella, `-offline` per non scaricare) e verificati con lo SHA-256
fissato nel codice (`internal/bodypose/models.go`); non sono nel repository.

La pipeline replica i grafi di MediaPipe (`internal/bodypose`): sul frame
intero il detector SSD (ancore, NMS pesata, letterbox), dal centro dei fianchi
e dal punto di scala (per il mezzo busto: centro delle spalle e scala del
busto) un ritaglio quadrato ruotato, la rete dei landmark sul ritaglio,
raffinamento dei punti sulla heatmap, proiezione nell'immagine e filtri One
Euro con gli stessi parametri. Nei frame successivi il ritaglio viene dai
punti ausiliari del frame precedente (inseguimento) e il detector torna in
gioco solo quando la presenza della persona scende sotto 0,5. Una persona
alla volta: la più probabile.

Il calcolo (`internal/tflite`) legge i flatbuffer TFLite direttamente (pesi
float16 e sparsi compresi), compila il grafo in un piano NHWC (PAD fusi nelle
convoluzioni, somme residue fuse nella convoluzione che le precede, le teste
fully-connected come prodotti di matrici, memoria dei tensori riusata) e lo
esegue sulla GPU con shader di calcolo GLSL compilati in SPIR-V e incorporati
nel binario. Vulkan si carica a runtime con
[purego](https://github.com/ebitengine/purego) (`internal/vk`, niente cgo):
ogni modello è un command buffer registrato una volta e rieseguito, l'immagine
della webcam (piani Y, Cb, Cr) viene copiata una volta per frame e il ritaglio
ruotato, la conversione in RGB e la normalizzazione avvengono sulla GPU. Lo
stesso piano gira anche sulla CPU (kernel AVX2/FMA di `internal/nn`), come
ripiego se Vulkan manca e come riferimento.

A sinistra l'immagine **a colori** (il decoder MJPEG del viso, esteso alla
crominanza, in `internal/webcam`) con lo scheletro colorato per lato del
soggetto, la maschera della persona sfumata, il ritaglio della rete e il
riquadro del detector; sotto, gli ultimi 10 s di angoli articolari,
inclinazioni del busto e tempo dei modelli. A destra:

| Scheda | Contenuto |
|---|---|
| Postura | in piedi, seduto, accovacciato o sdraiato (dall'angolo di cosce e busto in 3D); busto inclinato di lato, in avanti e ruotato; spalle e testa inclinate; mani alzate; distanza dalla camera; punti visibili |
| Articolazioni | spalle, gomiti, anche e ginocchia, sinistra e destra, in 3D (corpo intero) o nel piano dell'immagine (mezzo busto) |
| Scheletro 3D | i punti in metri visti di fronte e di lato (solo corpo intero) |
| Modello | presenza della persona, ritaglio (dal detector o dall'inseguimento), tempi di detector e landmark con media, p95 e massimo, frame elaborati e scartati, nodi e costo delle reti, file verificati |
| Calcolo | GPU (nome, driver, Vulkan, memoria), tempo sulla GPU dai timestamp di Vulkan, attesa sulla CPU, GFLOP/s; oppure CPU; memoria dei modelli e tempo di caricamento |
| Webcam, Controlli · V4L2, Processo | come in webcam-face |

Un punto conta come visibile solo con visibilità **e** presenza almeno 0,5,
come nel disegno di MediaPipe: con la persona fuori dall'inquadratura la rete
stima comunque tutti i punti, ma quelli fuori dall'immagine restano esclusi
(nell'anteprima sono tenui). La distanza viene dalla focale ricavata da `-fov`
(default 70°, la C925e): col corpo intero dal rapporto tra le lunghezze del
busto in metri e in pixel, col mezzo busto ipotizzando spalle larghe 39 cm.

Tasti: `B` o `Tab` alterna corpo intero e mezzo busto; `1`, `2`, `3` scelgono
`lite`, `full`, `heavy`; `G` alterna GPU e CPU; `M` il disegno (scheletro,
scheletro e maschera, maschera, solo immagine); `V`, `P`, `R`, `S`, `F`,
`Esc` come in webcam-face.

| Flag | Default | Significato |
|---|---|---|
| `-mode` | `corpo` | `corpo` (corpo intero) o `busto` (mezzo busto) |
| `-variant` | `full` | modello del corpo intero: `lite`, `full`, `heavy` |
| `-backend`, `-workers` | `gpu`, 0 | `gpu` (Vulkan, con ripiego sulla CPU) o `cpu`; goroutine sulla CPU (0: una per core, al più 16) |
| `-models`, `-offline` | `~/.cache/tobii4c/models`, off | cartella dei modelli; non scaricare quelli mancanti |
| `-fov` | 70 | campo visivo orizzontale in gradi, per la distanza |
| `-device`, `-usb-id`, `-size`, `-format`, `-fps`, `-fixed-fps`, `-ctrl`, `-list`, `-decoders`, `-buffers`, `-mirror`, `-window`, `-monitor`, `-shots`, `-png`, `-png-size`, `-duration` | | come in webcam-face (`-format grey` toglie il colore; `-duration` con `-png`: 5 s) |

Variabili d'ambiente di `internal/vk`: `TOBII4C_VK_DEVICE=n` sceglie l'n-esima
GPU, `TOBII4C_VK_ALLOW_CPU=1` ammette le implementazioni software (llvmpipe),
`TOBII4C_VK_VALIDATION=1` attiva i validation layer se installati.

Verifica (2026-11-09, Ryzen 9 5950X, Radeon RX 6800 XT con RADV di Mesa
26.2.5, Logitech C925e in MJPEG 1280 × 720 a 60 fps):
- le sei reti (detector e landmark dei due modelli, le tre varianti) danno le
  stesse uscite di LiteRT (`ai-edge-litert`, XNNPACK) sullo stesso ingresso:
  scarto relativo massimo 8,4·10⁻⁵ sulla CPU e sulla GPU;
- la pipeline completa (detector, ritaglio, landmark, heatmap, 3D) su due foto
  NASA di 512 × 640 e 640 × 422 pixel, confrontata con MediaPipe 0.10.14 in
  modalità IMAGE: scarto medio dei punti visibili 1,5–5,4 pixel (massimo
  16 px), coordinate 3D a 5–64 mm in media. Le reti sono identiche, ma
  MediaPipe campiona il ritaglio con `cv::warpPerspective` a 8 bit e qui si
  campiona in float: detector e landmark reagiscono a queste differenze
  minime (emulare l'arrotondamento di OpenCV sposta gli scarti senza ridurli);
- GPU e CPU danno gli stessi punti a meno di 0,03 pixel;
- dal vivo, corpo intero `full` sulla GPU: 59,0 fps dalla webcam, tutti
  elaborati, nessuno scartato; 2,8 ms per frame tra modelli e filtri, di cui
  1,9 ms sulla GPU (landmark a 411 GFLOP/s); 4,0 ms dalla cattura al
  risultato; heap di Go 27 MiB col mezzo busto caricato;
- tempo per inferenza, ingresso già pronto
  (`go test -run '^$' -bench Models ./internal/tflite`, 100 esecuzioni; CPU
  con 16 goroutine; per la GPU il tempo totale dall'invio alla fine
  dell'attesa e, tra parentesi, quello dei timestamp di Vulkan):

| Rete | MMAC | GPU | CPU |
|---|---:|---:|---:|
| detector del corpo intero (224 × 224) | 430 | 0,97 ms (0,91) | 12,6 ms |
| landmark `lite` (256 × 256) | 198 | 0,98 ms (0,91) | 17,2 ms |
| landmark `full` | 386 | 1,23 ms (1,16) | 20,2 ms |
| landmark `heavy` | 1 927 | 3,41 ms (3,34) | 57,8 ms |
| detector del mezzo busto (128 × 128) | 469 | 1,07 ms (1,01) | 22,4 ms |
| landmark del mezzo busto (256 × 256) | 424 | 1,40 ms (1,32) | 24,6 ms |

Il mezzo busto è il modello del 2020: Google lo ha tolto da MediaPipe 0.8.4
perché il corpo intero copre anche la sola parte superiore. Qui resta perché
è addestrato su quell'inquadratura e il ritaglio è centrato sulle spalle, non
sui fianchi fuori dall'immagine, ma il suo detector è più debole: su un primo
piano con la testa molto inclinata sbaglia l'orientamento del ritaglio, dove
il corpo intero no. Entrambi vogliono le spalle nell'inquadratura: con la
webcam alta che riprende solo la testa il mezzo busto non trova nessuno e il
corpo intero aggancia solo il viso.

## Come calibrare correttamente

Le calibrazioni sono due e diverse: una riguarda il dispositivo, l'altra la
persona che lo usa.

### Calibrazione della camera: una sola persona, una volta sola

Serve solo al pitch della testa. Misura la camera IR del 4C: focale, centro
ottico e posizione della camera rispetto al tracker. Sono proprietà
dell'hardware, non di chi sta davanti al sensore. Basta quindi che **una sola
persona la faccia una volta**, per esempio chi installa il sistema o chi
assiste l'utente. Non dipende né dal monitor né dalla calibrazione dello
sguardo.

1. Con il 4C montato, avvia `tobii4c headpose-calibrate` e scegli
   **1 (calibrazione completa)**.
2. Muoviti con calma finché si riempiono le caselle della distanza (50–85 cm),
   di lato (±20 cm) e in altezza (±6 cm). Il movimento della testa è
   indispensabile: a testa ferma i parametri della camera non si possono
   ricavare.
3. Il programma accetta il risultato solo se l'errore di riproiezione è sotto
   4 px e l'incertezza della rotazione sotto 1°. Con Invio lo salva in
   `~/.config/tobii4c/camera.json`, legato al numero di serie del 4C.

Va rifatta solo se si cambia 4C. Su un altro computer con lo stesso 4C basta
copiare `camera.json`. Chi non usa il pitch può saltarla
(`tobii4c run -headpose off`): lo sguardo funziona comunque. I dettagli sono in
[Pitch della testa](#pitch-della-testa).

### Calibrazione dello sguardo: personale

La calibrazione dello sguardo adatta il modello degli occhi a una persona
precisa. Quindi la fa **chi userà il tracker**, con i propri occhi. È la stessa
procedura dell'applicazione ufficiale Tobii: posizione del sensore, 7 punti da
fissare e verifica (dettagli in
[Configurazione e calibrazione](#configurazione-e-calibrazione)):

```sh
tobii4c calibrate
```

Basta guardare i punti, la testa non si deve muovere. I tasti (Spazio, Invio)
può premerli chi assiste. La calibrazione va rifatta **ogni volta che cambia la
persona**, e anche se cambiano il monitor, la posizione del sensore, gli
occhiali o le lenti a contatto. Resta attiva sul 4C e viene salvata in
`~/.config/tobii4c/calibration.bin`. `tobii4c run -calibration saved` la
riapplica a ogni connessione: non è ancora verificato se il 4C la conservi
quando resta senza alimentazione. Con più persone si tiene un file per
ciascuna: `tobii4c calibration save -f persona1.bin`, poi
`tobii4c calibration apply -f persona1.bin`.

Anche lo **zero personale del pitch** appartiene alla persona. Si misura dopo la
calibrazione della camera: in `tobii4c headpose-calibrate` si sceglie
**2 (solo il viso)** e si guarda il punto al centro con la testa dritta. I passi
successivi, con il mento in su e in giù, sono solo un controllo e vanno avanti
da soli. La verifica della presenza che segue si salta con S. Anche la
calibrazione completa misura alla fine uno zero, ma è quello di chi l'ha
eseguita: se è stata un'altra persona, l'utente rifà il passo 2.

## Configurazione e calibrazione

`tobii4c calibrate` apre una finestra a schermo intero sul monitor primario
(`-monitor HDMI-0` per sceglierne un altro, `-monitors` per elencarli). Come
`headpose-calibrate`, richiede una sessione grafica (X11, oppure Wayland con
XWayland): senza, termina con `connessione a X11 ($DISPLAY)`. Ripropone i
passi dell'applicazione ufficiale Tobii:

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
provare l'interfaccia, anche su un display virtuale (`sudo apt install xvfb`,
su Arch/Omarchy `sudo pacman -S xorg-server-xvfb`):
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
   ONNX, incorporata nel binario (`internal/facemodel`) ed eseguita da `internal/nn`, un interprete in Go puro dei soli
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
   (entro 150 ms dall'immagine), con il bit 7 dei flag UDP. Con
   `-headpose-rotation image` (default) sostituisce anche yaw e roll ricavati
   dagli occhi con quelli del viso; senza viso tracciato restano quelli degli
   occhi. I bit 6/7/8 dei flag v2 dicono quali angoli vengono dalle immagini.

**Perché yaw e roll dalle immagini.** Il 4C ha una sola camera, sotto lo
schermo, e dei due centri corneali misura bene la posizione sul piano
dell'immagine ma non quale dei due è più vicino. Lo yaw, che dipende proprio
da quella differenza di profondità, esce dalla linea degli occhi sottostimato,
e la differenza di profondità diventa in parte una differenza di altezza,
cioè roll. Misure con una persona davanti (2026-11-04, `-stats 1s`):

| movimento | viso: yaw / roll | occhi: yaw / roll |
|---|---|---|
| testa dritta | +5° / −1° | 0° / 0° |
| girata a sinistra | +42° / −8° | +3…+7° / +10° |
| girata a destra | −31° / +2° | −8…−10° / −9° |
| inclinata a destra | −5° / −28° | −6° / −28° |

Con la testa girata il viso resta tracciato nel 100% delle immagini, mentre
oltre 25° circa il tracker perde gli occhi, e con loro lo sguardo e la
posizione. Per l'inclinazione pura gli occhi sono precisi quanto il viso.
Con la testa dritta lo yaw del viso ha uno scarto fisso di qualche grado,
che dipende dalla calibrazione della camera: le applicazioni lo tolgono con
il loro punto neutro.

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
  Con la testa dritta yaw e roll dell'immagine coincidono con quelli degli
  occhi entro 1–3°; con la testa girata no (vedi sopra).
- **Verifica del pitch:** +11° con il mento alzato e −21° con il mento
  abbassato.

**Controlli nel log** (`head pose`, ogni `-stats`): immagini ricevute ed
elaborate, percentuale con il viso, millisecondi per immagine, pitch, e
soprattutto `yaw_img_minus_eyes_deg` / `roll_img_minus_eyes_deg`. Questi due
valori sono la differenza tra yaw e roll visti dall'immagine e quelli ricavati
dagli occhi: vicini a zero **con la testa dritta** vuol dire che camera,
modello e display area sono coerenti. Girando la testa lo scarto dello yaw
cresce, perché gli occhi lo sottostimano. `yaw_img_deg` e `roll_img_deg` sono
gli angoli del viso nell'ultima immagine.

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
| 60 | `f32×3` | rotazione della testa, rad (pitch, yaw, roll) attorno agli assi dello schermo; il pitch è valido (bit 7) solo con la stima dalle immagini attiva, che di default fornisce anche yaw e roll (flag v2, bit 6/7/8) |
| 72 / 84 | `f32×3` | centro corneale sinistro / destro, mm, sistema del tracker |
| 96 / 100 | `f32` | diametro pupilla sinistra / destra, mm |
| 104 / 112 | `f32×2` | punto di sguardo 2D sinistro / destro |
| 120 | `u32` | flag di validità v2 (bit 0/1 origine schermo L/R, 2/3 track box L/R, 4/5 apertura L/R, 6/7/8 pitch/yaw/roll della testa dal viso nelle immagini invece che dagli occhi) |
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
gli angoli della testa. `head_valid` vale per la posizione: yaw e roll dal
viso possono essere validi anche quando il tracker vede un occhio solo. Il
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
- `internal/tflite`: le sei reti di MediaPipe Pose (dalla cache dei modelli)
  confrontate con le uscite di LiteRT salvate in `testdata` (stesso ingresso
  generato in Go e in numpy), sulla CPU e sulla GPU; un modello sintetico con
  tutti gli operatori e le fusioni (PAD nella convoluzione, ADD residua,
  depthwise, pooling, ridimensionamento, depth-to-space, concatenazione,
  fully-connected) uguale su GPU e CPU; il warp dell'immagine YCbCr con bordo
  nero e replicato; la conversione dei float16. `BenchmarkModels` misura le
  reti.
- `internal/bodypose`: la pipeline completa su due foto NASA confrontata con
  MediaPipe 0.10.14 per le tre varianti; GPU uguale alla CPU sui punti e sulla
  maschera; ancore, ritaglio ruotato e warp, NMS pesata, filtro One Euro,
  angoli, distanza e postura (in piedi, seduto, sdraiato) su pose sintetiche.
- `internal/vk`: copia dalla CPU alla memoria della GPU e ritorno con un
  programma rieseguito, riempimento e timestamp.
- `internal/webcam` (condiviso da `webcam-face` e `webcam-body`): numeri degli
  ioctl V4L2 uguali a quelli del kernel a 64 bit; estrazione della luminanza
  e dei colori da YUYV con padding di riga e frame troncati; scelta della
  modalità sull'elenco della C925e (la più veloce, la più grande a 30 fps, la
  risoluzione o la frequenza più vicine, YUYV a parità di modalità); nomi e
  valori dei controlli come in `v4l2-ctl`; il decoder MJPEG confrontato con
  `image/jpeg` su JPEG scritti dal test con restart, 4:2:2, 4:2:0, 4:4:4,
  grigio, bordi parziali e codici di Huffman fino a 16 bit, a una e più
  goroutine, senza allocazioni, sia per la luminanza sia per Cb e Cr (scarto
  massimo un livello); frame troncati, rovinati a caso e progressivi (errore o
  `image/jpeg`, mai un panic; c'è anche `FuzzMJPEG`). Con `WEBCAM_FACE_MJPEG`
  (uno stream salvato da `v4l2-ctl --stream-to`) confronta e misura il
  decoder su frame veri.
- `examples/webcam-body`: clic sui radio button (tipologia, variante
  disabilitata col mezzo busto, GPU assente), cambio di modello a caldo nel
  motore con un frame vero, interfaccia disegnata senza X (con
  `TOBII4C_BODY_PNG=<dir>` salva le schermate), segmenti spessi ritagliati.
  I test che usano i modelli si saltano se la cache è vuota.
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
  −21° con il mento alzato e abbassato; yaw e roll coerenti con gli occhi a
  testa dritta. Girando la testa il viso dà ±31–42°, gli occhi solo 3–10°:
  da qui `-headpose-rotation image`.
  Un test di regressione copre il bug trovato dal vivo: dopo una ricerca lenta
  il viso deve essere seguito, non ricercato.
- `internal/telemetry`: le 39 colonne del 4C ordinate in 16 righe (coppie
  sinistro/destro e validità, nessuna esclusa); il cruscotto disegnato senza
  X a più dimensioni, da 300 × 200 a 3840 × 2160; la domanda sul servizio
  (azione fuori dal loop, errore, nuovo tentativo, `Esc`); il simulatore che
  scrive mentre l'interfaccia legge (`-race`). Con `TOBII4C_TELEMETRY_PNG` e
  `TOBII4C_FACE_IMAGE` salva le schermate con un viso vero. `internal/ttp`
  ricodifica il frame gaze dell'ET5 byte per byte uguale e decodifica track
  box, proprietà e log catturati dal 4C. `internal/x11ui` verifica che la
  cache dei glifi disegni pixel per pixel come il rasterizzatore.
- `examples/webcam-face`: ammiccamenti contati, e nessuno se lo sguardo resta basso; il
  modello vero su un frame e l'interfaccia disegnata senza X e senza webcam a
  più dimensioni, compreso il passaggio all'anteprima diretta durante una
  ricerca. Con `TOBII4C_FACE_IMAGE` usa un viso vero, con `WEBCAM_FACE_PNG`
  salva le schermate.
- Installazione da zero (2026-11-04) su una VM Debian 13.7 appena installata,
  senza desktop (VirtualBox 7.1, 4C in passthrough USB). Tutti i blocchi
  `sh` di questo README, eseguiti così come sono scritti e nell'ordine in cui
  compaiono, terminano senza errori:
  - build, test e benchmark;
  - `go install`;
  - udev: `uaccess` con un login locale, variante `plugdev` via SSH;
  - LSL a 89,9 Hz;
  - le 17 righe di [Uso](#uso). `calibrate` e `headpose-calibrate` sono
    stati provati su Xvfb. `calibration apply`/`restore` lasciano il blob
    sul dispositivo identico byte per byte;
  - servizio systemd;
  - hotplug con riconnessione;
  - `camera.json` copiato da un altro computer.

  Con le immagini sempre attive: 2 minuti di gaze a 90 Hz senza frame persi
  e immagini IR a 30 Hz.

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
  | immagini, senza stimatore, trasferimenti USB 16 KB × 8 | 8 min | 6 |
  | immagini, trasferimenti USB 64 KB × 16 (default attuale) | 8 min | 2 |
  | immagini e stimatore, trasferimenti USB 16 KB × 8 | 8 min | 6 |

  Il frame manca dal dispositivo, non dal PC. Il log riporta il salto
  dell'orologio del tracker (`device_gap_us`, 22,2 ms, cioè un frame) e
  l'intervallo di arrivo sul PC (`host_gap_us`, 22,0–22,2 ms): la ricezione
  non si ferma mai, è il frame che non parte. Per ridurre l'esposizione il
  driver **sospende lo stream immagine dopo 3 s di assenza** dell'utente,
  quando le immagini sono comunque buie, e lo riprende appena l'utente torna.
  `-headpose-images-always` lo tiene sempre attivo.

  I trasferimenti in coda contano quando la latenza USB cresce, per esempio
  in una macchina virtuale. In una VM VirtualBox con il 4C in passthrough e
  le immagini sempre attive, i vecchi 16 KB × 8 (128 KB in coda, meno di
  un'immagine IR da 307 KB) perdevano l'**1,4%** dei frame gaze: 51 su 3 522
  in 40 s, a gruppi di 6–9. Il `host_gap_us` di 3 ms indica che i dati si
  perdevano nel trasporto. Con 64 KB × 16, il default dal 2026-11-04, la VM ne
  ha persi 2 in 2 minuti, come il PC. Sul PC le due configurazioni si
  equivalgono: 0 frame persi in 2 minuti ciascuna, latenza p50 di 39–46 µs.
- Restano non esportate alcune colonne dello stream gaze: punto di sguardo 3D
  per occhio (0x04/0x0a), centro del bulbo oculare (0x17/0x18), posizione 2D
  rispetto al sensore (0x19/0x1a), guida al posizionamento (0x25–0x28) e 0x11,
  senza nome. `tobii4c dump` e la finestra `telemetry` le mostrano.
- **Scheletro dalla webcam** (`examples/webcam-body`): una persona alla
  volta; il mezzo busto è il modello del 2020, più debole del corpo intero sui
  primi piani molto inclinati; le distanze dipendono da `-fov`; la GPU serve
  Vulkan 1.1 (provato solo su RADV con una Radeon RX 6800 XT). I modelli si
  scaricano al primo uso: senza rete e senza cache il programma non parte.
- Testato su un solo esemplare 4C (firmware 4.7.0). Un firmware diverso con
  colonne diverse viene segnalato all'avvio e si gestisce con `-keymap`.

## Licenza

MIT, vedi [`LICENSE`](LICENSE). I componenti di terze parti inclusi nel
repository sono i dati del modello canonico del viso e le connessioni della
mesh di MediaPipe (Apache 2.0)
e le catture dell'Eye Tracker 5 di Krazubu/tobii-linux (MIT). Sono elencati,
insieme alle dipendenze, in [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).

Tobii è un marchio di Tobii AB. Questo è un progetto indipendente, non
affiliato, sponsorizzato né approvato da Tobii. Il driver scrive sul
dispositivo calibrazione e display area (della calibrazione fa prima un
backup): usalo a tuo rischio.
