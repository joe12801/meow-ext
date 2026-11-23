#!/usr/bin/env python3
"""生成音源示意 LOGO（160x160 PNG）。

飞牛音乐的列表行只有"封面方框 + 标题 + 专辑"三处能放视觉元素，标题是纯文本，
放不了图片；所以"音源 LOGO"唯一可行的落点就是封面位——在线曲目本来也没有
真实封面（Meow 的搜索结果 cover_url 为空），这个位置正好空着。

用法：python tools/make_source_logos.py
产物：assets/logos/<source>.png
"""
import os
from PIL import Image, ImageDraw, ImageFont

# 渲染成 512 而不是列表实际用的 40~160：播放页会把同一张图放大到 400px 以上，
# 用 160 的原图会糊。512 交给浏览器缩放即可，两侧都清晰。
SIZE = 512
RADIUS = 115

# 键名与 Meow 的 source 值一一对应；品牌色取自各平台主色，
# 目的是在 40px 的缩略图里也能靠色块认出是哪个源。
LOGOS = {
    "kuwo":     ("酷我", "#1B6EF3"),
    "netease":  ("网易", "#D33A31"),
    "kugou":    ("酷狗", "#2BA7E8"),
    "qq":       ("QQ",   "#31C27C"),
    "ximalaya": ("喜马", "#F86442"),
    "fivesing": ("5sing", "#FF6B00"),
    "lizhi":    ("荔枝", "#FF5B4A"),
    "bilibili": ("bili", "#FB7299"),
    "qishui":   ("汽水", "#0DD3C5"),
    "ext":      ("扩展", "#7A5AF8"),
    # YouTube 红：#FF0033 太刺眼，用官方的 #FF0000 略压暗一档，40px 下更稳
    "youtube":  ("YT",   "#E62117"),
    # 兜底：来源未知时用它，免得回到灰色音符占位图
    "meow":     ("Meow", "#6C5CE7"),
}

FONT_CANDIDATES = [
    r"C:\Windows\Fonts\msyhbd.ttc",
    r"C:\Windows\Fonts\msyh.ttc",
    "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
]


def load_font(px):
    for path in FONT_CANDIDATES:
        if os.path.exists(path):
            try:
                return ImageFont.truetype(path, px)
            except OSError:
                continue
    return ImageFont.load_default()


def fit_font(text, max_w, start=272):
    """按文本宽度自适应字号，长的英文名（5sing/bili）不至于溢出。"""
    px = start
    while px > 72:
        f = load_font(px)
        if f.getbbox(text)[2] - f.getbbox(text)[0] <= max_w:
            return f
        px -= 4
    return load_font(72)


def make(label, color):
    img = Image.new("RGBA", (SIZE, SIZE), (0, 0, 0, 0))
    d = ImageDraw.Draw(img)
    d.rounded_rectangle([0, 0, SIZE - 1, SIZE - 1], radius=RADIUS, fill=color)

    font = fit_font(label, SIZE - 110)
    box = d.textbbox((0, 0), label, font=font)
    w, h = box[2] - box[0], box[3] - box[1]
    # textbbox 含字体的上下留白，按 bbox 偏移居中才真的居中
    d.text(((SIZE - w) / 2 - box[0], (SIZE - h) / 2 - box[1]), label,
           font=font, fill=(255, 255, 255, 255))
    return img


def main():
    out = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                       "assets", "logos")
    os.makedirs(out, exist_ok=True)
    for key, (label, color) in LOGOS.items():
        path = os.path.join(out, key + ".png")
        make(label, color).save(path, optimize=True)
        print("%-10s %-6s %s %d bytes" % (key, label, color, os.path.getsize(path)))


if __name__ == "__main__":
    main()
