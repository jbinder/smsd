package tray

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

func decodeIcon(t *testing.T, raw []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decoding icon: %v", err)
	}
	if got := img.Bounds().Size(); got.X != iconSize || got.Y != iconSize {
		t.Fatalf("icon is %v, want %d×%d", got, iconSize, iconSize)
	}
	return img
}

// Every state must have its own icon; a missing entry would silently leave the
// tray showing the previous state's icon.
func TestBuildIconsCoversEveryState(t *testing.T) {
	icons := buildIcons()
	seen := map[string]State{}
	for _, state := range []State{StateDisconnected, StateConnected, StateUnread, StateError} {
		raw, ok := icons[state]
		if !ok {
			t.Fatalf("no icon for state %d", state)
		}
		decodeIcon(t, raw)
		if other, dup := seen[string(raw)]; dup {
			t.Errorf("states %d and %d render the same icon", other, state)
		}
		seen[string(raw)] = state
	}
}

// The bubble must be recognisable: opaque in the middle, transparent in the
// corners, and reaching into the lower left where the tail hangs.
func TestBubbleShape(t *testing.T) {
	img := decodeIcon(t, buildIcons()[StateConnected])
	alphaAt := func(x, y int) uint32 {
		_, _, _, a := img.At(x, y).RGBA()
		return a >> 8
	}

	if a := alphaAt(3, 8); a != 0xff {
		t.Errorf("bubble body at (3,8) has alpha %#x, want opaque", a)
	}
	for _, p := range [][2]int{{0, 0}, {iconSize - 1, 0}, {iconSize - 1, iconSize - 1}} {
		if a := alphaAt(p[0], p[1]); a != 0 {
			t.Errorf("corner %v has alpha %#x, want transparent", p, a)
		}
	}
	if a := alphaAt(6, 16); a == 0 {
		t.Error("tail is missing below the bubble body")
	}
	// The three dots are knocked out of the filled bubble.
	if a := alphaAt(11, 8); a != 0 {
		t.Errorf("centre dot has alpha %#x, want knocked out", a)
	}
}

// The disconnected icon is drawn as an outline, so its centre column is hollow
// between the top edge and the dots.
func TestDisconnectedIconIsHollow(t *testing.T) {
	img := decodeIcon(t, buildIcons()[StateDisconnected])
	_, _, _, a := img.At(11, 4).RGBA()
	if a>>8 != 0 {
		t.Errorf("alpha at (11,4) is %#x, want a hollow interior", a>>8)
	}
	_, _, _, edge := img.At(11, 2).RGBA()
	if edge>>8 == 0 {
		t.Error("top edge of the outline is missing")
	}
}

func TestSdConvexIgnoresWinding(t *testing.T) {
	cw := [][2]float64{{0, 0}, {10, 0}, {10, 10}}
	ccw := [][2]float64{{10, 10}, {10, 0}, {0, 0}}
	for _, pts := range [][][2]float64{cw, ccw} {
		if d := sdConvex(8, 5, pts); d >= 0 {
			t.Errorf("interior point reported outside (d=%v) for %v", d, pts)
		}
		if d := sdConvex(0, 9, pts); d <= 0 {
			t.Errorf("exterior point reported inside (d=%v) for %v", d, pts)
		}
	}
}
