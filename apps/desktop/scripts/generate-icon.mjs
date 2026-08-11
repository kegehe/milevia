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
  if (lengthSquared === 0) return; // 零长度线段无意义，避免除零
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

// 背景：圆角深绿卡片（四角透明），而非整张不透明方形。
// 四角外沿用距离判断剔除，圆角半径按 SVG 的 rx16/64 等比放大（64）。
const cornerRadius = 64; // size * (16/64)
const bg = [25, 51, 44, 255];
for (let y = 0; y < size; y++) {
  for (let x = 0; x < size; x++) {
    // 判断像素是否落在某个角的圆角之外（距离该圆角圆心平方 > 半径²）→ 透明
    const dx = Math.min(x, size - 1 - x);
    const dy = Math.min(y, size - 1 - y);
    const insideCorner = dx < cornerRadius && dy < cornerRadius;
    if (
      insideCorner &&
      (dx - cornerRadius) ** 2 + (dy - cornerRadius) ** 2 > cornerRadius ** 2
    ) {
      continue; // 保持透明（不填背景）
    }
    setPixel(x, y, bg);
  }
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

/** 将固定尺寸的 RGBA buffer 编码为 PNG（尺寸由入参决定）。 */
function encodePNG(s, buf) {
  const raw = Buffer.alloc((s * 4 + 1) * s);
  for (let y = 0; y < s; y++) {
    const row = y * (s * 4 + 1);
    raw[row] = 0; // filter none
    buf.copy(raw, row + 1, y * s * 4, (y + 1) * s * 4);
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(s, 0);
  ihdr.writeUInt32BE(s, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 6; // color type RGBA
  return Buffer.concat([
    Buffer.from("89504e470d0a1a0a", "hex"),
    pngChunk("IHDR", ihdr),
    pngChunk("IDAT", deflateSync(raw)),
    pngChunk("IEND", Buffer.alloc(0)),
  ]);
}

/**
 * 从 s×s RGBA 主图按“alpha 感知的面积平均”缩到 t×t。
 * 预乘 alpha 平均再回除，避免透明边缘出现灰边/缝合线，保住圆角干净。
 */
function downscale(src, s, t) {
  const ratio = s / t;
  const out = Buffer.alloc(t * t * 4);
  for (let y = 0; y < t; y++) {
    for (let x = 0; x < t; x++) {
      // 采样源范围 [x0,x1) x [y0,y1)
      const x0 = Math.floor(x * ratio);
      const x1 = Math.min(s, Math.max(x0 + 1, Math.floor((x + 1) * ratio)));
      const y0 = Math.floor(y * ratio);
      const y1 = Math.min(s, Math.max(y0 + 1, Math.floor((y + 1) * ratio)));
      let r = 0, g = 0, b = 0, a = 0;
      for (let yy = y0; yy < y1; yy++) {
        for (let xx = x0; xx < x1; xx++) {
          const i = (yy * s + xx) * 4;
          const alpha = src[i + 3];
          r += src[i] * alpha;
          g += src[i + 1] * alpha;
          b += src[i + 2] * alpha;
          a += alpha;
        }
      }
      const n = (x1 - x0) * (y1 - y0);
      const o = (y * t + x) * 4;
      if (a > 0) {
        out[o] = Math.round(r / a);
        out[o + 1] = Math.round(g / a);
        out[o + 2] = Math.round(b / a);
        out[o + 3] = Math.round(a / n);
      } else {
        out[o] = out[o + 1] = out[o + 2] = out[o + 3] = 0;
      }
    }
  }
  return out;
}

// 主图为 256×256（已在开头绘制）。从这里派生各尺寸，再打包成多条目 ICO。
const master = pixels; // RGBA 256×256
const srcSize = 256;
const targetSizes = [256, 128, 64, 48, 32, 24, 16].filter((s) => s <= srcSize);
const entries = []; // {size, png}
for (const t of targetSizes) {
  const buf = t === srcSize ? master : downscale(master, srcSize, t);
  entries.push({ size: t, png: encodePNG(t, buf) });
}

// 组装 ICO：ICONDIR 头 + 每个尺寸的 ICONDIRENTRY + 依次排列的 PNG 数据。
const count = entries.length;
const dirSize = 6 + 16 * count;
const dataSize = entries.reduce((sum, e) => sum + e.png.length, 0);
const header = Buffer.alloc(dirSize + dataSize);
header.writeUInt16LE(0, 0); // reserved
header.writeUInt16LE(1, 2); // type: icon
header.writeUInt16LE(count, 4); // image count
let offset = dirSize;
entries.forEach((e, i) => {
  const base = 6 + i * 16;
  header.writeUInt8(e.size >= 256 ? 0 : e.size, base); // width (0 => 256)
  header.writeUInt8(e.size >= 256 ? 0 : e.size, base + 1); // height
  header.writeUInt8(0, base + 2); // color count
  header.writeUInt8(0, base + 3); // reserved
  header.writeUInt16LE(1, base + 4); // planes
  header.writeUInt16LE(32, base + 6); // bit count
  header.writeUInt32LE(e.png.length, base + 8); // bytes in resource
  header.writeUInt32LE(offset, base + 12); // image offset
  e.png.copy(header, offset);
  offset += e.png.length;
});
mkdirSync(dirname(iconPath), { recursive: true });
writeFileSync(iconPath, header);
