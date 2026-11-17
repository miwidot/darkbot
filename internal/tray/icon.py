# Generates icon.ico: python3 internal/tray/icon.py internal/tray/icon.ico /tmp/icon256.png (needs Pillow).
# Draws darkbot's tray icon: a dark sound pad with the amber LED (the app's
# one accent color), rendered at 4x and downsampled per size for clean edges.
import sys
from PIL import Image, ImageDraw, ImageFilter
PAD, EDGE, LED, GLOW = (34, 40, 52, 255), (62, 70, 89, 255), (255, 178, 63, 255), (255, 178, 63, 110)
def draw(size):
    s = size * 4
    im = Image.new("RGBA", (s, s), (0, 0, 0, 0))
    d = ImageDraw.Draw(im)
    r = int(s * 0.22)
    d.rounded_rectangle([0, 0, s - 1, s - 1], radius=r, fill=EDGE)
    b = max(4, int(s * 0.06))
    d.rounded_rectangle([b, b, s - 1 - b, s - 1 - b], radius=r - b, fill=PAD)
    glow = Image.new("RGBA", (s, s), (0, 0, 0, 0))
    gd = ImageDraw.Draw(glow)
    c, gr = s / 2, s * 0.30
    gd.ellipse([c - gr, c - gr, c + gr, c + gr], fill=GLOW)
    glow = glow.filter(ImageFilter.GaussianBlur(s * 0.06))
    im = Image.alpha_composite(im, glow)
    d = ImageDraw.Draw(im)
    lr = s * 0.21
    d.ellipse([c - lr, c - lr, c + lr, c + lr], fill=LED)
    return im.resize((size, size), Image.LANCZOS)
sizes = [16, 20, 24, 32, 40, 48, 64, 256]
big = draw(256)
big.save(sys.argv[1], format="ICO", sizes=[(n, n) for n in sizes], append_images=[draw(n) for n in sizes[:-1]])
draw(256).save(sys.argv[2])
