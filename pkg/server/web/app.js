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
const WEBCAM_MAX_DIMENSION = 2000;
const WEBCAM_OUTPUT_SIZE = 1000;
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
  portraitSize: document.querySelector("#portrait-size"),
  openWebcam: document.querySelector("#open-webcam"),
  webcamDialog: document.querySelector("#webcam-dialog"),
  webcamVideo: document.querySelector("#webcam-video"),
  webcamStatus: document.querySelector("#webcam-status"),
  webcamCropCanvas: document.querySelector("#webcam-crop-canvas"),
  webcamCropControls: document.querySelector("#webcam-crop-controls"),
  webcamCropSize: document.querySelector("#webcam-crop-size"),
  captureWebcam: document.querySelector("#capture-webcam"),
  retakeWebcam: document.querySelector("#retake-webcam"),
  useWebcamCrop: document.querySelector("#use-webcam-crop"),
  closeWebcam: document.querySelector("#close-webcam"),
  logo: document.querySelector("#logo"),
  monogramLetter: document.querySelector("#monogram-letter"),
  monogramColor: document.querySelector("#monogram-color"),
  accent: document.querySelector("#accent"),
  accentPreset: document.querySelector("#accent-preset"),
  canvas: document.querySelector("#design-canvas"),
  finalPreview: document.querySelector("#final-preview"),
  previewTitle: document.querySelector("#preview-title"),
  previewState: document.querySelector("#preview-state"),
  connect: document.querySelector("#connect"),
  connectionDot: document.querySelector("#connection-dot"),
  deviceName: document.querySelector("#device-name"),
  transportSelect: document.querySelector("#transport-select"),
  writeBadge: document.querySelector("#write-badge"),
  status: document.querySelector("#status"),
  progress: document.querySelector("#progress"),
  progressLabel: document.querySelector("#progress-label"),
};

const context = elements.canvas.getContext("2d");
const webcamSource = document.createElement("canvas");
const webcamSourceContext = webcamSource.getContext("2d");
const webcamCropContext = elements.webcamCropCanvas.getContext("2d");
let activeTab = "design";
let artworkFile = null;
let artworkURL = "";
let portraitImage = null;
let logoImage = null;
let packedImage = null;
let previewPngBytes = null;
let previewURL = "";
let bluetoothDevice = null;
let transferring = false;
let expectedDisconnect = false;
let webcamStream = null;
let webcamCrop = null;
let webcamDrag = null;
let webcamRequest = 0;
let transportMode = "ble";
let nfcStatusChecking = false;
let nfcReaderAvailable = false;
let nfcReaderName = "";
let nfcBadgePresent = false;

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
  if (transportMode === "ble") {
    const connected = Boolean(bluetoothDevice?.gatt?.connected);
    elements.connectionDot.classList.toggle("connected", connected);
    elements.deviceName.textContent = bluetoothDevice
      ? (connected ? (bluetoothDevice.name || "Selected badge") : `${bluetoothDevice.name || "Badge"} disconnected`)
      : "No badge connected";
    elements.connect.textContent = bluetoothDevice ? "Change badge" : "Choose badge";
    elements.connect.disabled = transferring;
    elements.writeBadge.disabled = !packedImage || !bluetoothDevice || transferring;
  } else {
    elements.connectionDot.classList.toggle("connected", nfcReaderAvailable);
    if (nfcStatusChecking) {
      elements.deviceName.textContent = "Checking for reader…";
    } else if (!nfcReaderAvailable) {
      elements.deviceName.textContent = "No NFC reader found";
    } else {
      elements.deviceName.textContent = `${nfcReaderName || "NFC reader"} · ${
        nfcBadgePresent ? "badge detected" : "no badge on reader"
      }`;
    }
    elements.connect.textContent = "Check for reader";
    elements.connect.disabled = transferring || nfcStatusChecking;
    elements.writeBadge.disabled = !previewPngBytes || !nfcReaderAvailable || transferring;
  }
}

async function checkNFCReader() {
  if (nfcStatusChecking) return;
  nfcStatusChecking = true;
  updateActions();
  try {
    const response = await fetch("/api/nfc/status", {
      method: "GET",
      headers: {"X-WebBadge-Request": "1"},
    });
    const payload = await response.json();
    nfcReaderAvailable = Boolean(payload.available);
    nfcReaderName = payload.reader || "";
    nfcBadgePresent = Boolean(payload.badgePresent);
    if (!nfcReaderAvailable) {
      clientLog("info", "nfc.status.unavailable", {detail: payload.error || ""});
    }
  } catch (error) {
    nfcReaderAvailable = false;
    nfcReaderName = "";
    nfcBadgePresent = false;
    clientLog("error", "nfc.status.failed", {
      detail: `${error.name || "Error"}: ${error.message || String(error)}`,
    });
  } finally {
    nfcStatusChecking = false;
    updateActions();
  }
}

function invalidatePrepared() {
  packedImage = null;
  previewPngBytes = null;
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

function selectedMonogramLetter() {
  return Array.from(elements.monogramLetter.value.trim())[0] || "";
}

function drawDesign() {
  const accent = elements.accent.value;
  const monogramLetter = selectedMonogramLetter();
  const hasHeaderMark = Boolean(monogramLetter || logoImage);
  context.fillStyle = "#fff";
  context.fillRect(0, 0, WIDTH, HEIGHT);

  context.fillStyle = accent;
  context.fillRect(0, 0, WIDTH, 116);
  context.fillStyle = contrastingTextColor(accent);
  const companyName = elements.company.value.trim() || "COMPANY";
  const companyMaximumWidth = hasHeaderMark ? 268 : 400;
  const companyInitialSize = hasHeaderMark ? 22 : 42;
  const headerCompanySize = fitText(companyName, companyMaximumWidth, companyInitialSize, 800, 22);
  context.font = `800 ${headerCompanySize}px Inter, Arial, sans-serif`;
  context.textBaseline = "middle";
  context.fillText(ellipsize(companyName, companyMaximumWidth), 30, 58);

  if (monogramLetter) {
    context.save();
    context.fillStyle = elements.monogramColor.value;
    roundedRect(context, 358, 22, 72, 72, 8);
    context.fill();
    context.fillStyle = contrastingTextColor(elements.monogramColor.value);
    context.font = "800 44px Inter, Arial, sans-serif";
    context.textAlign = "center";
    context.textBaseline = "middle";
    context.fillText(monogramLetter, 394, 58);
    context.restore();
  } else if (logoImage) {
    context.fillStyle = "#fff";
    roundedRect(context, 318, 22, 112, 72, 8);
    context.fill();
    const scale = Math.min(92 / logoImage.width, 52 / logoImage.height);
    const logoWidth = logoImage.width * scale;
    const logoHeight = logoImage.height * scale;
    context.drawImage(logoImage, 374 - logoWidth / 2, 58 - logoHeight / 2, logoWidth, logoHeight);
  }

  const fullSizePortrait = elements.portraitSize.value === "full";
  const portraitFrame = fullSizePortrait
    ? {x: 0, y: 116, width: WIDTH, height: 428}
    : {x: 30, y: 148, width: 400, height: 370};

  context.save();
  if (!fullSizePortrait) {
    roundedRect(context, portraitFrame.x, portraitFrame.y, portraitFrame.width, portraitFrame.height, 18);
    context.clip();
  }
  if (portraitImage) {
    drawCoverImage(
      portraitImage,
      portraitFrame.x,
      portraitFrame.y,
      portraitFrame.width,
      portraitFrame.height,
    );
  } else {
    context.fillStyle = "#efeee9";
    context.fillRect(
      portraitFrame.x,
      portraitFrame.y,
      portraitFrame.width,
      portraitFrame.height,
    );
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
    previewPngBytes = previewBytes;
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
    previewPngBytes = null;
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

function writeBadge() {
  return transportMode === "nfc" ? writeBadgeOverNFC() : writeBadgeOverBLE();
}

async function writeBadgeOverBLE() {
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

async function writeBadgeOverNFC() {
  if (!previewPngBytes || transferring) return;
  transferring = true;
  updateActions();
  const startedAt = performance.now();
  clientLog("info", "nfc.transfer.started", {bytes: previewPngBytes.byteLength});
  setProgress(2, "Starting NFC write…");
  let finished = false;
  let reader = null;
  try {
    const form = new FormData();
    form.append("image", new Blob([previewPngBytes], {type: "image/png"}), "badge.png");
    const response = await fetch("/api/nfc/write", {
      method: "POST",
      headers: {"X-WebBadge-Request": "1"},
      body: form,
    });
    if (!response.ok) {
      const payload = await response.json().catch(() => ({}));
      throw new Error(payload.error || "NFC write failed.");
    }
    if (!response.body) throw new Error("This browser does not support streamed responses.");

    reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    while (!finished) {
      const {value, done} = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, {stream: true});
      let boundary;
      while (!finished && (boundary = buffer.indexOf("\n\n")) >= 0) {
        const chunk = buffer.slice(0, boundary);
        buffer = buffer.slice(boundary + 2);
        const line = chunk.split("\n").find((candidate) => candidate.startsWith("data: "));
        if (!line) continue;
        const event = JSON.parse(line.slice("data: ".length));
        clientLog("info", "nfc.event", {stage: event.stage, detail: event.message});
        if (event.stage === "error") throw new Error(event.message || "NFC write failed.");
        setProgress(event.percent ?? 0, event.message || "");
        if (event.stage === "done") finished = true;
      }
    }
    if (!finished) throw new Error("Connection closed before the write finished.");
    clientLog("info", "nfc.transfer.completed", {
      detail: `duration_ms=${Math.round(performance.now() - startedAt)}`,
    });
  } catch (error) {
    clientLog("error", "nfc.transfer.failed", {
      detail: `elapsed_ms=${Math.round(performance.now() - startedAt)}; ${error.name || "Error"}: ${error.message || String(error)}`,
    });
    setProgress(0, `NFC write failed: ${error.message}`);
  } finally {
    if (reader && !finished) reader.cancel().catch(() => {});
    transferring = false;
    updateActions();
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

function stopWebcamStream() {
  if (webcamStream) {
    for (const track of webcamStream.getTracks()) track.stop();
  }
  webcamStream = null;
  elements.webcamVideo.srcObject = null;
}

function resetWebcamView() {
  elements.webcamVideo.hidden = false;
  elements.webcamCropCanvas.hidden = true;
  elements.webcamCropControls.hidden = true;
  elements.captureWebcam.hidden = false;
  elements.captureWebcam.disabled = true;
  elements.retakeWebcam.hidden = true;
  elements.useWebcamCrop.hidden = true;
  elements.webcamStatus.textContent = "Requesting camera access…";
  webcamCrop = null;
  webcamDrag = null;
}

function webcamErrorMessage(error) {
  if (error.name === "NotAllowedError") {
    return "Camera access was denied. Allow camera access in the browser and try again.";
  }
  if (error.name === "NotFoundError") {
    return "No webcam was found.";
  }
  if (error.name === "NotReadableError") {
    return "The webcam is already in use by another application.";
  }
  return "The webcam could not be started.";
}

async function startWebcam() {
  if (!navigator.mediaDevices?.getUserMedia) {
    setProgress(0, "Webcam capture is unavailable in this browser or connection.");
    return;
  }

  const request = ++webcamRequest;
  stopWebcamStream();
  resetWebcamView();
  if (!elements.webcamDialog.open) elements.webcamDialog.showModal();
  elements.openWebcam.disabled = true;
  try {
    const stream = await navigator.mediaDevices.getUserMedia({
      audio: false,
      video: {facingMode: {ideal: "user"}},
    });
    if (request !== webcamRequest || !elements.webcamDialog.open) {
      for (const track of stream.getTracks()) track.stop();
      return;
    }

    webcamStream = stream;
    elements.webcamVideo.srcObject = stream;
    await elements.webcamVideo.play();
    elements.captureWebcam.disabled = false;
    elements.webcamStatus.textContent = "Position yourself, then capture the photo.";
  } catch (error) {
    if (request === webcamRequest) {
      stopWebcamStream();
      if (elements.webcamDialog.open) {
        elements.webcamStatus.textContent = webcamErrorMessage(error);
      }
    }
  } finally {
    if (request === webcamRequest) elements.openWebcam.disabled = false;
  }
}

function drawWebcamCrop() {
  if (!webcamCrop) return;

  const canvas = elements.webcamCropCanvas;
  const {x, y, size} = webcamCrop;
  webcamCropContext.clearRect(0, 0, canvas.width, canvas.height);
  webcamCropContext.drawImage(webcamSource, 0, 0);
  webcamCropContext.fillStyle = "rgba(0, 0, 0, .58)";
  webcamCropContext.fillRect(0, 0, canvas.width, y);
  webcamCropContext.fillRect(0, y + size, canvas.width, canvas.height - y - size);
  webcamCropContext.fillRect(0, y, x, size);
  webcamCropContext.fillRect(x + size, y, canvas.width - x - size, size);
  webcamCropContext.strokeStyle = "#fff";
  webcamCropContext.lineWidth = Math.max(2, canvas.width / 400);
  webcamCropContext.strokeRect(x, y, size, size);
}

function captureWebcamPhoto() {
  const sourceWidth = elements.webcamVideo.videoWidth;
  const sourceHeight = elements.webcamVideo.videoHeight;
  if (sourceWidth < 1 || sourceHeight < 1) {
    elements.webcamStatus.textContent = "The webcam is not ready. Try capturing again.";
    return;
  }

  const scale = Math.min(1, WEBCAM_MAX_DIMENSION / Math.max(sourceWidth, sourceHeight));
  webcamSource.width = Math.max(1, Math.round(sourceWidth * scale));
  webcamSource.height = Math.max(1, Math.round(sourceHeight * scale));
  webcamSourceContext.drawImage(
    elements.webcamVideo,
    0,
    0,
    webcamSource.width,
    webcamSource.height,
  );
  stopWebcamStream();

  const minimumDimension = Math.min(webcamSource.width, webcamSource.height);
  const size = minimumDimension * Number(elements.webcamCropSize.value) / 100;
  webcamCrop = {
    x: (webcamSource.width - size) / 2,
    y: (webcamSource.height - size) / 2,
    size,
  };
  elements.webcamCropCanvas.width = webcamSource.width;
  elements.webcamCropCanvas.height = webcamSource.height;
  elements.webcamVideo.hidden = true;
  elements.webcamCropCanvas.hidden = false;
  elements.webcamCropControls.hidden = false;
  elements.captureWebcam.hidden = true;
  elements.retakeWebcam.hidden = false;
  elements.useWebcamCrop.hidden = false;
  elements.webcamStatus.textContent = "Adjust the square crop, then use the selected area.";
  drawWebcamCrop();
}

function webcamCanvasPoint(event) {
  const bounds = elements.webcamCropCanvas.getBoundingClientRect();
  return {
    x: (event.clientX - bounds.left) * elements.webcamCropCanvas.width / bounds.width,
    y: (event.clientY - bounds.top) * elements.webcamCropCanvas.height / bounds.height,
  };
}

function moveWebcamCrop(point, offsetX, offsetY) {
  const maximumX = webcamSource.width - webcamCrop.size;
  const maximumY = webcamSource.height - webcamCrop.size;
  webcamCrop.x = Math.min(Math.max(point.x - offsetX, 0), maximumX);
  webcamCrop.y = Math.min(Math.max(point.y - offsetY, 0), maximumY);
  drawWebcamCrop();
}

function startWebcamCropDrag(event) {
  if (!webcamCrop) return;

  const point = webcamCanvasPoint(event);
  const insideCrop =
    point.x >= webcamCrop.x && point.x <= webcamCrop.x + webcamCrop.size &&
    point.y >= webcamCrop.y && point.y <= webcamCrop.y + webcamCrop.size;
  const offsetX = insideCrop ? point.x - webcamCrop.x : webcamCrop.size / 2;
  const offsetY = insideCrop ? point.y - webcamCrop.y : webcamCrop.size / 2;
  webcamDrag = {pointerId: event.pointerId, offsetX, offsetY};
  elements.webcamCropCanvas.setPointerCapture(event.pointerId);
  moveWebcamCrop(point, offsetX, offsetY);
}

function dragWebcamCrop(event) {
  if (!webcamCrop || !webcamDrag || event.pointerId !== webcamDrag.pointerId) return;
  event.preventDefault();
  moveWebcamCrop(webcamCanvasPoint(event), webcamDrag.offsetX, webcamDrag.offsetY);
}

function stopWebcamCropDrag(event) {
  if (!webcamDrag || event.pointerId !== webcamDrag.pointerId) return;
  if (elements.webcamCropCanvas.hasPointerCapture(event.pointerId)) {
    elements.webcamCropCanvas.releasePointerCapture(event.pointerId);
  }
  webcamDrag = null;
}

function resizeWebcamCrop() {
  if (!webcamCrop) return;

  const centerX = webcamCrop.x + webcamCrop.size / 2;
  const centerY = webcamCrop.y + webcamCrop.size / 2;
  const minimumDimension = Math.min(webcamSource.width, webcamSource.height);
  const size = minimumDimension * Number(elements.webcamCropSize.value) / 100;
  webcamCrop.size = size;
  webcamCrop.x = Math.min(Math.max(centerX - size / 2, 0), webcamSource.width - size);
  webcamCrop.y = Math.min(Math.max(centerY - size / 2, 0), webcamSource.height - size);
  drawWebcamCrop();
}

function useWebcamCrop() {
  if (!webcamCrop) return;

  const output = document.createElement("canvas");
  output.width = WEBCAM_OUTPUT_SIZE;
  output.height = WEBCAM_OUTPUT_SIZE;
  const outputContext = output.getContext("2d");
  outputContext.drawImage(
    webcamSource,
    webcamCrop.x,
    webcamCrop.y,
    webcamCrop.size,
    webcamCrop.size,
    0,
    0,
    output.width,
    output.height,
  );
  portraitImage = output;
  elements.portrait.value = "";
  drawDesign();
  invalidatePrepared();
  elements.webcamDialog.close();
}

function closeWebcam() {
  if (elements.webcamDialog.open) elements.webcamDialog.close();
}

function clearWebcamCapture() {
  webcamRequest++;
  stopWebcamStream();
  elements.openWebcam.disabled = !navigator.mediaDevices?.getUserMedia;
  webcamSource.width = 1;
  webcamSource.height = 1;
  elements.webcamCropCanvas.width = 1;
  elements.webcamCropCanvas.height = 1;
  webcamCrop = null;
  webcamDrag = null;
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
elements.portraitSize.addEventListener("change", () => {
  drawDesign();
  invalidatePrepared();
});
elements.openWebcam.addEventListener("click", startWebcam);
elements.captureWebcam.addEventListener("click", captureWebcamPhoto);
elements.retakeWebcam.addEventListener("click", startWebcam);
elements.useWebcamCrop.addEventListener("click", useWebcamCrop);
elements.closeWebcam.addEventListener("click", closeWebcam);
elements.webcamDialog.addEventListener("close", clearWebcamCapture);
elements.webcamCropSize.addEventListener("input", resizeWebcamCrop);
elements.webcamCropCanvas.addEventListener("pointerdown", startWebcamCropDrag);
elements.webcamCropCanvas.addEventListener("pointermove", dragWebcamCrop);
elements.webcamCropCanvas.addEventListener("pointerup", stopWebcamCropDrag);
elements.webcamCropCanvas.addEventListener("pointercancel", stopWebcamCropDrag);
elements.logo.addEventListener("change", () => selectOptionalImage(elements.logo, (image) => { logoImage = image; }));
elements.monogramLetter.addEventListener("input", () => {
  drawDesign();
  invalidatePrepared();
});
elements.monogramColor.addEventListener("input", () => {
  drawDesign();
  invalidatePrepared();
});
elements.connect.addEventListener("click", () => {
  if (transportMode === "ble") connectBadge();
  else checkNFCReader();
});
elements.transportSelect.addEventListener("change", () => {
  transportMode = elements.transportSelect.value;
  updateActions();
  if (transportMode === "nfc" && !nfcStatusChecking) checkNFCReader();
});
elements.writeBadge.addEventListener("click", writeBadge);
window.addEventListener("pagehide", clearWebcamCapture);

drawDesign();
clientLog("info", "page.ready", {
  detail: `secure_context=${window.isSecureContext}; web_bluetooth=${browserSupportsBluetooth()}; user_agent=${navigator.userAgent}`,
});
if (!browserSupportsBluetooth()) {
  const bleOption = elements.transportSelect.querySelector('option[value="ble"]');
  if (bleOption) bleOption.disabled = true;
  transportMode = "nfc";
  elements.transportSelect.value = "nfc";
  checkNFCReader();
}
updateActions();
if (!navigator.mediaDevices?.getUserMedia) {
  elements.openWebcam.disabled = true;
  elements.openWebcam.title = "Webcam capture requires a secure context and a supported browser.";
}
