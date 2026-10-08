// 生成网页版（PWA）所需的位图图标 —— 从 public/milevia-mark.svg 出 192/512 与 iOS 图标。
//
// 为什么非要有位图：manifest 里只放一份 SVG 时，Chrome for Android 生成 WebAPK 找不到
// 可用的 192/512 PNG，装出来的快捷方式会退化成首字母方块；iOS 则根本不看 manifest，
// 只认 <link rel="apple-touch-icon">，缺了就截页面当图标。矢量那份继续留着当标签页图标。
//
// 两类图形各有各的用法，不能混：
//   rounded    圆角矩形底 + 原样图形，给 purpose:any 与标签页（保持品牌外观）
//   fullBleed  满幅方底 + 缩进安全区的图形，给 purpose:maskable 与 apple-touch-icon
//              —— 安卓按自己的形状（圆/水滴/方）裁切，圆角素材被裁会露透明角；
//                 iOS 自己加圆角，预先画了圆角会变成双层圆角，所以这类素材一律画满幅
//
// 用法：pnpm icons（或 node scripts/generate-icons.mjs）
// 依赖 playwright（devDependency）；首次跑需要 `pnpm exec playwright install chromium`。
import { readFile, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright";

const publicDir = fileURLToPath(new URL("../public/", import.meta.url));
const sourcePath = `${publicDir}milevia-mark.svg`;
const BACKGROUND = "#19332C";
// "#RRGGBB" -> [r, g, b]：判定"哪些像素属于图形"用
const BACKGROUND_RGB = [1, 3, 5].map((offset) => parseInt(BACKGROUND.slice(offset, offset + 2), 16));

// 图形（两段 stroke 路径）在 64×64 里的包围盒是 x 9.5–49.5、y 15.5–49.5，中心 (29.5, 32.5)。
// 照原样当 maskable 也勉强塞得进中心 80% 的安全圆（实测 24.9 / 25.6），但笔画会贴着圆形
// 裁切线，所以缩到 0.78 再移回正中（实测 19.4 / 25.6），给各种裁切形状留出余量。
const GLYPH_CENTER = [29.5, 32.5];
const GLYPH_SCALE = 0.78;

const source = await readFile(sourcePath, "utf8");
const glyph = [...source.matchAll(/<path[\s\S]*?\/>/g)].map((match) => match[0]).join("");
if (!glyph) throw new Error(`${sourcePath} 里没有解析到任何 <path>，图标会画成一块纯色底板`);

// svg 与 fullBleed 绑在同一个对象里，避免"满幅的图配了非满幅的开关"这种配错还能跑的组合
const ARTWORKS = {
  rounded: { fullBleed: false, svg: source.trim() },
  fullBleed: {
    fullBleed: true,
    svg: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" fill="none">
  <rect width="64" height="64" fill="${BACKGROUND}"/>
  <g transform="translate(32 32) scale(${GLYPH_SCALE}) translate(${-GLYPH_CENTER[0]} ${-GLYPH_CENTER[1]})">${glyph}</g>
</svg>`,
  },
};

const targets = [
  { file: "icon-192.png", size: 192, artwork: ARTWORKS.rounded },
  { file: "icon-512.png", size: 512, artwork: ARTWORKS.rounded },
  { file: "icon-maskable-192.png", size: 192, artwork: ARTWORKS.fullBleed },
  { file: "icon-maskable-512.png", size: 512, artwork: ARTWORKS.fullBleed },
  { file: "apple-touch-icon.png", size: 180, artwork: ARTWORKS.fullBleed },
];

// 出图后回读一遍，把手算的安全区常量变成实测断言 —— 源 SVG 改了而常量没跟着改时
// （图形顶出安全圆、圆角被去掉）在这里直接报错，而不是等装到手机上才发现被切。
// 用画布量真实 PNG 而不是量 DOM 包围盒：包围盒既不含描边也不含抗锯齿，量不准；
// 「有没有透明通道」则直接读 PNG 头的 colorType（见下），比读像素可靠。
// 注意校验的是像素级不变量（尺寸 / 透明度 / 安全区），不是字节哈希：PNG 的压缩字节由
// Chromium 编码器产出，重跑出现"像素一致、二进制不同"的 diff 属正常，别当回归看。
async function inspect(page, png) {
  return page.evaluate(
    async ({ dataUrl, rgb }) => {
      const image = new Image();
      image.src = dataUrl;
      await image.decode();
      const canvas = document.createElement("canvas");
      canvas.width = image.naturalWidth;
      canvas.height = image.naturalHeight;
      const context = canvas.getContext("2d", { willReadFrequently: true });
      context.drawImage(image, 0, 0);
      const { data } = context.getImageData(0, 0, canvas.width, canvas.height);
      const half = canvas.width / 2;
      let reach = 0;
      for (let y = 0; y < canvas.height; y += 1) {
        for (let x = 0; x < canvas.width; x += 1) {
          const offset = (y * canvas.width + x) * 4;
          const alpha = data[offset + 3];
          // 「图形像素」= 与满幅底板色明显不同、且不透明的像素。
          // 留 8 级容差是为了容忍渲染与编码的舍入：底板色只要被舍入 ±1 就会被算成图形，
          // 整块底板都成了"图形"，安全区自检也就失去意义了。
          const isGlyph =
            alpha === 255 && (Math.abs(data[offset] - rgb[0]) > 8 || Math.abs(data[offset + 1] - rgb[1]) > 8 || Math.abs(data[offset + 2] - rgb[2]) > 8);
          if (isGlyph) reach = Math.max(reach, Math.hypot(x + 0.5 - half, y + 0.5 - half));
        }
      }
      // 角像素的 alpha 只对"透明角"那类素材有意义（满幅素材的角天然不透明）
      return { reach, cornerAlpha: data[3] };
    },
    { dataUrl: `data:image/png;base64,${png.toString("base64")}`, rgb: BACKGROUND_RGB },
  );
}

const browser = await chromium.launch();
try {
  const page = await browser.newPage({ viewport: { width: 64, height: 64 }, deviceScaleFactor: 1 });
  for (const target of targets) {
    const { svg, fullBleed } = target.artwork;
    await page.setViewportSize({ width: target.size, height: target.size });
    await page.setContent(
      `<!doctype html><meta charset="utf-8"><style>
        html, body { margin: 0; padding: 0; width: ${target.size}px; height: ${target.size}px; overflow: hidden; background: transparent; }
        svg { display: block; width: ${target.size}px; height: ${target.size}px; }
      </style>${svg}`,
    );
    // 不透明那几张要压掉透明通道：iOS 会把 alpha 直接填成黑色，Android 剪影下也会露底。
    const png = await page.screenshot({ omitBackground: !fullBleed });
    const width = png.readUInt32BE(16);
    const height = png.readUInt32BE(20);
    if (width !== target.size || height !== target.size) {
      throw new Error(`${target.file} 尺寸不对：期望 ${target.size}×${target.size}，实际 ${width}×${height}`);
    }

    const stats = await inspect(page, png);
    const safeRadius = target.size * 0.4;
    // IHDR 里的颜色类型：2=RGB（无 alpha 通道），6=RGBA。这条查的是落盘文件本身有没有
    // 透明通道 —— 不能靠画布读像素来判断：omitBackground:false 时画面已被合成到白底上，
    // 读出来永远是 alpha 255，那样的断言永远不会失败，等于没查。
    const colorType = png[25];
    if (fullBleed && colorType !== 2) {
      throw new Error(`${target.file} 的 PNG 带 alpha 通道（colorType ${colorType}），iOS 会把它填成黑底（满幅素材要 omitBackground:false）`);
    }
    if (!fullBleed && colorType !== 6) {
      throw new Error(`${target.file} 应当是有透明角的 RGBA（colorType ${colorType}），检查 omitBackground 与源 SVG 的圆角`);
    }
    if (fullBleed && stats.reach === 0) {
      throw new Error(`${target.file} 里没找出任何与底板不同的图形像素，安全区自检形同虚设（图形颜色和底板太接近？）`);
    }
    if (fullBleed && stats.reach > safeRadius) {
      throw new Error(`${target.file} 的图形伸出安全圆：${stats.reach.toFixed(1)}px > ${safeRadius.toFixed(1)}px，换圆形裁切会切到笔画（检查 GLYPH_SCALE）`);
    }
    if (!fullBleed && stats.cornerAlpha !== 0) {
      throw new Error(`${target.file} 的圆角外应当是透明的，实测不透明（检查源 SVG 的圆角与 rx）`);
    }

    await writeFile(`${publicDir}${target.file}`, png);
    console.log(
      `${target.file}  ${width}×${height}  ${(png.length / 1024).toFixed(1)} KB` +
        (fullBleed ? `  图形最远处 ${stats.reach.toFixed(1)}px / 安全圆 ${safeRadius.toFixed(1)}px` : ""),
    );
  }
} finally {
  await browser.close();
}
