"use strict";

import {
  BLOCK_COUNT,
  BLOCK_SIZE,
  HEIGHT,
  RAW_SIZE,
  UART_NOTIFY,
  UART_SERVICE,
  UART_WRITE,
  WIDTH,
  blockCommand,
  encodeLiteralBlock,
  startCommand,
} from "./badge-protocol.js";

const BLE_CHUNK_SIZE = 120;
const BLE_BURST_CHUNKS = 16;
const BLE_WRITE_ATTEMPTS = 20;
const BLE_WRITE_RETRY_DELAY_MS = 5;
const ACK_TIMEOUT_MS = 18000;
const logSession = typeof crypto.randomUUID === "function"
  ? crypto.randomUUID()
  : `session-${Date.now()}-${Math.floor(Math.random() * 1_000_000)}`;
let logSequence = 0;

function logText(value, maximum) {
  return String(value ?? "")
    .replace(/[\u0000-\u001f\u007f]/g, " ")
    .slice(0, maximum);
}

function clientLog(level, event, fields = {}) {
  const payload = {
    level,
    event,
    session: logSession,
    sequence: logSequence++,
    stage: logText(fields.stage, 96),
    device: logText(fields.device, 160),
    detail: logText(fields.detail, 768),
    block: Number.isInteger(fields.block) ? fields.block : 0,
    bytes: Number.isInteger(fields.bytes) ? fields.bytes : 0,
  };
  void fetch("/api/log", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-WebBadge-Request": "1",
    },
    body: JSON.stringify(payload),
    keepalive: true,
  }).catch(() => {
    // Logging must never interfere with a BLE transfer.
  });
}

function bytesToHex(value) {
  return Array.from(value, (byte) => byte.toString(16).padStart(2, "0")).join(" ");
}

const elements = {
  uploadTab: document.querySelector("#upload-tab"),
  designTab: document.querySelector("#design-tab"),
  uploadPanel: document.querySelector("#upload-panel"),
  designPanel: document.querySelector("#design-panel"),
  artwork: document.querySelector("#artwork"),
  artworkFit: document.querySelector("#artwork-fit"),
  dropZone: document.querySelector("#drop-zone"),
  uploadSource: document.querySelector("#upload-source"),
  uploadImage: document.querySelector("#upload-image"),
  prepareUpload: document.querySelector("#prepare-upload"),
  prepareDesign: document.querySelector("#prepare-design"),
  personName: document.querySelector("#person-name"),
  personTitle: document.querySelector("#person-title"),
  company: document.querySelector("#company"),
  portrait: document.querySelector("#portrait"),
  logo: document.querySelector("#logo"),
  accent: document.querySelector("#accent"),
  accentPreset: document.querySelector("#accent-preset"),
  canvas: document.querySelector("#design-canvas"),
  finalPreview: document.querySelector("#final-preview"),
  previewTitle: document.querySelector("#preview-title"),
  previewState: document.querySelector("#preview-state"),
  connect: document.querySelector("#connect"),
  connectionDot: document.querySelector("#connection-dot"),
  deviceName: document.querySelector("#device-name"),
  writeBadge: document.querySelector("#write-badge"),
  status: document.querySelector("#status"),
  progress: document.querySelector("#progress"),
  progressLabel: document.querySelector("#progress-label"),
};

const context = elements.canvas.getContext("2d");
let activeTab = "design";
let artworkFile = null;
let artworkURL = "";
let portraitImage = null;
let logoImage = null;
let packedImage = null;
let previewURL = "";
let bluetoothDevice = null;
let transferring = false;
let expectedDisconnect = false;

class AckQueue {
  constructor() {
    this.buffered = [];
    this.waiters = [];
  }

  clear(reason = "reset") {
    if (this.waiters.length > 0 || this.buffered.length > 0) {
      clientLog("info", "ack.queue_cleared", {
        detail: `reason=${reason}; waiters=${this.waiters.length}; buffered=${this.buffered.join(",") || "none"}`,
      });
    }
    this.buffered = [];
    for (const waiter of this.waiters.splice(0)) {
      window.clearTimeout(waiter.timer);
      waiter.reject(new Error("Acknowledgement wait was cancelled."));
    }
  }

  push(type) {
    const index = this.waiters.findIndex((waiter) => waiter.type === type);
    if (index >= 0) {
      const [waiter] = this.waiters.splice(index, 1);
      window.clearTimeout(waiter.timer);
      clientLog("info", "ack.matched", {stage: waiter.stage, detail: `type=${type}`});
      waiter.resolve();
      return;
    }
    this.buffered.push(type);
    clientLog("info", "ack.buffered", {detail: `type=${type}`});
  }

  wait(type, stage) {
    clientLog("info", "ack.waiting", {stage, detail: `type=${type}`});
    const bufferedIndex = this.buffered.indexOf(type);
    if (bufferedIndex >= 0) {
      this.buffered.splice(bufferedIndex, 1);
      clientLog("info", "ack.used_buffered", {stage, detail: `type=${type}`});
      return Promise.resolve();
    }
    return new Promise((resolve, reject) => {
      const waiter = {type, stage, resolve, reject, timer: 0};
      waiter.timer = window.setTimeout(() => {
        const index = this.waiters.indexOf(waiter);
        if (index >= 0) this.waiters.splice(index, 1);
        clientLog("error", "ack.timeout", {stage, detail: `type=${type}`});
        reject(new Error(`Timed out waiting for the ${type} acknowledgement during ${stage}.`));
      }, ACK_TIMEOUT_MS);
      this.waiters.push(waiter);
    });
  }
}

const acknowledgements = new AckQueue();

function setTab(name) {
  activeTab = name;
  const upload = name === "upload";
  elements.uploadTab.parentElement.classList.toggle("is-active", upload);
  elements.designTab.parentElement.classList.toggle("is-active", !upload);
  elements.uploadTab.setAttribute("aria-selected", String(upload));
  elements.designTab.setAttribute("aria-selected", String(!upload));
  elements.uploadPanel.hidden = !upload;
  elements.designPanel.hidden = upload;
  invalidatePrepared();
  if (upload && elements.uploadImage.complete && elements.uploadImage.src) {
    drawImageFit(elements.uploadImage, elements.artworkFit.value);
  } else if (!upload) {
    drawDesign();
  }
}

function setProgress(value, message) {
  elements.progress.value = value;
  elements.progressLabel.textContent = `${Math.round(value)}%`;
  elements.status.textContent = message;
}

function updateActions() {
  const connected = Boolean(bluetoothDevice?.gatt?.connected);
  elements.writeBadge.disabled = !packedImage || !bluetoothDevice || transferring;
  elements.connect.disabled = transferring;
  elements.connectionDot.classList.toggle("connected", connected);
}

function invalidatePrepared() {
  packedImage = null;
  if (previewURL) URL.revokeObjectURL(previewURL);
  previewURL = "";
  elements.finalPreview.hidden = true;
  elements.canvas.hidden = false;
  elements.previewTitle.textContent = activeTab === "design" ? "Layout preview" : "Artwork preview";
  elements.previewState.textContent = "Draft";
  elements.previewState.className = "tag is-dark";
  setProgress(0, "Prepare an image to continue.");
  updateActions();
}

function loadImage(file) {
  return new Promise((resolve, reject) => {
    if (!file || !["image/png", "image/jpeg"].includes(file.type) || file.size > 10 * 1024 * 1024) {
      reject(new Error("Choose a PNG or JPEG no larger than 10 MB."));
      return;
    }
    const url = URL.createObjectURL(file);
    const image = new Image();
    image.onload = () => resolve({image, url});
    image.onerror = () => {
      URL.revokeObjectURL(url);
      reject(new Error("The selected image could not be decoded."));
    };
    image.src = url;
  });
}

async function selectArtwork(file) {
  try {
    const loaded = await loadImage(file);
    if (artworkURL) URL.revokeObjectURL(artworkURL);
    artworkFile = file;
    artworkURL = loaded.url;
    elements.uploadImage.src = loaded.url;
    elements.uploadSource.classList.remove("empty");
    elements.prepareUpload.disabled = false;
    drawImageFit(loaded.image, elements.artworkFit.value);
    invalidatePrepared();
  } catch (error) {
    artworkFile = null;
    elements.prepareUpload.disabled = true;
    setProgress(0, error.message);
  }
}

function drawImageFit(image, fit) {
  context.fillStyle = "#fff";
  context.fillRect(0, 0, WIDTH, HEIGHT);
  if (!image) return;
  const scale = fit === "cover"
    ? Math.max(WIDTH / image.width, HEIGHT / image.height)
    : Math.min(WIDTH / image.width, HEIGHT / image.height);
  const width = image.width * scale;
  const height = image.height * scale;
  context.drawImage(image, (WIDTH - width) / 2, (HEIGHT - height) / 2, width, height);
}

function roundedRect(ctx, x, y, width, height, radius) {
  ctx.beginPath();
  ctx.roundRect(x, y, width, height, radius);
  ctx.closePath();
}

function fitText(text, maximumWidth, initialSize, weight = 700, minimumSize = 14) {
  let size = initialSize;
  while (size > minimumSize) {
    context.font = `${weight} ${size}px Inter, Arial, sans-serif`;
    if (context.measureText(text).width <= maximumWidth) break;
    size -= 2;
  }
  return size;
}

function ellipsize(text, maximumWidth) {
  if (context.measureText(text).width <= maximumWidth) return text;
  let shortened = text;
  while (shortened.length > 1 && context.measureText(`${shortened}…`).width > maximumWidth) {
    shortened = shortened.slice(0, -1);
  }
  return `${shortened}…`;
}

function drawCoverImage(image, x, y, width, height) {
  const scale = Math.max(width / image.width, height / image.height);
  const sourceWidth = width / scale;
  const sourceHeight = height / scale;
  const sourceX = (image.width - sourceWidth) / 2;
  const sourceY = (image.height - sourceHeight) / 2;
  context.drawImage(image, sourceX, sourceY, sourceWidth, sourceHeight, x, y, width, height);
}

function contrastingTextColor(color) {
  const red = Number.parseInt(color.slice(1, 3), 16);
  const green = Number.parseInt(color.slice(3, 5), 16);
  const blue = Number.parseInt(color.slice(5, 7), 16);
  const luminance = (red * 299 + green * 587 + blue * 114) / 255000;
  return luminance > 0.58 ? "#111" : "#fff";
}

function drawDesign() {
  const accent = elements.accent.value;
  context.fillStyle = "#fff";
  context.fillRect(0, 0, WIDTH, HEIGHT);

  context.fillStyle = accent;
  context.fillRect(0, 0, WIDTH, 116);
  context.fillStyle = contrastingTextColor(accent);
  context.font = "800 22px Inter, Arial, sans-serif";
  context.textBaseline = "middle";
  context.fillText(elements.company.value.trim() || "COMPANY", 30, 58);

  if (logoImage) {
    context.fillStyle = "#fff";
    roundedRect(context, 318, 22, 112, 72, 8);
    context.fill();
    const scale = Math.min(92 / logoImage.width, 52 / logoImage.height);
    const logoWidth = logoImage.width * scale;
    const logoHeight = logoImage.height * scale;
    context.drawImage(logoImage, 374 - logoWidth / 2, 58 - logoHeight / 2, logoWidth, logoHeight);
  }

  context.save();
  roundedRect(context, 30, 148, 400, 370, 18);
  context.clip();
  if (portraitImage) {
    drawCoverImage(portraitImage, 30, 148, 400, 370);
  } else {
    context.fillStyle = "#efeee9";
    context.fillRect(30, 148, 400, 370);
    context.fillStyle = "#c9c7bf";
    context.beginPath();
    context.arc(230, 277, 75, 0, Math.PI * 2);
    context.fill();
    context.beginPath();
    context.ellipse(230, 477, 142, 115, 0, Math.PI, Math.PI * 2);
    context.fill();
  }
  context.restore();

  context.fillStyle = accent;
  context.fillRect(30, 544, 86, 8);
  context.fillStyle = "#111";
  const name = elements.personName.value.trim() || "Your Name";
  const nameSize = fitText(name, 400, 54, 800);
  context.font = `800 ${nameSize}px Inter, Arial, sans-serif`;
  context.textBaseline = "alphabetic";
  context.fillText(ellipsize(name, 400), 30, 620);

  context.fillStyle = "#333";
  const title = elements.personTitle.value.trim() || "Title";
  const titleSize = fitText(title, 400, 25, 600);
  context.font = `600 ${titleSize}px Inter, Arial, sans-serif`;
  context.fillText(ellipsize(title, 400), 30, 668);
  context.fillStyle = "#777";
  const company = elements.company.value.trim() || "Company";
  const companySize = fitText(company, 400, 21, 500);
  context.font = `500 ${companySize}px Inter, Arial, sans-serif`;
  context.fillText(ellipsize(company, 400), 30, 708);

  context.fillStyle = accent;
  context.fillRect(0, 748, WIDTH, 20);
}

function canvasBlob() {
  return new Promise((resolve, reject) => {
    elements.canvas.toBlob((blob) => {
      if (blob) resolve(blob);
      else reject(new Error("Could not create the badge artwork."));
    }, "image/png");
  });
}

function decodeBase64(value) {
  const binary = window.atob(value);
  const output = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index++) output[index] = binary.charCodeAt(index);
  return output;
}

async function prepare(file, fit) {
  setProgress(5, "Preparing image on the server…");
  elements.prepareUpload.disabled = true;
  elements.prepareDesign.disabled = true;
  try {
    const form = new FormData();
    form.append("fit", fit);
    form.append("image", file, file.name || "badge.png");
    const response = await fetch("/api/prepare", {
      method: "POST",
      headers: {"X-WebBadge-Request": "1"},
      body: form,
    });
    const payload = await response.json();
    if (!response.ok) throw new Error(payload.error || "Image preparation failed.");

    const packed = decodeBase64(payload.packed);
    if (packed.byteLength !== RAW_SIZE) throw new Error("The server returned an invalid badge image.");
    packedImage = packed;
    clientLog("info", "image.prepared", {bytes: packed.byteLength});

    const previewBytes = decodeBase64(payload.previewPng);
    previewURL = URL.createObjectURL(new Blob([previewBytes], {type: "image/png"}));
    elements.finalPreview.src = previewURL;
    elements.finalPreview.hidden = false;
    elements.canvas.hidden = true;
    elements.previewTitle.textContent = "E-ink preview";
    elements.previewState.textContent = "Confirmed";
    elements.previewState.className = "tag is-warning";
    setProgress(0, bluetoothDevice?.gatt?.connected
      ? "Ready to write to the selected badge."
      : "Preview confirmed. Choose a badge to continue.");
  } catch (error) {
    packedImage = null;
    setProgress(0, error.message);
  } finally {
    elements.prepareUpload.disabled = !artworkFile;
    elements.prepareDesign.disabled = false;
    updateActions();
  }
}

function browserSupportsBluetooth() {
  return "bluetooth" in navigator;
}

async function connectBadge() {
  if (!browserSupportsBluetooth()) {
    clientLog("error", "bluetooth.unavailable");
    setProgress(0, "Web Bluetooth is unavailable. Use Chrome on macOS over localhost or HTTPS.");
    return;
  }
  try {
    clientLog("info", "device.chooser_opened");
    const device = await navigator.bluetooth.requestDevice({
      filters: [{services: [UART_SERVICE]}],
    });
    if (bluetoothDevice) bluetoothDevice.removeEventListener("gattserverdisconnected", onDisconnected);
    bluetoothDevice = device;
    bluetoothDevice.addEventListener("gattserverdisconnected", onDisconnected);
    clientLog("info", "device.selected", {device: device.name || "unnamed"});
    setProgress(0, `Selected ${device.name || "badge"}. It will connect when writing.`);
    elements.deviceName.textContent = device.name || "Selected badge";
    elements.connect.textContent = "Change badge";
    // Connect now so the primary action accurately reflects readiness.
    clientLog("info", "gatt.connecting", {device: device.name || "unnamed"});
    await device.gatt.connect();
    clientLog("info", "gatt.connected", {device: device.name || "unnamed"});
    setProgress(0, packedImage ? "Ready to write to the selected badge." : "Badge connected. Prepare an image.");
  } catch (error) {
    const level = error.name === "NotFoundError" ? "info" : "error";
    clientLog(level, "gatt.connection_failed", {
      detail: `${error.name || "Error"}: ${error.message || String(error)}`,
    });
    if (error.name !== "NotFoundError") setProgress(0, `Bluetooth connection failed: ${error.message}`);
  } finally {
    updateActions();
  }
}

function onDisconnected() {
  acknowledgements.clear("gatt disconnected");
  clientLog(expectedDisconnect ? "info" : "warn", "gatt.disconnected", {
    device: bluetoothDevice?.name || "unnamed",
    detail: `expected=${expectedDisconnect}`,
  });
  if (expectedDisconnect) {
    expectedDisconnect = false;
    updateActions();
    return;
  }
  elements.deviceName.textContent = bluetoothDevice?.name
    ? `${bluetoothDevice.name} disconnected`
    : "Badge disconnected";
  setProgress(0, "Badge disconnected. Choose it again or retry the transfer.");
  updateActions();
}

function parseAcknowledgement(event) {
  const value = event.target.value;
  const bytes = new Uint8Array(value.buffer, value.byteOffset, value.byteLength);
  clientLog("info", "notification.received", {
    detail: `hex=${bytesToHex(bytes)}`,
    bytes: bytes.byteLength,
  });
  if (value.byteLength !== 3 || value.getUint8(0) !== 0x23 || value.getUint8(1) !== 0x23) {
    clientLog("warn", "notification.unrecognized", {detail: `hex=${bytesToHex(bytes)}`});
    return;
  }
  if (value.getUint8(2) === 0x03) acknowledgements.push("command");
  if (value.getUint8(2) === 0x12) acknowledgements.push("data");
}

function characteristicProperties(characteristic) {
  const names = [
    "authenticatedSignedWrites",
    "broadcast",
    "indicate",
    "notify",
    "read",
    "reliableWrite",
    "writableAuxiliaries",
    "write",
    "writeWithoutResponse",
  ];
  return names.filter((name) => characteristic.properties[name]).join(",") || "none";
}

function commandWriteMode(characteristic) {
  if (characteristic.properties.writeWithoutResponse &&
      typeof characteristic.writeValueWithoutResponse === "function") {
    return "without_response";
  }
  if (characteristic.properties.write &&
      typeof characteristic.writeValueWithResponse === "function") {
    return "with_response";
  }
  return "unsupported";
}

function dataWriteMode(characteristic) {
  if (characteristic.properties.write &&
      characteristic.properties.writeWithoutResponse &&
      typeof characteristic.writeValueWithResponse === "function" &&
      typeof characteristic.writeValueWithoutResponse === "function") {
    return `hybrid_burst_${BLE_BURST_CHUNKS}`;
  }
  if (characteristic.properties.writeWithoutResponse &&
      typeof characteristic.writeValueWithoutResponse === "function") {
    return "without_response";
  }
  if (characteristic.properties.write &&
      typeof characteristic.writeValueWithResponse === "function") {
    return "with_response_fallback";
  }
  return "unsupported";
}

async function writeCommandValue(characteristic, value) {
  if (characteristic.properties.writeWithoutResponse &&
      typeof characteristic.writeValueWithoutResponse === "function") {
    await characteristic.writeValueWithoutResponse(value);
    return;
  }
  if (characteristic.properties.write &&
      typeof characteristic.writeValueWithResponse === "function") {
    await characteristic.writeValueWithResponse(value);
    return;
  }
  throw new Error("The badge write characteristic is not writable.");
}

async function writeDataValue(characteristic, value, requireResponse) {
  if (requireResponse && characteristic.properties.write &&
      typeof characteristic.writeValueWithResponse === "function") {
    await characteristic.writeValueWithResponse(value);
    return;
  }
  if (characteristic.properties.writeWithoutResponse &&
      typeof characteristic.writeValueWithoutResponse === "function") {
    let lastError = null;
    for (let attempt = 1; attempt <= BLE_WRITE_ATTEMPTS; attempt++) {
      try {
        await characteristic.writeValueWithoutResponse(value);
        return;
      } catch (error) {
        lastError = error;
        if (!bluetoothDevice?.gatt?.connected) throw error;
        if (attempt < BLE_WRITE_ATTEMPTS) {
          clientLog("warn", "data.write_retry", {
            detail: `attempt=${attempt}; error=${error.name || "Error"}: ${error.message || String(error)}`,
            bytes: value.byteLength,
          });
          await delay(BLE_WRITE_RETRY_DELAY_MS);
        }
      }
    }
    throw lastError || new Error("BLE data write failed.");
  }
  if (characteristic.properties.write &&
      typeof characteristic.writeValueWithResponse === "function") {
    await characteristic.writeValueWithResponse(value);
    return;
  }
  throw new Error("The badge write characteristic is not writable.");
}

const delay = (milliseconds) => new Promise((resolve) => window.setTimeout(resolve, milliseconds));

async function writeBadge() {
  if (!packedImage || !bluetoothDevice || transferring) return;
  transferring = true;
  updateActions();
  acknowledgements.clear("transfer starting");
  let notifier = null;
  let stage = "initializing";
  let currentBlock = 0;
  const transferStartedAt = performance.now();
  try {
    clientLog("info", "transfer.started", {
      device: bluetoothDevice.name || "unnamed",
      stage,
      bytes: packedImage.byteLength,
    });
    setProgress(2, "Connecting to badge…");
    stage = "GATT connection";
    const reusedConnection = bluetoothDevice.gatt.connected;
    const gatt = reusedConnection
      ? bluetoothDevice.gatt
      : await bluetoothDevice.gatt.connect();
    clientLog("info", "gatt.ready", {
      device: bluetoothDevice.name || "unnamed",
      stage,
      detail: `reused=${reusedConnection}`,
    });
    stage = "service discovery";
    const service = await gatt.getPrimaryService(UART_SERVICE);
    clientLog("info", "gatt.service_found", {stage, detail: `uuid=${service.uuid}`});
    const writer = await service.getCharacteristic(UART_WRITE);
    notifier = await service.getCharacteristic(UART_NOTIFY);
    clientLog("info", "gatt.characteristics_found", {
      stage,
      detail: `writer=${characteristicProperties(writer)}; notifier=${characteristicProperties(notifier)}; command_mode=${commandWriteMode(writer)}; data_mode=${dataWriteMode(writer)}`,
    });
    notifier.addEventListener("characteristicvaluechanged", parseAcknowledgement);
    stage = "notification subscription";
    await notifier.startNotifications();
    clientLog("info", "notifications.started", {stage});

    setProgress(5, "Starting image transfer…");
    const header = startCommand();
    stage = "initial image header";
    clientLog("info", "command.writing", {stage, detail: `hex=${bytesToHex(header)}`, bytes: header.byteLength});
    await writeCommandValue(writer, header);
    clientLog("info", "command.written", {stage});
    await acknowledgements.wait("command", stage);

    let lastCompressedLength = 0;
    for (let block = 0; block < BLOCK_COUNT; block++) {
      currentBlock = block + 1;
      const offset = block * BLOCK_SIZE;
      const raw = packedImage.subarray(offset, Math.min(offset + BLOCK_SIZE, packedImage.length));
      const compressed = encodeLiteralBlock(raw);
      lastCompressedLength = compressed.length;

      const command = blockCommand(compressed.length, currentBlock);
      stage = `block ${currentBlock} ready`;
      clientLog("info", "block.command_writing", {
        stage,
        block: currentBlock,
        bytes: compressed.byteLength,
        detail: `hex=${bytesToHex(command)}; raw_bytes=${raw.byteLength}`,
      });
      await writeCommandValue(writer, command);
      clientLog("info", "block.command_written", {stage, block: currentBlock});
      await acknowledgements.wait("data", stage);

      stage = `block ${currentBlock} data`;
      clientLog("info", "block.data_writing", {
        stage,
        block: currentBlock,
        bytes: compressed.byteLength,
        detail: `chunk_size=${BLE_CHUNK_SIZE}; burst_chunks=${BLE_BURST_CHUNKS}; attempts=${BLE_WRITE_ATTEMPTS}`,
      });
      const blockStartedAt = performance.now();
      let chunkIndex = 0;
      for (let sent = 0; sent < compressed.length; sent += BLE_CHUNK_SIZE) {
        const end = Math.min(sent + BLE_CHUNK_SIZE, compressed.length);
        const requireResponse =
          end === compressed.length || (chunkIndex + 1) % BLE_BURST_CHUNKS === 0;
        await writeDataValue(writer, compressed.subarray(sent, end), requireResponse);
        chunkIndex++;
      }
      clientLog("info", "block.data_written", {
        stage,
        block: currentBlock,
        bytes: compressed.byteLength,
        detail: `duration_ms=${Math.round(performance.now() - blockStartedAt)}`,
      });
      stage = `block ${currentBlock} completion`;
      await acknowledgements.wait("command", stage);
      setProgress(10 + ((block + 1) * 85 / BLOCK_COUNT),
        `Transferred block ${block + 1} of ${BLOCK_COUNT}…`);
    }

    stage = "final badge refresh";
    await delay(500 + lastCompressedLength / 2);
    clientLog("info", "transfer.completed", {
      stage,
      block: currentBlock,
      bytes: packedImage.byteLength,
      detail: `duration_ms=${Math.round(performance.now() - transferStartedAt)}`,
    });
    setProgress(100, "Badge update completed.");
  } catch (error) {
    clientLog("error", "transfer.failed", {
      stage,
      block: currentBlock,
      detail: `elapsed_ms=${Math.round(performance.now() - transferStartedAt)}; ${error.name || "Error"}: ${error.message || String(error)}`,
    });
    setProgress(0, `Transfer failed: ${error.message}`);
  } finally {
    transferring = false;
    acknowledgements.clear("transfer cleanup");
    if (notifier) {
      notifier.removeEventListener("characteristicvaluechanged", parseAcknowledgement);
    }
    if (bluetoothDevice?.gatt?.connected) {
      expectedDisconnect = true;
      bluetoothDevice.gatt.disconnect();
    }
    updateActions();
    clientLog("info", "transfer.cleanup_completed", {stage});
  }
}

async function selectOptionalImage(input, setter) {
  const file = input.files[0];
  if (!file) {
    setter(null);
    drawDesign();
    invalidatePrepared();
    return;
  }
  try {
    const loaded = await loadImage(file);
    setter(loaded.image);
    drawDesign();
    URL.revokeObjectURL(loaded.url);
    invalidatePrepared();
  } catch (error) {
    setProgress(0, error.message);
  }
}

elements.uploadTab.addEventListener("click", () => setTab("upload"));
elements.designTab.addEventListener("click", () => setTab("design"));
elements.artwork.addEventListener("change", () => selectArtwork(elements.artwork.files[0]));
elements.artworkFit.addEventListener("change", () => {
  if (elements.uploadImage.complete && elements.uploadImage.src) {
    drawImageFit(elements.uploadImage, elements.artworkFit.value);
    invalidatePrepared();
  }
});
elements.dropZone.addEventListener("dragover", (event) => {
  event.preventDefault();
  elements.dropZone.classList.add("dragging");
});
elements.dropZone.addEventListener("dragleave", () => elements.dropZone.classList.remove("dragging"));
elements.dropZone.addEventListener("drop", (event) => {
  event.preventDefault();
  elements.dropZone.classList.remove("dragging");
  selectArtwork(event.dataTransfer.files[0]);
});
elements.prepareUpload.addEventListener("click", () => prepare(artworkFile, elements.artworkFit.value));
elements.prepareDesign.addEventListener("click", async () => {
  try {
    drawDesign();
    const blob = await canvasBlob();
    prepare(new File([blob], "badge-layout.png", {type: "image/png"}), "contain");
  } catch (error) {
    setProgress(0, error.message);
  }
});
for (const input of [elements.personName, elements.personTitle, elements.company]) {
  input.addEventListener("input", () => {
    drawDesign();
    invalidatePrepared();
  });
}
elements.accentPreset.addEventListener("change", () => {
  if (elements.accentPreset.value !== "custom") {
    elements.accent.value = elements.accentPreset.value;
    document.documentElement.style.setProperty("--accent", elements.accent.value);
  }
  drawDesign();
  invalidatePrepared();
});
elements.accent.addEventListener("input", () => {
  elements.accentPreset.value = "custom";
  document.documentElement.style.setProperty("--accent", elements.accent.value);
  drawDesign();
  invalidatePrepared();
});
elements.portrait.addEventListener("change", () => selectOptionalImage(elements.portrait, (image) => { portraitImage = image; }));
elements.logo.addEventListener("change", () => selectOptionalImage(elements.logo, (image) => { logoImage = image; }));
elements.connect.addEventListener("click", connectBadge);
elements.writeBadge.addEventListener("click", writeBadge);

drawDesign();
clientLog("info", "page.ready", {
  detail: `secure_context=${window.isSecureContext}; web_bluetooth=${browserSupportsBluetooth()}; user_agent=${navigator.userAgent}`,
});
if (!browserSupportsBluetooth()) {
  elements.connect.disabled = true;
  elements.deviceName.textContent = "Web Bluetooth unavailable";
}
