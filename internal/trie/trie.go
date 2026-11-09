// Package trie implements a rune-based prefix tree (Trie) optimized for the
// read-heavy workload of a profanity filter.
//
// Design notes:
//
//   - Children for 'a'..'z' and ' ' live in a fixed-size array, so the hot
//     path of a lookup is a bounds-checked array index instead of a map
//     lookup or hashing. Any other rune (digits, non-Latin letters) falls
//     back to a lazily allocated map, keeping the structure Unicode-correct.
//
//   - The Trie is built once at startup and never mutated afterwards, so it
//     can be read by any number of goroutines concurrently without locks.
//
//   - Every word is tagged with a 64-bit set (Tags). The filter uses one bit
//     per language, so a single Trie serves every language combination: a
//     word "exists" for a request when its tags intersect the requested set.
//
//   - Scanning is exposed through a tiny value-type Cursor. Callers walk the
//     tree one rune at a time, which lets the filter layer implement its own
//     matching policy (word boundaries, repeated letters) without the Trie
//     knowing anything about it, and without allocating.
package trie

// Tags is a set of up to 64 labels attached to a word, one bit per label.
type Tags uint64

// alphabet is the number of directly indexed children: 'a'..'z' plus ' '.
const alphabet = 27

type node struct {
	children [alphabet]*node
	extra    map[rune]*node // children outside the fast alphabet, rarely used
	tags     Tags           // non-zero when a dictionary word ends here
	word     string         // the word ending here, stored once at insert time
}

// index maps a rune to its slot in node.children, or -1 if it has none.
func index(r rune) int {
	switch {
	case r >= 'a' && r <= 'z':
		return int(r - 'a')
	case r == ' ':
		return alphabet - 1
	default:
		return -1
	}
}

func (n *node) child(r rune) *node {
	if i := index(r); i >= 0 {
		return n.children[i]
	}
	return n.extra[r] // nil map read is safe and returns nil
}

func (n *node) addChild(r rune) *node {
	if c := n.child(r); c != nil {
		return c
	}
	c := &node{}
	if i := index(r); i >= 0 {
		n.children[i] = c
	} else {
		if n.extra == nil {
			n.extra = make(map[rune]*node)
		}
		n.extra[r] = c
	}
	return c
}

// Trie is a prefix tree of words.
type Trie struct {
	root  *node
	words int
	nodes int
}

// New returns an empty Trie.
func New() *Trie {
	return &Trie{root: &node{}, nodes: 1}
}

// Insert adds a word with the given tags (merged with any tags it already
// has). It returns false when the word is empty, tags is zero, or the word
// already carried all of those tags. Complexity: O(len(word)).
func (t *Trie) Insert(word []rune, tags Tags) bool {
	if len(word) == 0 || tags == 0 {
		return false
	}
	cur := t.root
	for _, r := range word {
		next := cur.child(r)
		if next == nil {
			next = cur.addChild(r)
			t.nodes++
		}
		cur = next
	}
	if cur.tags&tags == tags {
		return false
	}
	if cur.tags == 0 {
		t.words++
		cur.word = string(word)
	}
	cur.tags |= tags
	return true
}

// Lookup returns the tags of word, or 0 if it was never inserted.
// Complexity: O(len(word)), independent of how many words the Trie holds.
func (t *Trie) Lookup(word []rune) Tags {
	c := t.Root()
	for _, r := range word {
		var ok bool
		if c, ok = c.Next(r); !ok {
			return 0
		}
	}
	return c.Tags()
}

// Len returns the number of distinct words stored.
func (t *Trie) Len() int { return t.words }

// Nodes returns the number of nodes, including the root.
func (t *Trie) Nodes() int { return t.nodes }

// Root returns a Cursor positioned at the root of the Trie.
func (t *Trie) Root() Cursor { return Cursor{n: t.root} }

// Cursor is a position inside the Trie. It is a plain value (one pointer),
// so advancing it never allocates.
type Cursor struct {
	n *node
}

// Next follows the edge labeled r. It returns false (and the original
// cursor) when no such edge exists.
func (c Cursor) Next(r rune) (Cursor, bool) {
	if child := c.n.child(r); child != nil {
		return Cursor{n: child}, true
	}
	return c, false
}

// Tags returns the tags of the word ending at this position (0 if none).
func (c Cursor) Tags() Tags { return c.n.tags }

// Word returns the word ending at this position ("" if none). The string
// was built at insert time, so reading it during a scan never allocates.
func (c Cursor) Word() string { return c.n.word }
