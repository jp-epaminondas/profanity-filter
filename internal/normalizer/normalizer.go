// Package normalizer transforms raw user text into a canonical rune stream
// that can be matched against the profanity Trie.
//
// The transformation is designed to defeat the most common evasion tricks
// used against word filters in Brazilian Portuguese:
//
//	"M3RD4"        -> "merda"   (case folding + leetspeak)
//	"p.u.t.a"      -> "puta"    (separator characters are stripped)
//	"desgraçado"   -> "desgracado" (diacritics are removed)
//
// Crucially, normalization is *position preserving*: every rune in the output
// remembers the byte span it came from in the original text. That is what
// allows the filter to censor the ORIGINAL text (keeping accents, casing and
// punctuation of the clean parts) while matching against the normalized one.
package normalizer

import (
	"unicode"
	"unicode/utf8"
)

// Span is a half-open byte interval [Start, End) in the original text.
type Span struct {
	Start int
	End   int
}

// Buffer holds the result of a normalization pass.
//
// The three slices are parallel: Runes[i] came from Spans[i] in the original
// text, and SoftBreak[i] reports whether separator characters (".", "*", "-",
// "!" ...) were removed right before Runes[i].
//
// A Buffer is meant to be reused across calls (see Reset) so that the hot
// path performs no heap allocations once the slices have grown.
type Buffer struct {
	Runes     []rune
	Spans     []Span
	SoftBreak []bool
}

// Reset empties the buffer while keeping the allocated capacity.
func (b *Buffer) Reset() {
	b.Runes = b.Runes[:0]
	b.Spans = b.Spans[:0]
	b.SoftBreak = b.SoftBreak[:0]
}

// Len returns the number of normalized runes.
func (b *Buffer) Len() int { return len(b.Runes) }

// IsBoundary reports whether position i (between rune i-1 and rune i) is a
// word boundary. Boundaries are the edges of the text, whitespace, and the
// places where separator characters were stripped.
//
// Matching only at boundaries is what prevents the classic "Scunthorpe
// problem": "computador" must not be flagged because it contains "puta".
func (b *Buffer) IsBoundary(i int) bool {
	n := len(b.Runes)
	if i <= 0 || i >= n {
		return true
	}
	return b.Runes[i] == ' ' || b.Runes[i-1] == ' ' || b.SoftBreak[i]
}

func (b *Buffer) push(r rune, start, end int, softBreak bool) {
	b.Runes = append(b.Runes, r)
	b.Spans = append(b.Spans, Span{Start: start, End: end})
	b.SoftBreak = append(b.SoftBreak, softBreak)
}

// Normalize writes the canonical form of text into buf (which is reset first).
//
// Rules, applied rune by rune in a single O(n) pass:
//  1. Leetspeak symbols are mapped to letters (0->o, 4->a, 3->e, 1->i, @->a ...).
//  2. Letters are lowercased and stripped of diacritics (Ç->c, Ã->a ...).
//     Cyrillic and Greek homoglyphs fold into the Latin letter they imitate
//     (Cyrillic а->a, Greek Ρ->p ...: see homoglyphUpper and homoglyphLower).
//  3. Runs of whitespace collapse into a single ' ' (hard word boundary).
//  4. Every other symbol/punctuation is removed, leaving a soft boundary.
//  5. Combining marks (decomposed accents) are dropped without a trace.
func Normalize(text string, buf *Buffer) {
	buf.Reset()

	pendingBreak := false // a separator was skipped since the last emitted rune
	lastWasSpace := true  // true at start so leading whitespace is ignored

	for i := 0; i < len(text); {
		var (
			r    rune
			size int
		)
		// ASCII fast path: the vast majority of chat text is ASCII.
		if c := text[i]; c < utf8.RuneSelf {
			r, size = rune(c), 1
		} else {
			r, size = utf8.DecodeRuneInString(text[i:])
		}

		out, k := classify(r)
		switch k {
		case kindLetter:
			buf.push(out, i, i+size, pendingBreak && !lastWasSpace)
			pendingBreak = false
			lastWasSpace = false
		case kindSpace:
			if !lastWasSpace {
				buf.push(' ', i, i+size, false)
				lastWasSpace = true
			}
			pendingBreak = false
		case kindSeparator:
			pendingBreak = true
		case kindDrop:
			// Nothing: e.g. U+0301 in a decomposed "é" simply disappears.
		}
		i += size
	}

	// Trim a trailing space so phrase matching never depends on it.
	if n := len(buf.Runes); n > 0 && buf.Runes[n-1] == ' ' {
		buf.Runes = buf.Runes[:n-1]
		buf.Spans = buf.Spans[:n-1]
		buf.SoftBreak = buf.SoftBreak[:n-1]
	}
}

// String is a convenience wrapper that returns only the normalized text.
// It allocates, so it is meant for setup code (building the Trie) and tests,
// not for the request hot path.
func String(s string) string {
	var buf Buffer
	Normalize(s, &buf)
	return string(buf.Runes)
}

// ---------------------------------------------------------------------------
// Character classification
// ---------------------------------------------------------------------------

type kind uint8

const (
	kindSeparator kind = iota // removed, but marks a possible word boundary
	kindLetter                // kept (after folding)
	kindSpace                 // collapsed into a single ' '
	kindDrop                  // removed without leaving a boundary
)

type entry struct {
	r rune
	k kind
}

// tableSize covers ASCII, Latin-1 Supplement and Latin Extended-A, i.e. every
// character realistically typed in Brazilian Portuguese. Lookups for these
// runes are a single array index: no maps, no branches on unicode tables.
const tableSize = 0x180

var table = buildTable()

// leet maps leetspeak symbols to the letter they usually stand for.
var leet = map[rune]rune{
	'0': 'o',
	'1': 'i',
	'3': 'e',
	'4': 'a',
	'5': 's',
	'7': 't',
	'@': 'a',
	'$': 's',
}

// accents lists, for each base letter, the accented forms folded into it.
var accents = map[rune]string{
	'a': "àáâãäåā",
	'c': "çćĉċč",
	'e': "èéêëēėę",
	'i': "ìíîïīį",
	'n': "ñń",
	'o': "òóôõöøō",
	'u': "ùúûüūů",
	'y': "ýÿ",
}

func buildTable() [tableSize]entry {
	fold := make(map[rune]rune)
	for base, forms := range accents {
		for _, f := range forms {
			fold[f] = base
		}
	}

	var t [tableSize]entry
	for r := rune(0); r < tableSize; r++ {
		lower := unicode.ToLower(r)
		switch {
		case leet[r] != 0:
			t[r] = entry{leet[r], kindLetter}
		case fold[lower] != 0:
			t[r] = entry{fold[lower], kindLetter}
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			t[r] = entry{lower, kindLetter}
		case unicode.IsSpace(r):
			t[r] = entry{' ', kindSpace}
		default:
			t[r] = entry{0, kindSeparator}
		}
	}
	return t
}

// classify returns the folded rune and its kind.
func classify(r rune) (rune, kind) {
	if r >= 0 && r < tableSize {
		e := table[r]
		return e.r, e.k
	}
	// Slow path for everything outside Latin: still correct, just not tabled.
	if l, ok := homoglyphUpper[r]; ok {
		return l, kindLetter
	}
	switch {
	case unicode.Is(unicode.Mn, r):
		return 0, kindDrop
	case unicode.IsLetter(r) || unicode.IsDigit(r):
		lower := unicode.ToLower(r)
		if l, ok := homoglyphLower[lower]; ok {
			return l, kindLetter
		}
		return lower, kindLetter
	case unicode.IsSpace(r):
		return ' ', kindSpace
	default:
		return 0, kindSeparator
	}
}

// Homoglyphs: Cyrillic and Greek letters drawn exactly like Latin ones. Without
// this, "putа" written with a Cyrillic "а" (U+0430) slips past the filter while
// looking identical on screen. Each one folds into the Latin letter it imitates
// (in the spirit of the Unicode confusables, UTS #39).
//
// The uppercase table is checked first, because case folding loses the shape:
// Greek "Ν" is drawn as Latin "N", but its lowercase "ν" is drawn as "v". Only
// the lowercase forms that also look Latin go in the second table (Cyrillic "т"
// does not look like "t", but "Т" looks like "T").
//
// The same normalization runs over the word lists, so a future dictionary in
// Russian or Greek still matches: both sides are folded the same way.
var homoglyphUpper = map[rune]rune{
	// Cyrillic
	'А': 'a', 'В': 'b', 'Е': 'e', 'К': 'k', 'М': 'm', 'Н': 'h', 'О': 'o', 'Р': 'p',
	'С': 'c', 'Т': 't', 'У': 'y', 'Х': 'x', 'Ѕ': 's', 'І': 'i', 'Ј': 'j',
	// Greek
	'Α': 'a', 'Β': 'b', 'Ε': 'e', 'Ζ': 'z', 'Η': 'h', 'Ι': 'i', 'Κ': 'k', 'Μ': 'm',
	'Ν': 'n', 'Ο': 'o', 'Ρ': 'p', 'Τ': 't', 'Υ': 'y', 'Χ': 'x',
}

var homoglyphLower = map[rune]rune{
	// Cyrillic
	'а': 'a', 'е': 'e', 'ё': 'e', 'о': 'o', 'р': 'p', 'с': 'c', 'у': 'y', 'х': 'x',
	'ѕ': 's', 'і': 'i', 'ї': 'i', 'ј': 'j', 'ԁ': 'd', 'ԛ': 'q', 'ԝ': 'w', 'ӏ': 'l', 'к': 'k',
	// Greek
	'α': 'a', 'ο': 'o', 'ρ': 'p', 'ν': 'v', 'ι': 'i', 'κ': 'k', 'υ': 'u', 'χ': 'x',
	'ε': 'e', 'γ': 'y', 'ω': 'w',
}
