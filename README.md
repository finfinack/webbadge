# webbadge

Web Badge / Badge Studio is a standalone Go web application for preparing and
transferring images to 460 × 768, four-color BOE e-ink badges, either over
Bluetooth Low Energy from the browser or over NFC through a local ACS
ACR1552 PC/SC reader.

<img width="600" alt="image" src="https://github.com/user-attachments/assets/b13720d1-c314-4fe2-b78e-a717ea502022" />

The Go server:

- embeds and serves the frontend
- accepts bounded PNG and JPEG uploads
- validates image dimensions before full decoding
- resizes with `contain` or `cover` behavior
- applies four-color Floyd–Steinberg dithering
- returns an exact badge preview and the packed 88,320-byte image

The browser performs the BLE transfer through Web Bluetooth. Alternatively,
the same confirmed image can be written over NFC: the Go process itself
talks to a locally attached ACR1552 reader (PC/SC over cgo) and streams
progress back to the browser, since PC/SC is not reachable from a web page.
Images and badge data are not persisted by the server.

Disclaimer: Almost all of this is AI generated. I would strongly advise against exposing this webserver anywhere else than localhost. This is doubly true for the NFC write path below, since any request that reaches this server can trigger a physical write on whatever badge is sitting on the reader.

## Requirements

- Chrome or Edge on macOS, Windows, ChromeOS, or supported Android devices
- a BLE badge exposing the Nordic-UART-style service described below, and/or
  an ACS ACR1552 PC/SC reader for the NFC transfer described below

Web Bluetooth works from `http://localhost` during development.

## Run

### Locally

```sh
go run .
```

Open <http://127.0.0.1:8080>, prepare and confirm an image, choose the badge,
then start the transfer.

To listen on a different address:

```sh
go run . -address 0.0.0.0:8080
```

Logging defaults to `info`. Browser BLE diagnostics are debug-only and can be
enabled when troubleshooting:

```sh
go run . -log debug
```

An HTTP LAN address is not a secure browser context and therefore cannot use
Web Bluetooth. Put a LAN deployment behind an HTTPS reverse proxy.

### Test

```sh
go test -race ./...
go vet ./...
```

### Docker

The Dockerfile accepts two optional build arguments (`ARG`) to inject version metadata into the Go application:

* `tag`: The version or release tag (Defaults to `latest`).
* `commit`: The specific Git commit SHA (Defaults to `-`).

#### Build

To build the image with default arguments:

```bash
docker build -t homedisplay:latest .
```

To pass a specific version and the current Git commit string into the application:

```bash
docker build \
  --build-arg tag="v1.2.3" \
  --build-arg commit=$(git rev-parse --short HEAD 2>/dev/null || echo "development") \
  -t homedisplay:v1.2.3 .
```

#### Run

Adjust the parameters as you see fit:

```bash
docker run --rm -it -p 8080:8080 homedisplay:latest --log info --address 0.0.0.0:8080
```

## Badge protocol

| Purpose | UUID |
| --- | --- |
| Service | `6e400001-b5a3-f393-e0a9-e50e24dcca9e` |
| Browser writes | `6e400002-b5a3-f393-e0a9-e50e24dcca9e` |
| Badge notifications | `6e400003-b5a3-f393-e0a9-e50e24dcca9e` |

The transfer uses the same byte stream as the ESP32 implementation. For Chrome
on macOS, data is segmented into conservative 120-byte ATT writes, below the
typical 182-byte CoreBluetooth payload limit. Chrome does not expose controller
backpressure, so the browser sends bounded bursts and uses every sixteenth
chunk as an acknowledged queue barrier. Transient unacknowledged-write errors
are retried. Keep the Chrome tab visible during a transfer.

## BLE diagnostics

The browser sends bounded structured BLE lifecycle events back to the Go
process. At `-log debug`, the CLI logs device selection, GATT discovery,
characteristic properties, commands, raw notifications, acknowledgement stages,
block progress, disconnects, and failures through `log/slog`. These events are
hidden at the default `info` level.

For example, an acknowledgement failure identifies the exact protocol stage:

```text
level=ERROR msg="browser BLE" component=web_bluetooth event=ack.timeout stage="initial image header" detail="type=command"
```

No image content or packed badge data is logged.

## Badge types and NFC transfer (ACR1552)

The header's badge-type selector picks which physical badge the prepared
image targets, and its transfer-method selector picks Bluetooth or a local
ACS ACR1552 PC/SC reader; the "Write" button then follows whichever
combination is selected:

- **3.97in 4-colour hybrid (460×768)** — supports both Bluetooth and NFC.
  The NFC path uses the badge's raw NFC-A FIFO protocol (recovered from the
  vendor Android app).
- **nfcink 4-colour (240×416)** — NFC-only; the Go process reads the
  badge's own reported configuration, LZO-compresses the image, uploads it,
  and triggers the e-ink refresh (recovered from a Python reference
  driver).

For NFC, the Go process itself waits for a badge on the reader and writes
the image, streaming progress back to the browser as it goes; there is no
Web Bluetooth equivalent step for that transport.

Requirements:

- macOS with cgo enabled (the default for `go run .` / `go build .` when
  Xcode's command line tools are installed).
- The ACS ACR1552 PC/SC driver installed, with the reader attached.
- Reader firmware 1.05.00 or later, for the hybrid badge's raw ISO 14443-3
  pass-through.

On any other platform, or a build with `CGO_ENABLED=0` (including the
Docker image below), the NFC endpoints report a clear "PC/SC requires macOS
with cgo enabled" error instead of failing to build; Bluetooth transfer is
unaffected.

## Dependencies and licensing

The Go application uses only the standard library. On macOS, the NFC
transfer additionally links against the system PC/SC framework through cgo
(no third-party Go module) — this is skipped entirely on other platforms or
when `CGO_ENABLED=0`. The nfcink badge's LZO1X-1 compressor
(`pkg/nfcink/lzo.go`) is an independent implementation written from the
publicly documented format, not derived from or linking any third-party
compression library.

The frontend vendors Bulma under its MIT license; the license text is stored in
`pkg/server/web/BULMA-LICENSE`.
