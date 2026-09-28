package sms

import (
	"strings"
	"testing"
)

func TestSplit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		sizes []int // length of each part in runes
	}{
		{"empty", "", nil},
		{"gsm single", strings.Repeat("a", 160), []int{160}},
		{"gsm multi", strings.Repeat("a", 161), []int{153, 8}},
		{"umlauts stay gsm", strings.Repeat("ä", 160), []int{160}},
		// Each € costs two septets: 80 fit one message, 81 do not.
		{"gsm extension", strings.Repeat("€", 80), []int{80}},
		{"gsm extension multi", strings.Repeat("€", 81), []int{76, 5}},
		{"ucs2 single", "ł" + strings.Repeat("a", 69), []int{70}},
		{"ucs2 multi", "ł" + strings.Repeat("a", 70), []int{67, 4}},
		// An emoji is two UTF-16 units and must not be cut in half.
		{"surrogate pair", strings.Repeat("a", 66) + "😀" + "bbb", []int{66, 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts := Split(tc.text)
			var sizes []int
			for _, p := range parts {
				sizes = append(sizes, len([]rune(p)))
			}
			if len(sizes) != len(tc.sizes) {
				t.Fatalf("parts = %v, want sizes %v", sizes, tc.sizes)
			}
			for i := range sizes {
				if sizes[i] != tc.sizes[i] {
					t.Fatalf("parts = %v, want sizes %v", sizes, tc.sizes)
				}
			}
			if strings.Join(parts, "") != tc.text {
				t.Errorf("parts do not reassemble the text")
			}
		})
	}
}

func TestDialable(t *testing.T) {
	for in, want := range map[string]string{
		"+44 7843 174004":  "+447843174004",
		" (0660) 123-45.6": "0660123456",
		"+436601234567":    "+436601234567",
		"PAYPAL":           "PAYPAL",
		"Raiffeisen":       "Raiffeisen",
		"12+34":            "12+34",
	} {
		if got := Dialable(in); got != want {
			t.Errorf("Dialable(%q) = %q, want %q", in, got, want)
		}
	}
}
