package titlenorm

import "unicode"

// Japanese-narrow script detectors. These live with the title normalizer
// (moved from classifier/contentfilter) so the script predicates stay in
// one place without an import cycle: contentfilter now depends on this
// package through the language gate.

// IsJapaneseScriptChar reports whether r belongs to a script used to write
// Japanese: Han (kanji), Hiragana, or Katakana. Han is shared with Chinese,
// so a Han character alone is "CJK", not exclusively Japanese; callers use
// this as a cheap "native East-Asian title" signal, not a language ID.
func IsJapaneseScriptChar(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r)
}

// IsKanaChar reports whether r is a Hiragana or Katakana character (excludes
// Han). Kana romanises deterministically to Hepburn; kanji does not.
func IsKanaChar(r rune) bool {
	return unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r)
}

// ContainsJapaneseScript reports whether s contains at least one Han,
// Hiragana, or Katakana character.
func ContainsJapaneseScript(s string) bool {
	for _, r := range s {
		if IsJapaneseScriptChar(r) {
			return true
		}
	}
	return false
}

// ContainsKana reports whether s contains at least one Hiragana or Katakana
// character.
func ContainsKana(s string) bool {
	for _, r := range s {
		if IsKanaChar(r) {
			return true
		}
	}
	return false
}
