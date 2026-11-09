// Package filter detects and censors profanity by scanning normalized text
// against an in-memory Trie.
//
// Pipeline for every request:
//
//	original text ──Normalize──▶ rune stream + byte spans
//	                               │
//	                               ▼
//	                      Trie scan (word starts only)
//	                               │  matches as normalized ranges
//	                               ▼
//	               map back to ORIGINAL byte spans ──▶ replace with '*'
//
// The scan is O(n·L), where n is the text length and L the length of the
// longest dictionary word. The size of the dictionary does not matter: a
// list with 10 or 100,000 words costs the same per character scanned.
//
// Languages: every dictionary is stored in the SAME Trie, and each word
// carries a bitmask of the languages it belongs to. A request selects a
// LangSet, and a word only counts when its mask intersects that set. The
// text is therefore scanned exactly once, whether the request asks for one
// language or all of them.
package filter

import (
	"fmt"
	"math/bits"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/jp-epaminondas/language-filter/internal/normalizer"
	"github.com/jp-epaminondas/language-filter/internal/trie"
	"github.com/jp-epaminondas/language-filter/internal/wordlist"
)

// MaxLanguages is the number of languages a Filter can hold (one bit each).
const MaxLanguages = 64

// OptInSuffix marks a dictionary that is never applied by default, such as
// "pt-br-strict": it is only used when a request (or DEFAULT_LANGUAGES)
// names it explicitly.
const OptInSuffix = "-strict"

// LangSet is a set of languages, one bit per language.
type LangSet = trie.Tags

// Language describes a loaded dictionary.
type Language struct {
	Code  string `json:"code"`
	Words int    `json:"words"`
	OptIn bool   `json:"opt_in"` // excluded from Defaults
}

// Match is a detected word, as a half-open byte range of the ORIGINAL text,
// plus the dictionary word it matched and the languages that contain it.
type Match struct {
	Start int
	End   int
	Word  string // dictionary form, e.g. "porra" for "P0RRRR4"
	Langs LangSet
}

// Mask controls how detected words are replaced.
type Mask struct {
	// Char replaces each character of a detected word. Zero means '*'.
	Char rune
	// KeepFirst keeps the first character of each word: "p****".
	// In a phrase, every word keeps its first letter: "f**** d* p***".
	KeepFirst bool
}

// Result is the outcome of filtering a text.
type Result struct {
	HasProfanity bool
	Censored     string
	Matches      []Match
	Detected     LangSet // union of the languages of every match
}

// Filter is safe for concurrent use. After New returns, the Trie is
// read-only, so requests never contend on a lock.
type Filter struct {
	trie   *trie.Trie
	langs  []Language     // index i is bit i of a LangSet
	byCode map[string]int // language code -> index in langs

	// pool recycles normalization buffers between requests, so a warmed-up
	// server performs almost no allocations on the hot path and puts very
	// little pressure on the garbage collector.
	pool sync.Pool
}

// maxPooledRunes caps the buffers kept in the pool, so one huge request
// does not pin a large amount of memory forever.
const maxPooledRunes = 64 * 1024

// New builds a Filter from per-language dictionaries. Words go through the
// exact same normalization as user input, so "Desgraçado" in the list
// matches "d3sgr4c4d0" in a message.
func New(dicts []wordlist.Dictionary) (*Filter, error) {
	if len(dicts) > MaxLanguages {
		return nil, fmt.Errorf("too many languages: %d (max %d)", len(dicts), MaxLanguages)
	}

	f := &Filter{trie: trie.New(), byCode: make(map[string]int, len(dicts))}
	for _, d := range dicts {
		code := wordlist.NormalizeCode(d.Lang)
		if code == "" {
			return nil, fmt.Errorf("dictionary with empty language code")
		}
		if _, dup := f.byCode[code]; dup {
			return nil, fmt.Errorf("duplicate language %q", code)
		}
		idx := len(f.langs)
		bit := LangSet(1) << idx

		lang := Language{Code: code, OptIn: strings.HasSuffix(code, OptInSuffix)}
		for _, w := range d.Words {
			if f.trie.Insert([]rune(normalizer.String(w)), bit) {
				lang.Words++
			}
		}
		f.langs = append(f.langs, lang)
		f.byCode[code] = idx
	}
	f.pool.New = func() any { return new(normalizer.Buffer) }
	return f, nil
}

// Languages returns the loaded languages, in bit order.
func (f *Filter) Languages() []Language {
	return append([]Language(nil), f.langs...)
}

// All returns the set of every loaded language.
func (f *Filter) All() LangSet {
	if len(f.langs) == MaxLanguages {
		return ^LangSet(0)
	}
	return LangSet(1)<<len(f.langs) - 1
}

// Defaults returns every language except the opt-in ones. It is the set
// used when neither the request nor DEFAULT_LANGUAGES chooses. If every
// dictionary is opt-in, it falls back to All.
func (f *Filter) Defaults() LangSet {
	var set LangSet
	for i, l := range f.langs {
		if !l.OptIn {
			set |= LangSet(1) << i
		}
	}
	if set == 0 {
		return f.All()
	}
	return set
}

// Resolve converts language codes into a LangSet. Codes are case-insensitive
// ("PT-BR" == "pt-br"). An unknown code is an error that lists the valid ones.
func (f *Filter) Resolve(codes []string) (LangSet, error) {
	var set LangSet
	for _, c := range codes {
		idx, ok := f.byCode[wordlist.NormalizeCode(c)]
		if !ok {
			return 0, fmt.Errorf("idioma desconhecido %q (disponíveis: %s)",
				c, strings.Join(f.Codes(f.All()), ", "))
		}
		set |= LangSet(1) << idx
	}
	return set, nil
}

// Codes lists the language codes in set, sorted alphabetically.
func (f *Filter) Codes(set LangSet) []string {
	codes := make([]string, 0, bits.OnesCount64(uint64(set)))
	for set != 0 {
		idx := bits.TrailingZeros64(uint64(set))
		if idx < len(f.langs) {
			codes = append(codes, f.langs[idx].Code)
		}
		set &= set - 1 // clear the lowest bit
	}
	sort.Strings(codes)
	return codes
}

// Words returns how many distinct words the Trie holds across languages.
func (f *Filter) Words() int { return f.trie.Len() }

// Nodes returns how many nodes the underlying Trie has.
func (f *Filter) Nodes() int { return f.trie.Nodes() }

// Censor detects profanity of the given languages in text and returns it
// with every detected word replaced by asterisks (one '*' per character;
// whitespace inside a matched phrase is preserved).
func (f *Filter) Censor(text string, langs LangSet) Result {
	return f.CensorWith(text, langs, Mask{})
}

// CensorWith is Censor with a custom Mask. Every masked character becomes
// exactly one character, so the censored text has the same length in
// characters as the original and match offsets stay valid for both.
func (f *Filter) CensorWith(text string, langs LangSet, m Mask) Result {
	if langs == 0 {
		return Result{Censored: text}
	}

	buf := f.pool.Get().(*normalizer.Buffer)
	defer f.release(buf)

	normalizer.Normalize(text, buf)
	matches := f.scan(buf, langs)

	if len(matches) == 0 {
		// Fast path: clean text is returned as-is, with zero extra allocations.
		return Result{Censored: text}
	}
	var detected LangSet
	for _, m := range matches {
		detected |= m.Langs
	}
	return Result{
		HasProfanity: true,
		Censored:     mask(text, matches, m),
		Matches:      matches,
		Detected:     detected,
	}
}

func (f *Filter) release(buf *normalizer.Buffer) {
	if cap(buf.Runes) > maxPooledRunes {
		return // let the GC reclaim oversized buffers
	}
	f.pool.Put(buf)
}

// scan walks the normalized stream left to right, trying a Trie match at
// every word start. On a hit it jumps past the match (non-overlapping).
func (f *Filter) scan(buf *normalizer.Buffer, langs LangSet) []Match {
	var matches []Match
	n := buf.Len()
	for i := 0; i < n; {
		if buf.Runes[i] == ' ' || !buf.IsBoundary(i) {
			i++
			continue
		}
		if end, found := f.longestMatch(buf, i, langs); end > i {
			matches = append(matches, Match{
				Start: buf.Spans[i].Start,
				End:   buf.Spans[end-1].End,
				Word:  f.dictionaryWord(buf, i, end),
				Langs: found,
			})
			i = end
			continue
		}
		i++
	}
	return matches
}

// longestMatch returns the end (exclusive) of the longest word of the
// requested languages starting at normalized position start, or -1 if there
// is none, together with the languages that word belongs to.
//
// Language filtering costs a single AND per terminal node: a word that only
// exists in an unselected language is simply not a terminal for this request.
//
// A match is only accepted when it ends on a word boundary, so "puta" is
// found in "puta!" and "p.u.t.a" but not in "computador" or "putaria".
//
// The walk is also tolerant to stretched letters: when a rune has no edge in
// the Trie but repeats the previous one, the cursor stays where it is. That
// turns "merdaaaa" and "porrrra" into hits without storing every variation.
func (f *Filter) longestMatch(buf *normalizer.Buffer, start int, langs LangSet) (int, LangSet) {
	cur := f.trie.Root()
	best, bestLangs := -1, LangSet(0)
	prev := rune(-1)

	for j := start; j < buf.Len(); j++ {
		r := buf.Runes[j]
		if next, ok := cur.Next(r); ok {
			cur = next
		} else if r != prev || r == ' ' {
			break // dead end, and not a stretched letter
		}
		prev = r
		if found := cur.Tags() & langs; found != 0 && buf.IsBoundary(j+1) {
			best, bestLangs = j+1, found
		}
	}
	return best, bestLangs
}

// dictionaryWord rebuilds the dictionary word behind a match by walking the
// Trie again with the same rules as longestMatch: a rune with an edge is
// part of the word, a stretched repetition is not. It only runs on hits, so
// clean text pays nothing for it.
func (f *Filter) dictionaryWord(buf *normalizer.Buffer, start, end int) string {
	cur := f.trie.Root()
	word := make([]rune, 0, end-start)
	for _, r := range buf.Runes[start:end] {
		if next, ok := cur.Next(r); ok {
			cur = next
			word = append(word, r)
		}
	}
	return string(word)
}

// mask builds the censored copy of text. Matches are sorted and disjoint,
// so a single forward pass is enough.
func mask(text string, matches []Match, m Mask) string {
	char := m.Char
	if char == 0 {
		char = '*'
	}

	var sb strings.Builder
	sb.Grow(len(text))

	last := 0
	for _, match := range matches {
		sb.WriteString(text[last:match.Start])
		wordStart := true
		for _, r := range text[match.Start:match.End] {
			switch {
			case unicode.IsSpace(r):
				sb.WriteRune(r)
				wordStart = true
			case m.KeepFirst && wordStart:
				sb.WriteRune(r)
				wordStart = false
			default:
				sb.WriteRune(char)
				wordStart = false
			}
		}
		last = match.End
	}
	sb.WriteString(text[last:])
	return sb.String()
}
