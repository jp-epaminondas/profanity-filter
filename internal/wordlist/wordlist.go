// Package wordlist loads the per-language dictionaries that feed the Trie.
//
// A dictionary directory holds one UTF-8 text file per language, named after
// the language code:
//
//	wordlists/
//	├── en.txt
//	└── pt-br.txt
//
// Each file lists one word or expression per line. Blank lines and lines
// starting with '#' are ignored. Casing and accents do not matter, because
// entries are normalized before being inserted.
package wordlist

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// Ext is the file extension of a dictionary.
const Ext = ".txt"

// Dictionary is the word list of a single language.
type Dictionary struct {
	Lang  string // language code taken from the file name, e.g. "pt-br"
	File  string // file the words came from, e.g. "pt-br.txt" (informative)
	Words []string
}

// LoadDir reads every "*.txt" file at the root of fsys as a dictionary.
// The result is sorted by language code, so it is deterministic.
//
// Taking an fs.FS means the same code loads the dictionaries embedded in the
// binary (see package wordlists) or a directory on disk (os.DirFS).
func LoadDir(fsys fs.FS) ([]Dictionary, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read word list directory: %w", err)
	}

	var dicts []Dictionary
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(path.Ext(e.Name()), Ext) {
			continue
		}
		f, err := fsys.Open(e.Name())
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", e.Name(), err)
		}
		words, err := Parse(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		dicts = append(dicts, Dictionary{
			Lang:  NormalizeCode(strings.TrimSuffix(e.Name(), path.Ext(e.Name()))),
			File:  e.Name(),
			Words: words,
		})
	}
	if len(dicts) == 0 {
		return nil, fmt.Errorf("no %s word lists found", Ext)
	}

	sort.Slice(dicts, func(i, j int) bool { return dicts[i].Lang < dicts[j].Lang })
	return dicts, nil
}

// Parse reads one entry per line, skipping blank lines and '#' comments.
func Parse(r io.Reader) ([]string, error) {
	var words []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		words = append(words, line)
	}
	return words, sc.Err()
}

// NormalizeCode canonicalizes a language code, so "PT-BR", " pt-br " and
// "pt_BR" all refer to the same dictionary.
func NormalizeCode(code string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(code)), "_", "-")
}
