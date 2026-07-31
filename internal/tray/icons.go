package tray

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Icons are drawn at process start rather than shipped as assets, so the binary
// stays a single self-contained file. Each state renders the same SMS speech
// bubble in a different colour: filled once a device is talking to us, outlined
// while there is none, and carrying an exclamation mark on error.
//
// Shapes are built from signed distance fields and antialiased by converting
// distance directly to coverage, which keeps the small curves clean without
// pulling in a rasteriser.

// iconSize is the conventional tray icon edge length in pixels. All geometry
// below is expressed in this coordinate space, with y pointing down.
const iconSize = 22

// glyph is the mark drawn inside the bubble.
type glyph int

const (
	glyphDots glyph = iota // three dots — a message
	glyphBang              // exclamation mark — something went wrong
)

// buildIcons renders one PNG per state.
func buildIcons() map[State][]byte {
	return map[State][]byte{
		StateDisconnected: bubble(color.NRGBA{0x9e, 0x9e, 0x9e, 0xff}, glyphDots, false), // grey, hollow
		StateConnected:    bubble(color.NRGBA{0x4c, 0xaf, 0x50, 0xff}, glyphDots, true),  // green
		StateUnread:       bubble(color.NRGBA{0x21, 0x96, 0xf3, 0xff}, glyphDots, true),  // blue
		StateError:        bubble(color.NRGBA{0xf4, 0x43, 0x36, 0xff}, glyphBang, true),  // red
	}
}

// Bubble geometry: a rounded rectangle with a tail hanging off the lower left.
// The tail's top edge sits inside the body so the two merge into one outline.
var (
	bodyCenter = [2]float64{11, 8}
	bodyHalf   = [2]float64{9.5, 6}
	bodyRadius = 3.5
	tail       = [][2]float64{{6.0, 11.0}, {11.5, 12.5}, {4.5, 19.0}}
)

// strokeWidth is the outline thickness used by the hollow (disconnected) icon.
const strokeWidth = 2.0

// bubble renders one icon. When filled, the glyph is knocked out of the body so
// the desktop background shows through it; when hollow, only the outline is
// drawn and the glyph is painted inside it.
func bubble(c color.NRGBA, g glyph, filled bool) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, iconSize, iconSize))
	for y := 0; y < iconSize; y++ {
		for x := 0; x < iconSize; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5
			body := coverage(sdBubble(px, py))
			mark := coverage(sdGlyph(px, py, g))

			var alpha float64
			if filled {
				alpha = body * (1 - mark)
			} else {
				// abs(d) turns the body's edge into a centred stroke.
				alpha = math.Max(coverage(math.Abs(sdBubble(px, py))-strokeWidth/2), mark)
			}
			if alpha <= 0 {
				continue
			}
			out := c
			out.A = uint8(math.Round(float64(c.A) * alpha))
			img.SetNRGBA(x, y, out)
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// coverage maps a signed distance to a 0..1 pixel coverage, giving a one-pixel
// antialiased band across the shape's edge.
func coverage(d float64) float64 {
	return math.Min(1, math.Max(0, 0.5-d))
}

// sdBubble is the distance to the bubble outline: the union (minimum) of the
// rounded body and the tail.
func sdBubble(px, py float64) float64 {
	return math.Min(
		sdRoundRect(px, py, bodyCenter, bodyHalf, bodyRadius),
		sdConvex(px, py, tail),
	)
}

func sdGlyph(px, py float64, g glyph) float64 {
	if g == glyphBang {
		// A stem over a dot, both centred on the bubble body.
		stem := sdRoundRect(px, py, [2]float64{11, 6.6}, [2]float64{0.95, 2.6}, 0.95)
		dot := math.Hypot(px-11, py-11.6) - 1.05
		return math.Min(stem, dot)
	}
	d := math.Inf(1)
	for _, cx := range []float64{6.8, 11.0, 15.2} {
		d = math.Min(d, math.Hypot(px-cx, py-8.0)-1.55)
	}
	return d
}

// sdRoundRect returns the exact signed distance to a rounded rectangle.
func sdRoundRect(px, py float64, center, half [2]float64, r float64) float64 {
	qx := math.Abs(px-center[0]) - (half[0] - r)
	qy := math.Abs(py-center[1]) - (half[1] - r)
	return math.Min(math.Max(qx, qy), 0) + math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) - r
}

// sdConvex returns the distance to a convex polygon as the maximum over its
// edge half-planes, negative inside. Winding is detected from the signed area,
// so the caller need not order the points. Corners are very slightly rounded
// off from the outside, which is invisible at this size.
func sdConvex(px, py float64, pts [][2]float64) float64 {
	var area2 float64
	for i, a := range pts {
		b := pts[(i+1)%len(pts)]
		area2 += a[0]*b[1] - b[0]*a[1]
	}
	flip := 1.0
	if area2 < 0 {
		flip = -1
	}

	d := math.Inf(-1)
	for i, a := range pts {
		b := pts[(i+1)%len(pts)]
		ex, ey := b[0]-a[0], b[1]-a[1]
		l := math.Hypot(ex, ey)
		nx, ny := flip*ey/l, -flip*ex/l // outward edge normal
		d = math.Max(d, (px-a[0])*nx+(py-a[1])*ny)
	}
	return d
}
