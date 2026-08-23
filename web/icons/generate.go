//go:build ignore

// Generates the Pail icon set from design 6b, "stacked slabs".
//
// Run with: go run web/icons/generate.go
//
// There is no SVG rasteriser in this toolchain and adding one as a dependency
// to draw four rounded rectangles would be absurd, so the mark is rendered
// directly. The output is committed; this exists so the icons can be
// regenerated rather than hand-edited, and so the geometry lives somewhere
// readable instead of only inside a binary PNG.
//
// The design specifies different geometry at small sizes: below 48px the bars
// widen and their gaps open up, because at 16px bars drawn to the large-size
// proportions merge into a single block. That is the whole reason 6b was
// chosen over the other directions, so it is reproduced here rather than
// scaling one drawing down and losing it.
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

// The palette, from the design's own swatches.
var (
	ink         = color.NRGBA{0x12, 0x21, 0x1d, 0xff} // #12211D
	accent      = color.NRGBA{0x2f, 0x9e, 0x78, 0xff} // #2F9E78
	accentLight = color.NRGBA{0x7f, 0xd3, 0xb1, 0xff} // #7FD3B1
)

// rect is a rounded rectangle in the 64-unit design grid.
type rect struct {
	x, y, w, h, r float64
	fill          color.NRGBA
	alpha         float64
}

// mark is one complete drawing: a rounded background plus its bars.
type mark struct {
	radius float64 // corner radius of the background, in grid units
	bg     color.NRGBA
	bars   []rect
}

// grid is the design's viewBox: everything is expressed in 64ths.
const grid = 64.0

// barsLarge is the geometry used at 48px and above.
func barsLarge(top, mid, bottom color.NRGBA, bottomAlpha float64) []rect {
	return []rect{
		{14, 17, 36, 9, 4.5, top, 1},
		{14, 29.5, 36, 9, 4.5, mid, 1},
		{14, 42, 22, 9, 4.5, bottom, bottomAlpha},
	}
}

// barsSmall is the 32px geometry: slightly taller bars, wider gaps, and a
// third bar a shade longer so it still reads as deliberate rather than as a
// rendering artefact.
func barsSmall() []rect {
	return []rect{
		{13, 17, 38, 9.5, 4.75, accentLight, 1},
		{13, 30, 38, 9.5, 4.75, accent, 1},
		{13, 43, 23, 9.5, 4.75, accent, 0.5},
	}
}

// barsTiny is the 16px geometry, where the bars are at their thickest and the
// gaps at their widest. Below this the mark stops being a mark.
func barsTiny() []rect {
	return []rect{
		{13, 17, 38, 10, 5, accentLight, 1},
		{13, 31, 38, 10, 5, accent, 1},
		{13, 45, 22, 10, 5, accent, 0.5},
	}
}

func main() {
	outDir := filepath.Join("web", "public")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fail(err)
	}

	// Favicons. Each size gets the geometry drawn for it, not a resample of a
	// larger one.
	writePNG(outDir, "favicon-16.png", 16, mark{14, ink, barsTiny()})
	writePNG(outDir, "favicon-32.png", 32, mark{15, ink, barsSmall()})

	// Home-screen and PWA icons. The design's app-icon panel uses a slightly
	// tighter corner than the favicon, which is what iOS and Android expect
	// under their own rounding.
	large := barsLarge(accentLight, accent, accent, 0.45)
	writePNG(outDir, "apple-touch-icon.png", 180, mark{14, ink, large})
	writePNG(outDir, "icon-192.png", 192, mark{14, ink, large})
	writePNG(outDir, "icon-512.png", 512, mark{14, ink, large})

	// Maskable: the platform applies its own shape, so the background must run
	// edge to edge — a rounded icon inside a circle crop shows pale corners.
	// The bars already sit inside the middle 56% of the grid, comfortably
	// within the 80% safe zone a circle crop leaves.
	writePNG(outDir, "icon-512-maskable.png", 512, mark{0, ink, large})

	writeSVG(outDir)
	fmt.Println("wrote the icon set to", outDir)
}

// writePNG renders one mark at one pixel size.
func writePNG(dir, name string, size int, m mark) {
	const samples = 4 // per axis, so 16 coverage samples per pixel
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	scale := grid / float64(size)

	background := rect{0, 0, grid, grid, m.radius, m.bg, 1}

	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var acc [4]float64 // straight (non-premultiplied) accumulation

			for sy := 0; sy < samples; sy++ {
				for sx := 0; sx < samples; sx++ {
					// Sample at subpixel centres.
					gx := (float64(px) + (float64(sx)+0.5)/samples) * scale
					gy := (float64(py) + (float64(sy)+0.5)/samples) * scale

					var r, g, b, a float64
					if background.contains(gx, gy) {
						r, g, b, a = componentsOf(background)
					}
					for _, bar := range m.bars {
						if !bar.contains(gx, gy) {
							continue
						}
						br, bg2, bb, ba := componentsOf(bar)
						// Source-over, in straight alpha.
						outA := ba + a*(1-ba)
						if outA == 0 {
							continue
						}
						r = (br*ba + r*a*(1-ba)) / outA
						g = (bg2*ba + g*a*(1-ba)) / outA
						b = (bb*ba + b*a*(1-ba)) / outA
						a = outA
					}

					acc[0] += r * a
					acc[1] += g * a
					acc[2] += b * a
					acc[3] += a
				}
			}

			total := float64(samples * samples)
			alpha := acc[3] / total
			if alpha <= 0 {
				continue
			}
			// acc holds premultiplied sums; divide the colour back out.
			img.SetNRGBA(px, py, color.NRGBA{
				R: clamp8(acc[0] / total / alpha),
				G: clamp8(acc[1] / total / alpha),
				B: clamp8(acc[2] / total / alpha),
				A: clamp8(alpha),
			})
		}
	}

	file, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		fail(err)
	}
	defer file.Close()
	if err := png.Encode(file, img); err != nil {
		fail(err)
	}
}

// contains reports whether a point in grid units falls inside the rounded
// rectangle. Corners are treated as quarter circles of radius r.
func (rc rect) contains(px, py float64) bool {
	if px < rc.x || px > rc.x+rc.w || py < rc.y || py > rc.y+rc.h {
		return false
	}
	r := math.Min(rc.r, math.Min(rc.w, rc.h)/2)
	if r <= 0 {
		return true
	}
	// Distance outside the inner rectangle that the corner arcs are drawn
	// around; zero on both axes means the point is in the straight middle.
	dx := math.Max(0, math.Max(rc.x+r-px, px-(rc.x+rc.w-r)))
	dy := math.Max(0, math.Max(rc.y+r-py, py-(rc.y+rc.h-r)))
	return dx*dx+dy*dy <= r*r
}

func componentsOf(rc rect) (r, g, b, a float64) {
	return float64(rc.fill.R) / 255, float64(rc.fill.G) / 255, float64(rc.fill.B) / 255,
		float64(rc.fill.A) / 255 * rc.alpha
}

func clamp8(v float64) uint8 {
	scaled := math.Round(v * 255)
	switch {
	case scaled < 0:
		return 0
	case scaled > 255:
		return 255
	default:
		return uint8(scaled)
	}
}

// writeSVG emits the scalable favicon. Browsers that take an SVG icon render
// it at whatever size they please, so this carries the large geometry.
func writeSVG(dir string) {
	const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" width="64" height="64">
  <rect width="64" height="64" rx="16" fill="#12211D"/>
  <rect x="14" y="17" width="36" height="9" rx="4.5" fill="#7FD3B1"/>
  <rect x="14" y="29.5" width="36" height="9" rx="4.5" fill="#2F9E78"/>
  <rect x="14" y="42" width="22" height="9" rx="4.5" fill="#2F9E78" opacity=".45"/>
</svg>
`
	if err := os.WriteFile(filepath.Join(dir, "favicon.svg"), []byte(svg), 0o644); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "icon generation failed:", err)
	os.Exit(1)
}
