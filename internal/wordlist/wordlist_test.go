package wordlist

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func TestLoadDir(t *testing.T) {
	fsys := fstest.MapFS{
		"pt-BR.txt":  {Data: []byte("# comentário\nporra\n\n  merda  \nfilho da puta\n")},
		"en.TXT":     {Data: []byte("shit\n")},
		"README.md":  {Data: []byte("ignored")},
		"sub/es.txt": {Data: []byte("ignored: not at the root")},
	}

	got, err := LoadDir(fsys)
	if err != nil {
		t.Fatal(err)
	}
	want := []Dictionary{
		{Lang: "en", File: "en.TXT", Words: []string{"shit"}},
		{Lang: "pt-br", File: "pt-BR.txt", Words: []string{"porra", "merda", "filho da puta"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LoadDir() = %+v, want %+v", got, want)
	}
}

func TestLoadDirEmpty(t *testing.T) {
	if _, err := LoadDir(fstest.MapFS{"notes.md": {}}); err == nil {
		t.Error("expected an error when no .txt files exist")
	}
}

func TestParse(t *testing.T) {
	got, err := Parse(strings.NewReader("a\r\n# c\r\n\r\nb c\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Parse() = %q, want %q", got, want)
	}
}

func TestNormalizeCode(t *testing.T) {
	for in, want := range map[string]string{"PT-BR": "pt-br", " en ": "en", "pt_BR": "pt-br"} {
		if got := NormalizeCode(in); got != want {
			t.Errorf("NormalizeCode(%q) = %q, want %q", in, got, want)
		}
	}
}
