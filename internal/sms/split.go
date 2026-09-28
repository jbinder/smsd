package sms

import (
	"strings"
	"unicode/utf16"
)

// gsmBasic is the GSM 03.38 default alphabet: each of these costs one septet.
const gsmBasic = "@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?" +
	"¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà"

// gsmExtension characters are reached through an escape and cost two septets.
const gsmExtension = "\f^{}\\[~]|€"

var gsmCost = func() map[rune]int {
	m := map[rune]int{}
	for _, r := range gsmBasic {
		m[r] = 1
	}
	for _, r := range gsmExtension {
		m[r] = 2
	}
	return m
}()

// Split divides text into the parts it is sent as. A text that fits the GSM
// 7-bit alphabet travels in 160 septets, or 153 per part once it needs several
// (the rest is the concatenation header). Anything else is sent as UCS-2: 70
// UTF-16 units, or 67 per part. Parts never split a character, so an escaped
// GSM character or a surrogate pair stays whole. The phone reassembles the
// parts into one message.
func Split(text string) []string {
	if text == "" {
		return nil
	}
	cost, single, multi := gsmCost, 160, 153
	for _, r := range text {
		if gsmCost[r] == 0 {
			cost, single, multi = nil, 70, 67
			break
		}
	}
	size := func(r rune) int {
		if cost != nil {
			return cost[r]
		}
		return len(utf16.Encode([]rune{r}))
	}

	total := 0
	for _, r := range text {
		total += size(r)
	}
	if total <= single {
		return []string{text}
	}

	var parts []string
	start, used := 0, 0
	for i, r := range text {
		n := size(r)
		if used+n > multi {
			parts = append(parts, text[start:i])
			start, used = i, 0
		}
		used += n
	}
	return append(parts, text[start:])
}

// Dialable strips the formatting from a phone number — "+44 7843 174004"
// becomes "+447843174004" — the form the network delivers incoming messages
// from. Android files a sent message under the address exactly as given, so
// sending to the formatted form would start a separate conversation. Anything
// that is not a formatted number, such as a sender name, is returned as is.
func Dialable(address string) string {
	address = strings.TrimSpace(address)
	var b strings.Builder
	for i, r := range address {
		switch {
		case r >= '0' && r <= '9', r == '+' && i == 0:
			b.WriteRune(r)
		case strings.ContainsRune(" -()./\u00a0", r):
		default:
			return address
		}
	}
	return b.String()
}
