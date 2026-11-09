package filter

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jp-epaminondas/language-filter/internal/wordlist"
	"github.com/jp-epaminondas/language-filter/wordlists"
)

// Small, fixed dictionaries keep these tests independent of the real lists,
// which are expected to change over time.
var testDicts = []wordlist.Dictionary{
	{Lang: "pt-br", Words: []string{
		"porra", "caralho", "merda", "puta", "cacete", "foda",
		"desgraçado", "filho da puta", "cu",
	}},
	{Lang: "en", Words: []string{"shit", "fuck", "ass", "merda"}},
}

func newTestFilter(t testing.TB) *Filter {
	t.Helper()
	f, err := New(testDicts)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func mustResolve(t testing.TB, f *Filter, codes ...string) LangSet {
	t.Helper()
	set, err := f.Resolve(codes)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func TestCensor(t *testing.T) {
	f := newTestFilter(t)
	pt := mustResolve(t, f, "pt-br")

	tests := []struct {
		name    string
		in      string
		want    string
		profane bool
	}{
		{"clean text", "Bom dia, tudo bem?", "Bom dia, tudo bem?", false},
		{"empty", "", "", false},
		{"simple", "que merda", "que *****", true},
		{"uppercase", "QUE MERDA", "QUE *****", true},
		{"leetspeak", "que m3rd4", "que *****", true},
		{"at sign", "p0rr@ meu", "***** meu", true},
		{"separators inside", "p.u.t.a", "*******", true},
		{"asterisks inside", "c*a*r*a*l*h*o", "*************", true},
		{"trailing punctuation", "porra!!!", "*****!!!", true},
		{"glued punctuation", "ola,merda", "ola,*****", true},
		{"stretched letters", "porrrraaaa", "**********", true},
		{"accent in list", "seu desgraçado", "seu **********", true},
		{"accent dropped by user", "seu desgracado", "seu **********", true},
		{"phrase keeps spaces", "seu filho da puta", "seu ***** ** ****", true},
		{"hyphenated suffix", "foda-se", "****-se", true},
		{"multiple", "merda e porra", "***** e *****", true},
		{"keeps original accents", "Ação: merda.", "Ação: *****.", true},

		// Scunthorpe problem: substrings inside innocent words must not match.
		{"inside word", "computador", "computador", false},
		{"prefix of longer word", "putaria", "putaria", false},
		{"cacetete", "cacetete", "cacetete", false},
		{"merdinha not in list", "merdinha", "merdinha", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := f.Censor(tt.in, pt)
			if got.Censored != tt.want {
				t.Errorf("Censor(%q).Censored = %q, want %q", tt.in, got.Censored, tt.want)
			}
			if got.HasProfanity != tt.profane {
				t.Errorf("Censor(%q).HasProfanity = %v, want %v", tt.in, got.HasProfanity, tt.profane)
			}
		})
	}
}

func TestCensorLanguageSelection(t *testing.T) {
	f := newTestFilter(t)
	// "cu" is profanity in pt-br but "cu later" is innocent English chat:
	// exactly why the caller must be able to pick languages.
	text := "cu later, shit happens"

	tests := []struct {
		langs    []string
		want     string
		detected []string
	}{
		{[]string{"pt-br"}, "** later, shit happens", []string{"pt-br"}},
		{[]string{"en"}, "cu later, **** happens", []string{"en"}},
		{[]string{"pt-br", "en"}, "** later, **** happens", []string{"en", "pt-br"}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.langs, "+"), func(t *testing.T) {
			got := f.Censor(text, mustResolve(t, f, tt.langs...))
			if got.Censored != tt.want {
				t.Errorf("Censored = %q, want %q", got.Censored, tt.want)
			}
			if codes := f.Codes(got.Detected); !reflect.DeepEqual(codes, tt.detected) {
				t.Errorf("Detected = %v, want %v", codes, tt.detected)
			}
		})
	}

	if got := f.Censor(text, 0); got.HasProfanity {
		t.Error("an empty language set must not detect anything")
	}
}

func TestWordInSeveralLanguages(t *testing.T) {
	f := newTestFilter(t)
	got := f.Censor("merda", f.All()) // listed in both test dictionaries
	if codes := f.Codes(got.Detected); !reflect.DeepEqual(codes, []string{"en", "pt-br"}) {
		t.Errorf("Detected = %v, want both languages", codes)
	}
	if f.Words() != 12 {
		t.Errorf("Words() = %d, want 12 distinct words", f.Words())
	}
}

func TestMatchDetails(t *testing.T) {
	f := newTestFilter(t)
	text := "Olá, P0RRRR4 e filho d4 puta!"
	got := f.Censor(text, f.All())

	want := []struct{ text, word string }{
		{"P0RRRR4", "porra"},               // leet + stretched letters
		{"filho d4 puta", "filho da puta"}, // longest match wins over "puta"
	}
	if len(got.Matches) != len(want) {
		t.Fatalf("matches = %+v", got.Matches)
	}
	for i, w := range want {
		m := got.Matches[i]
		if text[m.Start:m.End] != w.text || m.Word != w.word {
			t.Errorf("match %d = %q (%q), want %q (%q)", i, text[m.Start:m.End], m.Word, w.text, w.word)
		}
		if codes := f.Codes(m.Langs); !reflect.DeepEqual(codes, []string{"pt-br"}) {
			t.Errorf("match %d languages = %v", i, codes)
		}
	}
}

func TestMask(t *testing.T) {
	f := newTestFilter(t)
	pt := mustResolve(t, f, "pt-br")

	tests := []struct {
		name string
		in   string
		mask Mask
		want string
	}{
		{"default", "que p0rr4!", Mask{}, "que *****!"},
		{"custom char", "que p0rr4!", Mask{Char: '#'}, "que #####!"},
		{"multi-byte char", "que merda", Mask{Char: '█'}, "que █████"},
		{"keep first", "que p.u.t.a", Mask{KeepFirst: true}, "que p******"},
		{"keep first, phrase", "seu filho da puta", Mask{KeepFirst: true}, "seu f**** d* p***"},
		{"keep first + char", "que merda", Mask{KeepFirst: true, Char: '-'}, "que m----"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := f.CensorWith(tt.in, pt, tt.mask).Censored
			if got != tt.want {
				t.Errorf("CensorWith(%q) = %q, want %q", tt.in, got, tt.want)
			}
			// Masking never changes the length in characters.
			if len([]rune(got)) != len([]rune(tt.in)) {
				t.Errorf("length changed: %q -> %q", tt.in, got)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	f := newTestFilter(t)

	if _, err := f.Resolve([]string{"PT-BR", " en "}); err != nil {
		t.Errorf("codes should be case and space insensitive: %v", err)
	}
	_, err := f.Resolve([]string{"klingon"})
	if err == nil || !strings.Contains(err.Error(), "en, pt-br") {
		t.Errorf("unknown language error should list the valid ones, got %v", err)
	}
	if got := f.Codes(f.All()); !reflect.DeepEqual(got, []string{"en", "pt-br"}) {
		t.Errorf("Codes(All) = %v", got)
	}
}

func TestNewRejectsBadDictionaries(t *testing.T) {
	if _, err := New([]wordlist.Dictionary{{Lang: "en"}, {Lang: "EN"}}); err == nil {
		t.Error("duplicate language should fail")
	}
	if _, err := New([]wordlist.Dictionary{{Lang: " "}}); err == nil {
		t.Error("empty language code should fail")
	}
}

func TestEmbeddedWordlists(t *testing.T) {
	dicts, err := wordlist.LoadDir(wordlists.FS)
	if err != nil {
		t.Fatal(err)
	}
	f, err := New(dicts)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range f.Languages() {
		if l.Words == 0 {
			t.Errorf("language %q has no words", l.Code)
		}
	}
	defaults := f.Defaults()
	for in, want := range map[string]string{
		"vai se f0d3r":         "*** ** *****",
		"que merdas":           "que ******",
		"this is bullsh1t":     "this is ********",
		"o computador é top":   "o computador é top",
		"a classic assassin":   "a classic assassin",
		"foda-se":              "*******",
		"fuuuuck":              "*******",
		"seu b0i0l4":           "seu ******",
		"comprei água de coco": "comprei água de coco", // neutral words were dropped
		"a vaca foi pro pasto": "a vaca foi pro pasto", // pt-br-strict is opt-in
	} {
		if got := f.Censor(in, defaults).Censored; got != want {
			t.Errorf("Censor(%q) = %q, want %q", in, got, want)
		}
	}

	// Strict mode catches the double-meaning words when explicitly asked.
	strict := mustResolve(t, f, "pt-br", "pt-br-strict")
	if got := f.Censor("sua vaca", strict).Censored; got != "sua ****" {
		t.Errorf("strict: Censor(%q) = %q", "sua vaca", got)
	}

	// Identity terms must never be censored, not even in strict mode.
	for _, in := range []string{"ele é gay", "sou lésbica", "homossexual"} {
		if got := f.Censor(in, f.All()); got.HasProfanity {
			t.Errorf("identity term censored: %q -> %q", in, got.Censored)
		}
	}
}

func TestDefaultsSkipOptIn(t *testing.T) {
	f, err := New([]wordlist.Dictionary{
		{Lang: "pt-br", Words: []string{"merda"}},
		{Lang: "pt-br-strict", Words: []string{"vaca"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Codes(f.Defaults()); !reflect.DeepEqual(got, []string{"pt-br"}) {
		t.Errorf("Defaults() = %v, want [pt-br]", got)
	}
	if langs := f.Languages(); langs[0].OptIn || !langs[1].OptIn {
		t.Errorf("OptIn flags = %+v", langs)
	}

	onlyStrict, err := New([]wordlist.Dictionary{{Lang: "pt-br-strict", Words: []string{"vaca"}}})
	if err != nil {
		t.Fatal(err)
	}
	if onlyStrict.Defaults() != onlyStrict.All() {
		t.Error("with only opt-in dictionaries, Defaults should fall back to All")
	}
}

func TestCensorConcurrent(t *testing.T) {
	f := newTestFilter(t)
	pt := mustResolve(t, f, "pt-br")
	done := make(chan struct{})
	for range 32 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 1000 {
				if got := f.Censor("vai p0rr4", pt); got.Censored != "vai *****" {
					t.Errorf("got %q", got.Censored)
					return
				}
			}
		}()
	}
	for range 32 {
		<-done
	}
}

// ---------------------------------------------------------------------------
// Benchmarks: go test -bench=. -benchmem ./internal/filter
// ---------------------------------------------------------------------------

var sink Result

func benchmarkCensor(b *testing.B, f *Filter, langs LangSet, text string) {
	b.ReportAllocs()
	b.SetBytes(int64(len(text)))
	for b.Loop() {
		sink = f.Censor(text, langs)
	}
}

func BenchmarkCensor_ShortClean(b *testing.B) {
	f := newTestFilter(b)
	benchmarkCensor(b, f, f.All(), "Bom dia pessoal, alguém viu o jogo de ontem?")
}

func BenchmarkCensor_ShortProfane(b *testing.B) {
	f := newTestFilter(b)
	benchmarkCensor(b, f, f.All(), "Que p0rr4 de jogo, o juiz é um desgraçado!")
}

func BenchmarkCensor_1KB(b *testing.B) {
	f := newTestFilter(b)
	base := "Esse é um comentário comum de rede social com acentuação, m3rd4 e pontuação. "
	benchmarkCensor(b, f, f.All(), strings.Repeat(base, 1024/len(base)+1)[:1024])
}

func BenchmarkCensor_LargeDictionary(b *testing.B) {
	// 10k synthetic words on top of the test list: per-request cost must not
	// grow with the dictionary size, which is the whole point of the Trie.
	words := append([]string{}, testDicts[0].Words...)
	for i := range 10_000 {
		words = append(words, syntheticWord(i))
	}
	f, err := New([]wordlist.Dictionary{{Lang: "pt-br", Words: words}})
	if err != nil {
		b.Fatal(err)
	}
	benchmarkCensor(b, f, f.All(), "Que p0rr4 de jogo, o juiz é um desgraçado!")
}

func BenchmarkCensor_EmbeddedLists(b *testing.B) {
	// The real pt-br + en dictionaries, both selected: one single scan.
	dicts, err := wordlist.LoadDir(wordlists.FS)
	if err != nil {
		b.Fatal(err)
	}
	f, err := New(dicts)
	if err != nil {
		b.Fatal(err)
	}
	benchmarkCensor(b, f, f.All(), "Que p0rr4 de jogo, the referee is a fucking desgraçado!")
}

func syntheticWord(i int) string {
	var sb strings.Builder
	for n := i + 26*26; n > 0; n /= 26 {
		sb.WriteByte(byte('a' + n%26))
	}
	sb.WriteString("zx")
	return sb.String()
}
