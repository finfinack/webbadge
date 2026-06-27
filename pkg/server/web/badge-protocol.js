export const WIDTH = 460;
export const HEIGHT = 768;
export const RAW_SIZE = WIDTH * HEIGHT / 4;
export const BLOCK_SIZE = 7360;
export const BLOCK_COUNT = Math.ceil(RAW_SIZE / BLOCK_SIZE);

export const UART_SERVICE = "6e400001-b5a3-f393-e0a9-e50e24dcca9e";
export const UART_WRITE = "6e400002-b5a3-f393-e0a9-e50e24dcca9e";
export const UART_NOTIFY = "6e400003-b5a3-f393-e0a9-e50e24dcca9e";

export function startCommand() {
  return Uint8Array.of(
    0x23, 0x23, 0x01,
    RAW_SIZE & 0xff,
    (RAW_SIZE >> 8) & 0xff,
    (RAW_SIZE >> 16) & 0xff,
    0x01,
    BLOCK_COUNT,
  );
}

export function blockCommand(length, oneBasedIndex) {
  if (!Number.isSafeInteger(length) || length <= 0 || length > 0xffffff) {
    throw new RangeError("Block length must fit in three bytes.");
  }
  if (!Number.isSafeInteger(oneBasedIndex) || oneBasedIndex <= 0 || oneBasedIndex > 0xff) {
    throw new RangeError("Block index must fit in one byte.");
  }
  return Uint8Array.of(
    0x23, 0x23, 0x11,
    length & 0xff,
    (length >> 8) & 0xff,
    (length >> 16) & 0xff,
    0x01,
    oneBasedIndex,
  );
}

export function encodeLiteralBlock(input) {
  if (!(input instanceof Uint8Array) || input.length === 0) {
    throw new TypeError("Input must be a non-empty Uint8Array.");
  }
  const extensionBytes = input.length < 15
    ? 0
    : Math.floor((input.length - 15) / 255) + 1;
  const output = new Uint8Array(1 + extensionBytes + input.length);
  let outputIndex = 0;
  output[outputIndex++] = Math.min(input.length, 15) << 4;
  if (input.length >= 15) {
    let remaining = input.length - 15;
    while (remaining >= 255) {
      output[outputIndex++] = 255;
      remaining -= 255;
    }
    output[outputIndex++] = remaining;
  }
  output.set(input, outputIndex);
  return output;
}
