//go:build ignore

// Command gen_icons draws the PWA icons into assets/ with the standard library only
// (docs/15). Run it with "go generate ./internal/ui".
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
)

// The colors are the console direction's --bg, --info, --success and --warn (style.css).
var (
	background = color.NRGBA{0x11, 0x12, 0x15, 0xff}
	barColors  = []color.NRGBA{
		{0x7a, 0xb4, 0xf5, 0xff},
		{0x69, 0xcc, 0x97, 0xff},
		{0xe3, 0xb4, 0x60, 0xff},
	}
)

// samples is the supersampling factor per axis used for anti-aliasing.
const samples = 4

type icon struct {
	path string
	size int
	// corner is the radius of the transparent rounded corners as a fraction of the size.
	// Maskable and Apple touch icons are full-bleed: the platform cuts its own shape.
	corner float64
	// mark is the width and height of the three bars as a fraction of the size. A maskable
	// icon keeps them inside the central circle of radius 40% (mark below 0.8/sqrt 2).
	mark float64
}

// rect is a rounded rectangle in unit coordinates.
type rect struct {
	x0, y0, x1, y1, radius float64
}

func (r rect) contains(x, y float64) bool {
	if x < r.x0 || x > r.x1 || y < r.y0 || y > r.y1 {
		return false
	}
	dx := math.Max(math.Max(r.x0+r.radius-x, x-(r.x1-r.radius)), 0)
	dy := math.Max(math.Max(r.y0+r.radius-y, y-(r.y1-r.radius)), 0)
	return dx*dx+dy*dy <= r.radius*r.radius
}

// bars returns three quota gauges of different levels standing on a common baseline.
func bars(mark float64) []rect {
	left := (1 - mark) / 2
	bottom := left + mark
	width := mark * 0.26
	gap := (mark - 3*width) / 2
	out := make([]rect, 0, len(barColors))
	for i, level := range []float64{0.62, 1.0, 0.44} {
		x := left + float64(i)*(width+gap)
		out = append(out, rect{x, bottom - mark*level, x + width, bottom, width * 0.3})
	}
	return out
}

func draw(ic icon) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, ic.size, ic.size))
	plate := rect{0, 0, 1, 1, ic.corner}
	marks := bars(ic.mark)
	for py := 0; py < ic.size; py++ {
		for px := 0; px < ic.size; px++ {
			var r, g, b, covered int
			for sy := 0; sy < samples; sy++ {
				for sx := 0; sx < samples; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/samples) / float64(ic.size)
					y := (float64(py) + (float64(sy)+0.5)/samples) / float64(ic.size)
					if !plate.contains(x, y) {
						continue
					}
					c := background
					for i, m := range marks {
						if m.contains(x, y) {
							c = barColors[i]
						}
					}
					r, g, b, covered = r+int(c.R), g+int(c.G), b+int(c.B), covered+1
				}
			}
			if covered > 0 {
				img.SetNRGBA(px, py, color.NRGBA{
					uint8(r / covered), uint8(g / covered), uint8(b / covered),
					uint8(255 * covered / (samples * samples)),
				})
			}
		}
	}
	return img
}

func main() {
	for _, ic := range []icon{
		{path: "assets/icon-192.png", size: 192, corner: 0.18, mark: 0.62},
		{path: "assets/icon-512.png", size: 512, corner: 0.18, mark: 0.62},
		{path: "assets/icon-maskable-512.png", size: 512, mark: 0.50},
		{path: "assets/apple-touch-icon.png", size: 180, mark: 0.58},
	} {
		f, err := os.Create(ic.path)
		if err != nil {
			log.Fatal(err)
		}
		encoder := png.Encoder{CompressionLevel: png.BestCompression}
		if err := encoder.Encode(f, draw(ic)); err != nil {
			log.Fatal(err)
		}
		if err := f.Close(); err != nil {
			log.Fatal(err)
		}
	}
}
