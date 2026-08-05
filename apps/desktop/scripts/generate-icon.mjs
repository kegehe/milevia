import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { deflateSync } from "node:zlib";

const iconPath = resolve(import.meta.dirname, "../src-tauri/icons/icon.ico");
const size = 256;
const pixels = Buffer.alloc(size * size * 4);

function setPixel(x, y, color) {
  if (x < 0 || x >= size || y < 0 || y >= size) return;
  const index = (y * size + x) * 4;
  pixels[index] = color[0];
  pixels[index + 1] = color[1];
  pixels[index + 2] = color[2];
  pixels[index + 3] = color[3];
}

function drawLine(x1, y1, x2, y2, width, color) {
  const dx = x2 - x1;
  const dy = y2 - y1;
  const lengthSquared = dx * dx + dy * dy;
  const radiusSquared = (width / 2) ** 2;
  const minX = Math.floor(Math.min(x1, x2) - width / 2);
  const maxX = Math.ceil(Math.max(x1, x2) + width / 2);
  const minY = Math.floor(Math.min(y1, y2) - width / 2);
  const maxY = Math.ceil(Math.max(y1, y2) + width / 2);
  for (let y = minY; y <= maxY; y++) {
    for (let x = minX; x <= maxX; x++) {
      const projection = Math.max(0, Math.min(1, ((x - x1) * dx + (y - y1) * dy) / lengthSquared));
      const nearestX = x1 + projection * dx;
      const nearestY = y1 + projection * dy;
      if ((x - nearestX) ** 2 + (y - nearestY) ** 2 <= radiusSquared) setPixel(x, y, color);
    }
  }
}

for (let y = 0; y < size; y++) {
  for (let x = 0; x < size; x++) setPixel(x, y, [25, 51, 44, 255]);
}
drawLine(52, 184, 52, 76, 28, [200, 232, 90, 255]);
drawLine(52, 76, 104, 144, 28, [200, 232, 90, 255]);
drawLine(104, 144, 152, 76, 28, [200, 232, 90, 255]);
drawLine(152, 76, 152, 184, 28, [200, 232, 90, 255]);
drawLine(204, 76, 204, 184, 28, [145, 180, 56, 255]);

function crc32(buffer) {
  let crc = 0xffffffff;
  for (const byte of buffer) {
    crc ^= byte;
    for (let bit = 0; bit < 8; bit++) crc = (crc >>> 1) ^ (0xedb88320 & -(crc & 1));
  }
  return (crc ^ 0xffffffff) >>> 0;
}

function pngChunk(type, data) {
  const header = Buffer.alloc(8);
  header.writeUInt32BE(data.length, 0);
  header.write(type, 4, 4, "ascii");
  const checksum = Buffer.alloc(4);
  checksum.writeUInt32BE(crc32(Buffer.concat([Buffer.from(type, "ascii"), data])), 0);
  return Buffer.concat([header, data, checksum]);
}

const raw = Buffer.alloc((size * 4 + 1) * size);
for (let y = 0; y < size; y++) {
  const row = y * (size * 4 + 1);
  raw[row] = 0;
  pixels.copy(raw, row + 1, y * size * 4, (y + 1) * size * 4);
}
const ihdr = Buffer.alloc(13);
ihdr.writeUInt32BE(size, 0);
ihdr.writeUInt32BE(size, 4);
ihdr[8] = 8;
ihdr[9] = 6;
const png = Buffer.concat([
  Buffer.from("89504e470d0a1a0a", "hex"),
  pngChunk("IHDR", ihdr),
  pngChunk("IDAT", deflateSync(raw)),
  pngChunk("IEND", Buffer.alloc(0)),
]);

const header = Buffer.alloc(22);
header.writeUInt16LE(0, 0);
header.writeUInt16LE(1, 2);
header.writeUInt16LE(1, 4);
header.writeUInt8(0, 6);
header.writeUInt8(0, 7);
header.writeUInt16LE(1, 10);
header.writeUInt16LE(32, 12);
header.writeUInt32LE(png.length, 14);
header.writeUInt32LE(22, 18);
mkdirSync(dirname(iconPath), { recursive: true });
writeFileSync(iconPath, Buffer.concat([header, png]));
