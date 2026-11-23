"""生成 meow-ext 的应用图标。

设计：圆角方形渐变底 + 五条等距声波条（中间高两侧低）。
不用文字，避免 64px 下糊成一团；声波在 64px 仍然可辨认。
"""
from PIL import Image, ImageDraw

SCALE = 4          # 超采样倍数，缩小后边缘才干净
BASE = 256 * SCALE


def lerp(a, b, t):
    return tuple(int(x + (y - x) * t) for x, y in zip(a, b))


def build(size):
    img = Image.new("RGBA", (BASE, BASE), (0, 0, 0, 0))
    d = ImageDraw.Draw(img)

    # 渐变底：逐行画线比逐像素快得多，256*4 行的量级完全够用。
    top = (0x3B, 0x6F, 0xF0)
    bottom = (0x7B, 0x4F, 0xD8)
    for y in range(BASE):
        d.line([(0, y), (BASE, y)], fill=lerp(top, bottom, y / (BASE - 1)))

    # 圆角遮罩
    mask = Image.new("L", (BASE, BASE), 0)
    ImageDraw.Draw(mask).rounded_rectangle(
        [0, 0, BASE - 1, BASE - 1], radius=int(BASE * 0.22), fill=255
    )
    img.putalpha(mask)

    # 五条声波：高度比 0.34 / 0.62 / 0.86 / 0.55 / 0.38
    ratios = [0.34, 0.62, 0.88, 0.55, 0.34]
    bar_w = int(BASE * 0.075)
    gap = int(BASE * 0.055)
    total = len(ratios) * bar_w + (len(ratios) - 1) * gap
    x = (BASE - total) // 2
    cy = BASE // 2
    for r in ratios:
        h = int(BASE * r)
        y0 = cy - h // 2
        d.rounded_rectangle(
            [x, y0, x + bar_w, y0 + h],
            radius=bar_w // 2,
            fill=(255, 255, 255, 240),
        )
        x += bar_w + gap

    return img.resize((size, size), Image.LANCZOS)


if __name__ == "__main__":
    import pathlib

    fpk = pathlib.Path("packaging/fpk")
    (fpk / "icons").mkdir(parents=True, exist_ok=True)
    (fpk / "app/ui/images").mkdir(parents=True, exist_ok=True)

    big = build(256)
    big.save(fpk / "ICON_256.PNG", "PNG")
    build(64).save(fpk / "ICON.PNG", "PNG")
    # icons/ 供打包脚本取用；app/ui/images/ 是飞牛桌面直接读的落地位置，
    # 两处都写，避免绕开 build.sh 手工打包时缺图。
    big.save(fpk / "icons/icon_256.png", "PNG")
    build(64).save(fpk / "icons/icon_64.png", "PNG")
    big.save(fpk / "app/ui/images/icon_256.png", "PNG")
    build(64).save(fpk / "app/ui/images/icon_64.png", "PNG")

    # 预览图放在仓库根，方便 README 引用
    build(512).save("preview_icon.png", "PNG")
    print("图标已生成")
