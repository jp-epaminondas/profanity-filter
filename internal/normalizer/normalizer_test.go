package normalizer

import "testing"

func TestString(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"MERDA", "merda"},
		{"m3rd4", "merda"},
		{"P0RR@", "porra"},
		{"c4r4lh0", "caralho"},
		{"$ua", "sua"},
		{"p.u.t.a", "puta"},
		{"p*u*t*a", "puta"},
		{"Desgraçado", "desgracado"},
		{"AÇÃO É ÓTIMA", "acao e otima"},
		{"é", "e"}, // decomposed é (e + combining acute)
		{"  muitos   espaços \t\n aqui  ", "muitos espacos aqui"},
		{"", ""},
		// homoglyphs: Cyrillic and Greek letters drawn like Latin ones
		{"putа", "puta"},   // Cyrillic а
		{"РОRRА", "porra"}, // Cyrillic Р, О, А
		{"ΡΟRRΑ", "porra"}, // Greek Ρ, Ο, Α
		{"mеrdа", "merda"}, // Cyrillic е, а
		{"Νν", "nv"},       // Greek Ν looks like N; ν looks like v
		{"тест", "тecт"},   // Cyrillic т stays (it does not look like t)
	}
	for _, tt := range tests {
		if got := String(tt.in); got != tt.want {
			t.Errorf("String(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSpansPointToOriginalBytes(t *testing.T) {
	text := "Olá, p*uta"
	var buf Buffer
	Normalize(text, &buf)

	if got := string(buf.Runes); got != "ola puta" {
		t.Fatalf("runes = %q", got)
	}
	// 'a' came from the 2-byte "á" at bytes [2,4).
	if s := buf.Spans[2]; text[s.Start:s.End] != "á" {
		t.Errorf("span of 'a' = %q, want %q", text[s.Start:s.End], "á")
	}
	// 'u' came after a stripped '*', so it carries a soft break.
	if !buf.SoftBreak[5] {
		t.Errorf("expected soft break before 'u'")
	}
}

func TestIsBoundary(t *testing.T) {
	var buf Buffer
	Normalize("ola,merda computador", &buf)
	// o l a m e r d a _ c o m p u t a d o r
	// 0 1 2 3 4 5 6 7 8 9 ...
	cases := map[int]bool{0: true, 1: false, 3: true, 8: true, 9: true, 12: false}
	for i, want := range cases {
		if got := buf.IsBoundary(i); got != want {
			t.Errorf("IsBoundary(%d) = %v, want %v", i, got, want)
		}
	}
}

func BenchmarkNormalize(b *testing.B) {
	text := "Isso aqui é um texto normal de chat, com acentuação e p0ntu4ç@o!!"
	var buf Buffer
	b.ReportAllocs()
	b.SetBytes(int64(len(text)))
	for b.Loop() {
		Normalize(text, &buf)
	}
}
