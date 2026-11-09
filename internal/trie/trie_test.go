package trie

import "testing"

const (
	tagA Tags = 1 << iota
	tagB
)

func TestInsertAndLookup(t *testing.T) {
	tr := New()
	for _, w := range []string{"puta", "putaria", "filho da puta", "ação2"} {
		if !tr.Insert([]rune(w), tagA) {
			t.Fatalf("Insert(%q) = false", w)
		}
	}

	if tr.Insert([]rune("puta"), tagA) {
		t.Error("duplicate insert should return false")
	}
	if tr.Insert(nil, tagA) {
		t.Error("empty insert should return false")
	}
	if tr.Insert([]rune("x"), 0) {
		t.Error("insert without tags should return false")
	}
	if tr.Len() != 4 {
		t.Errorf("Len() = %d, want 4", tr.Len())
	}

	for _, w := range []string{"puta", "putaria", "filho da puta", "ação2"} {
		if tr.Lookup([]rune(w)) != tagA {
			t.Errorf("Lookup(%q) = %b, want %b", w, tr.Lookup([]rune(w)), tagA)
		}
	}
	// Prefixes and extensions of a word are not words themselves.
	for _, w := range []string{"put", "putar", "filho", "", "ação"} {
		if tr.Lookup([]rune(w)) != 0 {
			t.Errorf("Lookup(%q) != 0", w)
		}
	}
}

func TestTagsMerge(t *testing.T) {
	tr := New()
	tr.Insert([]rune("merda"), tagA)
	if !tr.Insert([]rune("merda"), tagB) {
		t.Fatal("adding a new tag to an existing word should return true")
	}
	if got := tr.Lookup([]rune("merda")); got != tagA|tagB {
		t.Errorf("tags = %b, want %b", got, tagA|tagB)
	}
	if tr.Len() != 1 {
		t.Errorf("Len() = %d, want 1 (same word, two tags)", tr.Len())
	}
}

func TestSharedPrefixesShareNodes(t *testing.T) {
	tr := New()
	tr.Insert([]rune("puta"), tagA)
	before := tr.Nodes()
	tr.Insert([]rune("putaria"), tagA) // reuses p-u-t-a, adds r-i-a
	if got := tr.Nodes() - before; got != 3 {
		t.Errorf("new nodes = %d, want 3", got)
	}
}

func TestCursor(t *testing.T) {
	tr := New()
	tr.Insert([]rune("foda"), tagB)

	c := tr.Root()
	for _, r := range "fod" {
		var ok bool
		if c, ok = c.Next(r); !ok {
			t.Fatalf("Next(%q) failed", r)
		}
		if c.Tags() != 0 {
			t.Fatalf("unexpected word end at %q", r)
		}
	}
	if _, ok := c.Next('x'); ok {
		t.Error("Next('x') should fail")
	}
	c, _ = c.Next('a')
	if c.Tags() != tagB {
		t.Errorf("Tags() = %b, want %b", c.Tags(), tagB)
	}
	if c.Word() != "foda" {
		t.Errorf("Word() = %q, want %q", c.Word(), "foda")
	}
}
