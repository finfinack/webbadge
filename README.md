# webbadge

Web Badge / Badge Studio is a standalone Go web application for preparing and
transferring images to 460 × 768, four-color BOE e-ink badges.

<img width="600" alt="image" src="https://github.com/user-attachments/assets/b13720d1-c314-4fe2-b78e-a717ea502022" />

The Go server:

- embeds and serves the frontend
- accepts bounded PNG and JPEG uploads
- validates image dimensions before full decoding
- resizes with `contain` or `cover` behavior
- applies four-color Floyd–Steinberg dithering
- returns an exact badge preview and the packed 88,320-byte image

The browser performs the BLE transfer through Web Bluetooth. Images and badge
data are not persisted by the server.

## Requirements

- Chrome or Edge on macOS, Windows, ChromeOS, or supported Android devices
- a BLE badge exposing the Nordic-UART-style service described below

Web Bluetooth works from `http://localhost` during development.

## Run

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

## Test

```sh
go test -race ./...
go vet ./...
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

## Dependencies and licensing

The Go application uses only the standard library.

The frontend vendors Bulma under its MIT license; the license text is stored in
`pkg/server/web/BULMA-LICENSE`.
