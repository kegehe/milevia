# 生成 Milevia 品牌安装向导横幅(NSIS/WiX), 延续品牌配色(深绿底 #19332C + 亮绿 #C8E85A)。
# 4x 超采样保证质量; 中文强制微软雅黑避免乱码。
from PIL import Image, ImageDraw, ImageFont
import os, re

S = 4  # 超采样倍率 (侧边/背景图用较高)

# ── 渐变背景 (垂直: 顶部深 → 中部品牌深绿 → 底部略亮) ──
def _gradient(w4, h4, top, mid, bot):
    img = Image.new("RGB", (w4, h4))
    d = ImageDraw.Draw(img)
    for y in range(h4):
        t = y / max(1, h4 - 1)
        if t < 0.5:
            f = t / max(0.001, 0.5)
            c = tuple(int(top[i] + (mid[i] - top[i]) * f) for i in range(3))
        else:
            f = (t - 0.5) / 0.5
            c = tuple(int(mid[i] + (bot[i] - mid[i]) * f) for i in range(3))
        d.line([0, y, w4, y], fill=c)
    return img

# 圆角矩形 (NSIS 展示为直角, 圆角在整幅图里不显形, 故徽标用直角圆即可)
def _rounded(d, box, r, fill, outline=None, width=1):
    x0, y0, x1, y1 = box
    d.rectangle(box, fill=fill, outline=outline, width=width)

# 精致的 logo 徽标: 圆角浅色底 + 居中亮绿 M
def draw_badge(d, cx, cy, wx, hx, px_scale):
    x0, y0 = int(cx - wx/2), int(cy - hx/2)
    x1, y1 = int(cx + wx/2), int(cy + hx/2)
    # 白色/极浅描边徽标底
    _rounded(d, (x0, y0, x1, y1), r=int(16*px_scale), fill=(245, 250, 245), outline=(205, 225, 205), width=int(1*px_scale))
    # 内部 M logo (深绿, 非亮绿, 徽标白底配深绿字更像正式品牌)
    mw, mh = int(wx*0.5), int(hx*0.55)
    mw2 = mw * px_scale
    lw = max(4, int(mw*0.18))
    mx0 = cx - mw/2; my0 = cy - mh/2
    pts = [(mx0, my0+mh), (mx0, my0), (mx0+mw*0.52, my0+mh), (mx0+mw, my0), (mx0+mw, my0+mh)]
    d.line(pts, fill=DEEP_X, width=lw, joint="curve")

# 文字带轻微投影提升层次
def shadow_cx(d, cx, y, s, px, fill, dx=0, dy=2):
    text_cx(d, cx+dx, y+dy, s, px, (0, 0, 0, 40) if fill==WHITE else (0,0,0,30))
    text_cx(d, cx, y, s, px, fill)

DEEP   = (25, 51, 44)     # 主背景
DEEP_X = (20, 42, 37)     # 深色(徽标里的 M / 强调)
DEEP2  = (31, 60, 53)     # 视觉分区
LIGHT  = (200, 232, 90)   # 亮绿(logo / 强调)
WHITE  = (255, 255, 255)
GREY   = (190, 208, 201)  # 次级文字

OUT = os.path.join(os.path.dirname(__file__), "..", "src-tauri", "icons")
os.makedirs(OUT, exist_ok=True)

_CJK = re.compile(r'[\u3000-\u303f\u4e00-\u9fff\uac00-\ud7af\u3040-\u30ff]')

def font(px, text):
    has_cjk = bool(_CJK.search(text))
    for p in (r"C:/Windows/Fonts/msyhbd.ttc" if has_cjk else r"C:/Windows/Fonts/seguib.ttf",
              r"C:/Windows/Fonts/msyh.ttc" if has_cjk else r"C:/Windows/Fonts/segoeui.ttf",
              r"C:/Windows/Fonts/arialbd.ttf", r"C:/Windows/Fonts/arial.ttf"):
        if os.path.exists(p):
            try: return ImageFont.truetype(p, px)
            except Exception: pass
    return ImageFont.load_default()

# 画 Milevia 的 M 形折线 logo(亮绿)。
def draw_logo(d, cx, cy, px_scale):
    w, h = 34 * px_scale, 48 * px_scale
    lw = max(4, int(6 * px_scale))
    x0, y0 = cx - w / 2, cy - h / 2
    pts = [(x0, y0 + h), (x0, y0), (x0 + w * 0.52, y0 + h), (x0 + w, y0), (x0 + w, y0 + h)]
    d.line(pts, fill=LIGHT, width=lw, joint="curve")

def text_at(d, x, y, s, px, fill):
    f = font(px, s)
    b = d.textbbox((0, 0), s, font=f)
    d.text((x - b[0], y - b[1]), s, font=f, fill=fill)

def text_cx(d, cx, y, s, px, fill):
    f = font(px, s)
    b = d.textbbox((0, 0), s, font=f)
    d.text((cx - (b[2] - b[0]) / 2 - b[0], y - b[1]), s, font=f, fill=fill)

def canvas(w, h):
    return Image.new("RGB", (w * S, h * S), DEEP)

def finish(cv, w, h):
    return cv.resize((w, h), Image.LANCZOS)

def build(w, h, paint, gradient=False):
    cv = _gradient(w * S, h * S, (16, 33, 29), DEEP, DEEP2) if gradient else canvas(w, h)
    paint(ImageDraw.Draw(cv), cv.size, w, h)
    return finish(cv, w, h)

def save(img, name):
    img.save(os.path.join(OUT, name))

# ── 1. NSIS sidebar 164x314: 渐变底, 居中徽标, 大标题, 信息区, 底部 ──
def side(d, cv, W, H):
    W4, H4 = cv
    # 上部 logo 徽标(圆角底 + M)
    draw_badge(d, W4/2, H4*0.13, int(W4*0.30), int(W4*0.30), S)
    # 大标题
    shadow_cx(d, W4/2, H4*0.24, "Milevia", 40*S, WHITE)
    text_cx(d, W4/2, H4*0.325, "多项目 AI 开发平台", 13*S, LIGHT)
    # 信息区: 简洁三行, 亮绿小圆点装饰
    lines = ["支持 Claude Code / Codex", "本地 / WSL / SSH 项目", "一步接入 AI 开发"]
    y0 = H4*0.47
    for i, ln in enumerate(lines):
        y = y0 + i*H4*0.07
        # 小圆点
        d.ellipse([W4*0.24, y-H4*0.012, W4*0.24+H4*0.014, y+H4*0.002], fill=LIGHT)
        text_cx(d, W4/2 + W4*0.01, y - H4*0.03, ln, 10*S, GREY)
    # 底部日志感分隔 + 小字
    d.rectangle([int(W4*0.18), int(H4*0.87), int(W4*0.82), int(H4*0.87)+2*S], fill=LIGHT)
    text_cx(d, W4/2, int(H4*0.905), "milevia", 11*S, GREY)
save(build(164, 314, side, gradient=True), "nsis-sidebar.bmp")
print("nsis-sidebar.bmp")

# ── 2. NSIS header 150x57: 渐变底, 左徽标 + 大字 ──
def hdr(d, cv, W, H):
    W4, H4 = cv
    draw_badge(d, W4*0.085, H4/2, int(H4*0.72), int(H4*0.72), S*0.55)
    d.rectangle([int(W4*0.16), int(H4*0.2), int(W4*0.16)+2*S, int(H4*0.8)], fill=LIGHT)
    text_at(d, W4*0.20, H4*0.28, "Milevia", 17*S, WHITE)
    text_at(d, W4*0.20, H4*0.62, "多项目 AI 开发", 6*S, GREY)
save(build(150, 57, hdr, gradient=True), "nsis-header.bmp")
print("nsis-header.bmp")

# ── 3. WiX banner 493x58: 渐变底, 左徽标 + 居中大字 ──
def wb(d, cv, W, H):
    W4, H4 = cv
    draw_badge(d, W4*0.034, H4/2, int(H4*0.78), int(H4*0.78), S*0.6)
    d.rectangle([int(W4*0.058), int(H4*0.18), int(W4*0.058)+3*S, int(H4*0.82)], fill=LIGHT)
    text_cx(d, W4*0.54, H4*0.30, "Milevia", 24*S, WHITE)
    text_cx(d, W4*0.54, H4*0.66, "多项目 AI 开发平台", 8*S, LIGHT)
save(build(493, 58, wb, gradient=True), "wix-banner.bmp")
print("wix-banner.bmp")

# ── 4. WiX background 493x312: 左 logo+大字, 右信息区 ──
def wbg(d, cv, W, H):
    W4, H4 = cv
    draw_logo(d, W4 * 0.08, H4 * 0.22, 1.3 * S)
    text_at(d, W4 * 0.14, H4 * 0.13, "Milevia", 34 * S, WHITE)
    text_at(d, W4 * 0.14, H4 * 0.30, "多项目 AI 自动开发平台", 11 * S, LIGHT)
    text_at(d, W4 * 0.14, H4 * 0.39, "Claude Code / Codex 驱动的", 8 * S, GREY)
    text_at(d, W4 * 0.14, H4 * 0.46, "持续 AI 开发", 8 * S, GREY)
    x0 = int(W4 * 0.63)
    x1 = int(W4 * 0.97)
    d.rectangle([x0, int(H4*0.06), x1, int(H4*0.94)], outline=LIGHT, width=2*S)
    # 内部渐变(垂直深→更深)
    top, bot = int(H4*0.06), int(H4*0.94)
    n = 50
    for i in range(n):
        t = i / max(1, n-1)
        c = (int(25 + 3*t), int(51 + 8*t), int(44 + 5*t))
        y = top + int((bot-top)*i/n)
        d.line([x0+2*S, y, x1-2*S, y], fill=c)
    draw_logo(d, x0 + (x1-x0)//2, int(H4*0.5), 0.8*S)
    text_cx(d, x0+(x1-x0)//2, int(H4*0.68), "MILEVIA", 10*S, WHITE)
    text_cx(d, x0+(x1-x0)//2, int(H4*0.78), "欢迎使用", 9*S, LIGHT)
save(build(493, 312, wbg), "wix-background.bmp")
print("wix-background.bmp")

print("done ->", os.path.abspath(OUT))
