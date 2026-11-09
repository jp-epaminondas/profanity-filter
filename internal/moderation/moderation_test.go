package moderation

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jp-epaminondas/profanity-filter/internal/wordlist"
)

var base = []wordlist.Dictionary{
	{Lang: "pt-br", File: "pt-br.txt", Words: []string{"merda", "porra", "vaca", "foda-se", "fodase"}},
	{Lang: "en", File: "en.txt", Words: []string{"shit"}},
}

func newService(t *testing.T, path string) *Service {
	t.Helper()
	s, err := New(base, path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func censor(t *testing.T, s *Service, text string) string {
	t.Helper()
	f := s.Filter()
	return f.Censor(text, f.All()).Censored
}

func TestReportCountsOncePerReporter(t *testing.T) {
	s := newService(t, "")

	for i, tc := range []struct {
		reporter string
		counted  bool
		total    int
	}{
		{"1.1.1.1", true, 1},
		{"1.1.1.1", false, 1}, // same client again: ignored
		{"2.2.2.2", true, 2},
	} {
		rep, counted, err := s.Report("pt-br", "VACA", tc.reporter, "é um animal")
		if err != nil {
			t.Fatalf("#%d: %v", i, err)
		}
		if counted != tc.counted || rep.Reports != tc.total {
			t.Errorf("#%d: counted=%v reports=%d, want %v %d", i, counted, rep.Reports, tc.counted, tc.total)
		}
	}

	reports := s.Reports()
	if len(reports) != 1 || reports[0].Word != "vaca" || len(reports[0].Reasons) != 2 {
		t.Errorf("Reports() = %+v", reports)
	}
}

func TestReportValidation(t *testing.T) {
	s := newService(t, "")
	if _, _, err := s.Report("klingon", "merda", "ip", ""); !errors.Is(err, ErrUnknownLanguage) {
		t.Errorf("unknown language: err = %v", err)
	}
	if _, _, err := s.Report("pt-br", "abacaxi", "ip", ""); !errors.Is(err, ErrWordNotFound) {
		t.Errorf("word not in dictionary: err = %v", err)
	}
	if _, _, err := s.Report("pt-br", "shit", "ip", ""); !errors.Is(err, ErrWordNotFound) {
		t.Errorf("word of another language: err = %v", err)
	}
}

func TestQuarantineAndRestore(t *testing.T) {
	s := newService(t, "")
	if got := censor(t, s, "que vaca"); got != "que ****" {
		t.Fatalf("before: %q", got)
	}

	st, err := s.Quarantine("PT-BR", "v4c4") // codes and words are normalized
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != StatusQuarantined || st.Word != "vaca" {
		t.Errorf("state = %+v", st)
	}
	if got := censor(t, s, "que vaca"); got != "que vaca" {
		t.Errorf("quarantined word still censored: %q", got)
	}
	if got := censor(t, s, "que merda"); got != "que *****" {
		t.Errorf("other words must keep working: %q", got)
	}
	if _, _, err := s.Report("pt-br", "vaca", "ip", ""); !errors.Is(err, ErrNotActive) {
		t.Errorf("reporting a quarantined word: err = %v", err)
	}
	if _, err := s.Quarantine("pt-br", "vaca"); err != nil {
		t.Errorf("quarantine must be idempotent: %v", err)
	}

	if _, err := s.Restore("pt-br", "vaca"); err != nil {
		t.Fatal(err)
	}
	if got := censor(t, s, "que vaca"); got != "que ****" {
		t.Errorf("restored word not censored: %q", got)
	}
	if _, err := s.Restore("pt-br", "vaca"); !errors.Is(err, ErrAlreadyActive) {
		t.Errorf("restore active word: err = %v", err)
	}
}

func TestRemove(t *testing.T) {
	s := newService(t, "")
	s.Report("pt-br", "vaca", "ip", "")

	if _, err := s.Remove("pt-br", "vaca"); err != nil {
		t.Fatal(err)
	}
	if len(s.Reports()) != 0 {
		t.Error("removing a word must close its reports")
	}
	if _, err := s.Quarantine("pt-br", "vaca"); !errors.Is(err, ErrAlreadyRemoved) {
		t.Errorf("quarantine after remove: err = %v", err)
	}
	if words := s.Words(); len(words) != 1 || words[0].Status != StatusRemoved {
		t.Errorf("Words() = %+v", words)
	}
}

func TestRemoveCoversEveryEntryWithTheSameForm(t *testing.T) {
	s := newService(t, "")
	// "foda-se" and "fodase" normalize to the same word.
	if _, err := s.Remove("pt-br", "foda-se"); err != nil {
		t.Fatal(err)
	}
	if got := censor(t, s, "fodase"); got != "fodase" {
		t.Errorf("got %q, want both spellings disabled", got)
	}
}

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.json")

	s := newService(t, path)
	s.Report("pt-br", "porra", "1.1.1.1", "")
	if _, err := s.Quarantine("pt-br", "vaca"); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || strings.Contains(string(data), "1.1.1.1") {
		t.Errorf("state file must exist and never store raw IPs:\n%s", data)
	}

	// A new process with the same file sees the same state.
	s2 := newService(t, path)
	if got := censor(t, s2, "que vaca"); got != "que vaca" {
		t.Errorf("quarantine not restored from disk: %q", got)
	}
	if _, counted, _ := s2.Report("pt-br", "porra", "1.1.1.1", ""); counted {
		t.Error("the same reporter must still be recognized after a restart")
	}
}

func TestAdd(t *testing.T) {
	s := newService(t, "")
	if got := censor(t, s, "seu otário"); got != "seu otário" {
		t.Fatalf("before: %q", got)
	}

	res, err := s.Add("PT-BR", []string{"otário", "  ", "merda", "otario", "Babaca", "x" + strings.Repeat("y", MaxWordLen)})
	if err != nil {
		t.Fatal(err)
	}
	var added []string
	for _, e := range res.Added {
		added = append(added, e.Word)
		if e.Source != SourceAPI || e.Status != StatusActive || e.AddedAt == nil {
			t.Errorf("added entry = %+v", e)
		}
	}
	if strings.Join(added, ",") != "otário,Babaca" {
		t.Errorf("added = %v", added)
	}
	reasons := map[string]string{}
	for _, sk := range res.Skipped {
		reasons[sk.Word] = sk.Reason
	}
	for word, want := range map[string]string{
		"  ":     "vazia",
		"merda":  "já existe (pt-br.txt)",
		"otario": "já existe (api)", // same normalized form as "otário", added above
	} {
		if !strings.Contains(reasons[word], want) {
			t.Errorf("skip reason for %q = %q, want it to contain %q", word, reasons[word], want)
		}
	}
	if len(res.Skipped) != 4 {
		t.Errorf("skipped = %+v", res.Skipped)
	}

	// Applied immediately, with every engine feature (leet, accents...).
	if got := censor(t, s, "seu 0t4r10 babaca"); got != "seu ****** ******" {
		t.Errorf("after add: %q", got)
	}

	// "mané" sent as Latin-1 arrives as "man\uFFFD": it must not become "man".
	res, _ = s.Add("pt-br", []string{"man\uFFFD"})
	if len(res.Added) != 0 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "UTF-8") {
		t.Errorf("corrupted word = %+v", res)
	}

	if _, err := s.Add("klingon", []string{"x"}); !errors.Is(err, ErrUnknownLanguage) {
		t.Errorf("unknown language: err = %v", err)
	}
}

func TestAddedWordLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := newService(t, path)
	if _, err := s.Add("pt-br", []string{"otário"}); err != nil {
		t.Fatal(err)
	}

	// Added words survive a restart.
	s = newService(t, path)
	if got := censor(t, s, "otário"); got != "******" {
		t.Fatalf("after restart: %q", got)
	}

	// They can be reported and quarantined like any other word.
	if _, _, err := s.Report("pt-br", "otario", "ip", ""); err != nil {
		t.Errorf("report added word: %v", err)
	}
	if _, err := s.Quarantine("pt-br", "otário"); err != nil {
		t.Fatal(err)
	}
	if got := censor(t, s, "otário"); got != "otário" {
		t.Errorf("quarantined added word still censored: %q", got)
	}

	// Removing an API word deletes it entirely: it can be added again later.
	if _, err := s.Remove("pt-br", "otário"); err != nil {
		t.Fatal(err)
	}
	if entries, _ := s.Entries(EntryFilter{Source: SourceAPI}); len(entries) != 0 {
		t.Errorf("removed API word still listed: %+v", entries)
	}
	if res, _ := s.Add("pt-br", []string{"otário"}); len(res.Added) != 1 {
		t.Errorf("re-adding a removed API word: %+v", res)
	}
}

func TestEntries(t *testing.T) {
	s := newService(t, "")
	s.Add("pt-br", []string{"otário"})
	s.Quarantine("pt-br", "vaca")
	s.Report("pt-br", "porra", "ip", "")

	all, err := s.Entries(EntryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 7 { // 5 pt-br lines + 1 en line + 1 added
		t.Errorf("len(all) = %d, want 7", len(all))
	}

	find := func(entries []Entry, word string) *Entry {
		for i := range entries {
			if entries[i].Word == word {
				return &entries[i]
			}
		}
		return nil
	}
	if e := find(all, "vaca"); e == nil || e.Source != "pt-br.txt" || e.Status != StatusQuarantined || e.UpdatedAt == nil {
		t.Errorf("vaca = %+v", e)
	}
	if e := find(all, "porra"); e == nil || e.Reports != 1 {
		t.Errorf("porra = %+v", e)
	}
	if e := find(all, "shit"); e == nil || e.Language != "en" || e.Source != "en.txt" {
		t.Errorf("shit = %+v", e)
	}
	if e := find(all, "foda-se"); e == nil || e.Normalized != "fodase" {
		t.Errorf("foda-se = %+v", e)
	}

	for name, tc := range map[string]struct {
		f    EntryFilter
		want int
	}{
		"by language":    {EntryFilter{Language: "EN"}, 1},
		"by status":      {EntryFilter{Status: StatusQuarantined}, 1},
		"by source file": {EntryFilter{Source: "PT-BR.TXT"}, 5},
		"by source api":  {EntryFilter{Source: SourceAPI}, 1},
		"by query, leet": {EntryFilter{Query: "f0d"}, 2},
		"combined":       {EntryFilter{Language: "pt-br", Status: StatusActive, Query: "a"}, 5}, // vaca is quarantined
	} {
		got, err := s.Entries(tc.f)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		} else if len(got) != tc.want {
			t.Errorf("%s: got %d entries, want %d", name, len(got), tc.want)
		}
	}

	if _, err := s.Entries(EntryFilter{Status: "banana"}); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("invalid status: err = %v", err)
	}
	if _, err := s.Entries(EntryFilter{Language: "xx"}); !errors.Is(err, ErrUnknownLanguage) {
		t.Errorf("unknown language: err = %v", err)
	}
}

func TestCorruptStateFileFailsFast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(base, path); err == nil {
		t.Error("a corrupt state file must stop the server, not be silently ignored")
	}
}

func TestReadersNeverSeeAPartialTrie(t *testing.T) {
	s := newService(t, "")
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				f := s.Filter()
				got := f.Censor("que merda, vaca", f.All()).Censored
				// "merda" is never touched; "vaca" flips between states.
				if got != "que *****, vaca" && got != "que *****, ****" {
					t.Errorf("inconsistent result: %q", got)
					return
				}
			}
		}()
	}
	for range 200 {
		s.Quarantine("pt-br", "vaca")
		s.Restore("pt-br", "vaca")
	}
	close(stop)
	wg.Wait()
}
