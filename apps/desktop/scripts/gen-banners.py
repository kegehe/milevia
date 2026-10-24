# -*- coding: utf-8 -*-
# 生成 Milevia 品牌安装向导图 (C2 终端开发者风, 2026-10-21 定稿)。
# 设计: 深色网格底 + 真实应用图标(亮绿描边) + 终端卡片 $ milevia init。
# 管线: HTML @4x -> Playwright 截图 -> LANCZOS 缩回逻辑尺寸 -> 24 位 BMP。
# 依赖: PIL + playwright (本机 chromium-1169, 缺失时回落到 playwright 默认浏览器)。
# 产物: src-tauri/icons/{nsis-sidebar,nsis-header,wix-banner}.bmp
import base64
import os
import tempfile

try:
    from PIL import Image
    from playwright.sync_api import sync_playwright
except ImportError as exc:
    raise SystemExit(f"缺少依赖: {exc} —— 请在装有 Pillow + playwright 的 Python 环境运行本脚本")

HERE = os.path.dirname(os.path.abspath(__file__))
ICONS = os.path.join(HERE, "..", "src-tauri", "icons")

# ── 品牌色 (采样自 icons/icon.png) ──
DEEP_BG = "#0c1512"        # 侧边栏底色
DEEP_BAR = "#0e1a15"       # 顶栏/横幅底色
LIME = "#c8e85a"           # 亮绿 (品牌 M 色)
GREY = "#5c766a"           # 注脚灰绿
RING = "box-shadow:0 0 0 1px rgba(200,232,90,.28),0 2px 8px rgba(0,0,0,.5)"

FONT_UI = "'Segoe UI','Microsoft YaHei',sans-serif"
FONT_MONO = "Consolas,'Microsoft YaHei',monospace"
INFO_LINES = ["Claude Code / Codex", "本地 / WSL / SSH", "一步接入 AI 开发"]
GRID = (
    "background-image:"
    "radial-gradient(140% 60% at 50% 0%,rgba(200,232,90,.07),transparent 60%),"
    "linear-gradient(rgba(200,232,90,.045) 1px,transparent 1px),"
    "linear-gradient(90deg,rgba(200,232,90,.045) 1px,transparent 1px);"
    "background-size:100% 100%,22px 22px,22px 22px"
)

_icon_path = os.path.join(ICONS, "icon.png")
if not os.path.exists(_icon_path):
    raise SystemExit(f"找不到 {_icon_path} —— 请先准备应用图标 icon.png 再生成本安装图")
with open(_icon_path, "rb") as f:
    ICON_B64 = "data:image/png;base64," + base64.b64encode(f.read()).decode()


def _stage(inner, w, h, z):
    return (
        f'<div class="stagewrap" style="width:{w * z}px;height:{h * z}px">'
        f'<div style="width:{w}px;height:{h}px;transform:scale({z});transform-origin:0 0">{inner}</div></div>'
    )


def _icon(size, radius):
    return f'<img src="{ICON_B64}" style="width:{size}px;height:{size}px;border-radius:{radius}px;{RING}">'


# ── NSIS 侧边栏 164x314 ──
def _sidebar():
    def ln(inner):
        return f'<div style="line-height:2.05">{inner}</div>'

    body = (
        ln(f'<span style="color:{LIME}">$ milevia init</span>')
        + "".join(
            ln(f'<span style="color:#8fb3a4">&#10003;</span> <span style="color:#bccfc5">{t}</span>')
            for t in INFO_LINES
        )
        + ln(f'<span style="display:inline-block;width:6px;height:11px;background:{LIME};vertical-align:-1px"></span>')
    )
    dots = "".join(
        f'<span style="width:7px;height:7px;border-radius:50%;background:{c};display:inline-block"></span>'
        for c in ("#e2695c", "#e2b45c", "#9cc260")
    )
    return f"""
<div style="width:164px;height:314px;position:relative;overflow:hidden;font-family:{FONT_MONO};
  background:{DEEP_BG};{GRID}">
  <div style="position:absolute;right:-18px;bottom:-38px;font-family:{FONT_UI};font-weight:800;
    font-size:104px;line-height:1;color:rgba(200,232,90,.055);letter-spacing:-4px">Mi</div>
  <div style="position:relative;display:flex;align-items:center;gap:9px;padding:19px 15px 0">
    {_icon(34, 9)}
    <span style="font-size:15px;font-weight:700;color:#fff;letter-spacing:.5px">milevia</span>
  </div>
  <div style="position:relative;margin:16px 14px 0;background:#15221d;border:1px solid #2a423a;
    border-radius:9px;overflow:hidden;box-shadow:0 6px 18px rgba(0,0,0,.35)">
    <div style="height:21px;background:#1b2b25;display:flex;align-items:center;gap:5px;padding:0 9px;box-sizing:border-box">
      {dots}
      <span style="margin-left:auto;font-size:7px;color:{GREY};letter-spacing:1px">安装向导</span>
    </div>
    <div style="padding:10px 11px 12px;font-size:9px;color:#bccfc5">{body}</div>
  </div>
  <div style="position:absolute;bottom:14px;left:0;right:0;text-align:center;font-size:8.5px;letter-spacing:2px;color:{GREY}">milevia</div>
</div>"""


# ── NSIS 顶栏 150x57 ──
def _header():
    return f"""
<div style="width:150px;height:57px;display:flex;align-items:center;padding:0 10px;box-sizing:border-box;
  font-family:{FONT_MONO};background:{DEEP_BAR};{GRID}">
  {_icon(33, 8)}
  <div style="display:flex;flex-direction:column;margin-left:9px">
    <div style="font-size:15px;font-weight:700;color:#fff;letter-spacing:.5px">milevia<span style="display:inline-block;width:5px;height:11px;background:{LIME};margin-left:4px;vertical-align:-1px"></span></div>
  </div>
</div>"""


# ── WiX 横幅 493x58 ──
def _wix_banner():
    return f"""
<div style="width:493px;height:58px;display:flex;align-items:center;padding:0 16px;box-sizing:border-box;
  font-family:{FONT_MONO};background:{DEEP_BAR};{GRID}">
  {_icon(40, 10)}
  <div style="display:flex;align-items:baseline;margin-left:12px">
    <span style="font-size:19px;font-weight:700;color:#fff;letter-spacing:.5px">milevia</span>
  </div>
  <div style="margin-left:auto;font-size:9px;color:{LIME};white-space:nowrap">$ milevia init</div>
</div>"""


def _render(shots):
    """shots: [(name, html, logical_w, logical_h, zoom), ...] -> {name: PIL.Image}"""
    out = {}
    with tempfile.TemporaryDirectory() as tmp:
        html_path = os.path.join(tmp, "banners.html")
        raw_path = os.path.join(tmp, "raw.png")
        y = 0
        marks = []
        for name, html, w, h, z in shots:
            marks.append((name, y, w * z, h * z, w, h))
            y += h * z
        body = "".join(_stage(s[1], s[2], s[3], s[4]) for s in shots)
        page = (
            '<!DOCTYPE html><html><head><meta charset="utf-8"><style>'
            "body{margin:0;background:#fff}img{display:block}"
            ".stagewrap{position:relative;overflow:hidden}"
            "</style></head><body>" + body + "</body></html>"
        )
        with open(html_path, "w", encoding="utf-8") as f:
            f.write(page)
        with sync_playwright() as p:
            exe = os.path.expandvars(
                r"%LOCALAPPDATA%\ms-playwright\chromium-1169\chrome-win\chrome.exe")
            kwargs = {"executable_path": exe} if os.path.exists(exe) else {}
            browser = p.chromium.launch(**kwargs)
            pg = browser.new_page(
                viewport={"width": max(s[2] * s[4] for s in shots), "height": 1400},
                device_scale_factor=1,
            )
            pg.goto("file:///" + html_path.replace("\\", "/"))
            pg.wait_for_timeout(400)
            pg.screenshot(path=raw_path, full_page=True)
            browser.close()
        raw = Image.open(raw_path)
        for name, oy, w, h, lw, lh in marks:
            out[name] = raw.crop((0, oy, w, oy + h)).resize((lw, lh), Image.LANCZOS)
    return out


def main():
    shots = [
        ("nsis-sidebar", _sidebar(), 164, 314, 4),
        ("nsis-header", _header(), 150, 57, 4),
        ("wix-banner", _wix_banner(), 493, 58, 2),
    ]
    images = _render(shots)
    for name, img in images.items():
        dst = os.path.join(ICONS, name + ".bmp")
        img.convert("RGB").save(dst)
        check = Image.open(dst)
        print(f"{name}.bmp  {check.size}  mode={check.mode}  -> {dst}")


if __name__ == "__main__":
    main()
