# Generates darkbot's app and tray icons from one drawing (see README in
# this folder). Needs rsvg-convert (librsvg), Pillow and, for macOS,
# iconutil:  python3 build/icon/icon.py
#
# The drawing is darkbot's own UI: a graphite console tile holding a 2x2
# grid of sound pads, one of them lit by the amber LED (the app's one
# accent color), like a pad that is playing.
import os, subprocess, sys, tempfile
from PIL import Image

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))

# Colors from frontend/dist/style.css.
METAL_0, METAL_3 = "#14171e", "#2e3544"
# Unlit pads are lighter than the app's --metal-2 so the grid still reads
# at 16 px on a dark taskbar; simple (tray) sizes lighter still.
PAD_TOP, PAD_BOTTOM, PAD_TINY = "#3d4559", "#2b3242", "#4a546b"
LED, LED_HOT = "#ffb23f", "#ffd38a"

def svg(full_bleed, simple=False):
    """The icon on a 1024 canvas. macOS wants the tile inset (824 px body,
    Apple's icon grid) with room for its shadow; Windows, Linux and the
    tray use the whole canvas. simple drops fine detail for tiny sizes."""
    if full_bleed:
        x0, size, radius = 0, 1024, 224
    else:
        x0, size, radius = 100, 824, 185
    gap = size * (0.1 if simple else 0.075)
    pad = (size - 3 * gap) / 2
    pr = pad * 0.16
    cells = []
    for row in range(2):
        for col in range(2):
            x = x0 + gap + col * (pad + gap)
            y = x0 + gap + row * (pad + gap)
            lit = (row, col) == (0, 1)
            fill = "url(#led)" if lit else "url(#pad)"
            cells.append(f'<rect x="{x:.1f}" y="{y:.1f}" width="{pad:.1f}" height="{pad:.1f}" rx="{pr:.1f}" fill="{fill}"/>')
            if not lit and not simple:
                # top edge highlight, like the pads in the app
                cells.append(f'<rect x="{x+pr:.1f}" y="{y+2:.1f}" width="{pad-2*pr:.1f}" height="{size*0.006:.1f}" rx="2" fill="#ffffff" opacity=".07"/>')
            if lit:
                cx, cy = x + pad / 2, y + pad / 2
                glow = f'<circle cx="{cx:.1f}" cy="{cy:.1f}" r="{pad*0.95:.1f}" fill="url(#glow)"/>'
                cells.insert(0, glow)
    shadow = "" if full_bleed else '<rect x="100" y="118" width="824" height="824" rx="185" fill="#000" opacity=".35" filter="url(#blur)"/>'
    return f'''<svg xmlns="http://www.w3.org/2000/svg" width="1024" height="1024" viewBox="0 0 1024 1024">
<defs>
  <linearGradient id="body" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="{METAL_3}"/><stop offset="1" stop-color="{METAL_0}"/></linearGradient>
  <linearGradient id="pad" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="{PAD_TINY if simple else PAD_TOP}"/><stop offset="1" stop-color="{PAD_TINY if simple else PAD_BOTTOM}"/></linearGradient>
  <radialGradient id="led" cx=".5" cy=".42" r=".7"><stop offset="0" stop-color="{LED_HOT}"/><stop offset=".55" stop-color="{LED}"/><stop offset="1" stop-color="#e8952a"/></radialGradient>
  <radialGradient id="glow"><stop offset="0" stop-color="{LED}" stop-opacity=".45"/><stop offset="1" stop-color="{LED}" stop-opacity="0"/></radialGradient>
  <clipPath id="tile"><rect x="{x0}" y="{x0}" width="{size}" height="{size}" rx="{radius}"/></clipPath>
  <filter id="blur" x="-10%" y="-10%" width="120%" height="130%"><feGaussianBlur stdDeviation="18"/></filter>
</defs>
{shadow}
<rect x="{x0}" y="{x0}" width="{size}" height="{size}" rx="{radius}" fill="url(#body)"/>
<g clip-path="url(#tile)">{''.join(cells)}</g>
<rect x="{x0+3}" y="{x0+3}" width="{size-6}" height="{size-6}" rx="{radius-3}" fill="none" stroke="#ffffff" stroke-opacity="{'.22' if simple else '.10'}" stroke-width="{24 if simple else 6}"/>
</svg>'''

def render(svg_text, px, out):
    with tempfile.NamedTemporaryFile("w", suffix=".svg", delete=False) as f:
        f.write(svg_text)
    subprocess.run(["rsvg-convert", "-w", str(px), "-h", str(px), "-o", out, f.name], check=True)
    os.unlink(f.name)

def png(svg_text, px):
    with tempfile.TemporaryDirectory() as d:
        p = os.path.join(d, "i.png")
        render(svg_text, px, p)
        return Image.open(p).convert("RGBA").copy()

def main():
    tmp = tempfile.mkdtemp()
    full, mac, tiny = svg(True), svg(False), svg(True, simple=True)
    # Sources kept in the repo for reference and for the README/website.
    open(os.path.join(HERE, "appicon.svg"), "w").write(full)
    open(os.path.join(HERE, "appicon-macos.svg"), "w").write(mac)
    render(full, 1024, os.path.join(HERE, "appicon.png"))

    # Windows .ico (exe and tray): tiny sizes from the simplified drawing.
    ico_sizes = [16, 20, 24, 32, 40, 48, 64, 128, 256]
    imgs = [png(tiny if n <= 32 else full, n) for n in ico_sizes]
    ico = os.path.join(ROOT, "internal", "tray", "icon.ico")
    imgs[-1].save(ico, format="ICO", sizes=[(n, n) for n in ico_sizes], append_images=imgs[:-1])
    win = os.path.join(HERE, "icon.ico")
    imgs[-1].save(win, format="ICO", sizes=[(n, n) for n in ico_sizes], append_images=imgs[:-1])

    # Linux window icon (embedded, set via Wails options).
    render(full, 256, os.path.join(ROOT, "build", "linux", "icon.png"))

    # macOS .icns via iconutil.
    iconset = os.path.join(tmp, "icon.iconset")
    os.makedirs(iconset)
    for n in [16, 32, 128, 256, 512]:
        render(mac, n, os.path.join(iconset, f"icon_{n}x{n}.png"))
        render(mac, n * 2, os.path.join(iconset, f"icon_{n}x{n}@2x.png"))
    subprocess.run(["iconutil", "-c", "icns", iconset, "-o", os.path.join(ROOT, "build", "darwin", "icon.icns")], check=True)
    print("icons written")

if __name__ == "__main__":
    main()
