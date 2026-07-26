package tray

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
)

// buildIcons renders one PNG per state. Icons are simple filled discs in a
// state colour, drawn at build-of-process time so no image assets are shipped.
func buildIcons() map[State][]byte {
	return map[State][]byte{
		StateDisconnected: disc(color.NRGBA{0x9e, 0x9e, 0x9e, 0xff}), // grey
		StateConnected:    disc(color.NRGBA{0x4c, 0xaf, 0x50, 0xff}), // green
		StateUnread:       disc(color.NRGBA{0x21, 0x96, 0xf3, 0xff}), // blue
		StateError:        disc(color.NRGBA{0xf4, 0x43, 0x36, 0xff}), // red
	}
}

// disc returns a PNG of a size×size anti-aliased-ish filled circle in c.
func disc(c color.NRGBA) []byte {
	const size = 22
	const r = size / 2
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	cx, cy := float64(size)/2, float64(size)/2
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			dist := dx*dx + dy*dy
			switch {
			case dist <= float64((r-1)*(r-1)):
				img.Set(x, y, c)
			case dist <= float64(r*r):
				// Soft edge: half-alpha ring for a less jagged outline.
				edge := c
				edge.A = 0x88
				img.Set(x, y, edge)
			default:
				img.Set(x, y, color.NRGBA{0, 0, 0, 0})
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
