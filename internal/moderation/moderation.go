// Package moderation manages the lifecycle of dictionary words at runtime:
// users report false positives, and an admin adds, quarantines, removes or
// restores words without restarting the server.
//
// Concurrency model (copy-on-write):
//
//   - Reads never lock. Requests call Filter(), which is a single atomic
//     pointer load, and use that immutable snapshot until they finish.
//
//   - Writes are serialized by a mutex. A change that affects matching
//     (quarantine, remove, restore) builds a brand-new Filter from the base
//     lists minus the disabled words, then publishes it with one atomic store.
//     In-flight requests keep the old Trie; new requests get the new one.
//     Nobody ever observes a half-built Trie.
//
// Persistence: decisions and reports are saved to a JSON file, written
// atomically (temp file + rename), and re-applied on top of the base lists at
// startup. The base .txt files are never modified.
package moderation

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jp-epaminondas/language-filter/internal/filter"
	"github.com/jp-epaminondas/language-filter/internal/normalizer"
	"github.com/jp-epaminondas/language-filter/internal/wordlist"
)

// Errors returned by Service. The API layer maps them to HTTP status codes.
var (
	ErrUnknownLanguage = errors.New("idioma desconhecido")
	ErrWordNotFound    = errors.New("palavra não está no dicionário")
	ErrNotActive       = errors.New("palavra não está ativa (em quarentena ou removida)")
	ErrAlreadyRemoved  = errors.New("palavra já foi removida; use restore para reativá-la")
	ErrAlreadyActive   = errors.New("palavra já está ativa")
	ErrInvalidStatus   = errors.New("status inválido (use active, quarantined ou removed)")
)

// SourceAPI is the source of words added through the admin API, as opposed
// to words loaded from a .txt file.
const SourceAPI = "api"

// MaxWordLen limits words added through the API (in runes).
const MaxWordLen = 64

const (
	stateVersion = 1
	maxReasons   = 10    // most recent reasons kept per word
	maxReasonLen = 280   // runes
	maxReporters = 10000 // distinct reporters tracked per word
)

// Status is the moderation state of a dictionary word.
type Status string

const (
	StatusActive      Status = "active"      // censored normally
	StatusQuarantined Status = "quarantined" // temporarily not censored, under review
	StatusRemoved     Status = "removed"     // final decision: not censored
)

// WordState is an admin decision about a word.
type WordState struct {
	Language  string    `json:"language"`
	Word      string    `json:"word"`
	Status    Status    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Report is the public view of the complaints about a word.
type Report struct {
	Language        string    `json:"language"`
	Word            string    `json:"word"`
	Status          Status    `json:"status"`
	Reports         int       `json:"reports"`
	Reasons         []string  `json:"reasons,omitempty"`
	FirstReportedAt time.Time `json:"first_reported_at"`
	LastReportedAt  time.Time `json:"last_reported_at"`
}

// reportRecord is the persisted form of a Report. Reporters are salted
// hashes, never raw IP addresses.
type reportRecord struct {
	Language        string    `json:"language"`
	Word            string    `json:"word"`
	Reporters       []string  `json:"reporters"`
	Reasons         []string  `json:"reasons,omitempty"`
	FirstReportedAt time.Time `json:"first_reported_at"`
	LastReportedAt  time.Time `json:"last_reported_at"`

	seen map[string]struct{} // index over Reporters, rebuilt on load
}

type snapshot struct {
	Version int             `json:"version"`
	Salt    string          `json:"salt"`
	Words   []*WordState    `json:"words"`
	Added   []*addedWord    `json:"added"`
	Reports []*reportRecord `json:"reports"`
}

// Entry is one word of the dictionaries, as listed by Entries.
type Entry struct {
	Language   string     `json:"language"`
	Word       string     `json:"word"`       // as written in its source
	Normalized string     `json:"normalized"` // form actually matched by the Trie
	Source     string     `json:"source"`     // file name (e.g. "pt-br.txt") or "api"
	Status     Status     `json:"status"`
	Reports    int        `json:"reports"`
	AddedAt    *time.Time `json:"added_at,omitempty"`   // only for source "api"
	UpdatedAt  *time.Time `json:"updated_at,omitempty"` // last admin decision
}

// EntryFilter narrows Entries. Zero values mean "any".
type EntryFilter struct {
	Language string
	Status   Status
	Source   string // file name or "api", case-insensitive
	Query    string // substring of the normalized word ("f0d" finds "foda")
}

// AddResult is the outcome of Add: words are added individually, so one
// invalid or duplicated word does not reject the others.
type AddResult struct {
	Added   []Entry   `json:"added"`
	Skipped []Skipped `json:"skipped"`
}

// Skipped explains why a word was not added.
type Skipped struct {
	Word   string `json:"word"`
	Reason string `json:"reason"`
}

// addedWord is a word added through the API, persisted in the state file.
type addedWord struct {
	Language string    `json:"language"`
	Word     string    `json:"word"` // as typed by the admin
	AddedAt  time.Time `json:"added_at"`
}

// key identifies a word: language code + normalized form, so "V4CA",
// "vaca" and "váca" all refer to the same entry.
type key struct{ lang, word string }

// Service owns the live Filter and the moderation state. It is safe for
// concurrent use.
type Service struct {
	current atomic.Pointer[filter.Filter]

	mu      sync.Mutex
	base    []wordlist.Dictionary
	norms   [][]string          // normalized form of base[i].Words[j]
	known   map[key]string      // every word of the base lists -> its file
	langs   map[string]struct{} // every language code of the base lists
	added   map[key]*addedWord  // words added through the API
	words   map[key]*WordState  // admin decisions (absent = active)
	reports map[key]*reportRecord
	salt    string
	path    string // "" disables persistence
	dirty   bool   // reports changed since the last save
	now     func() time.Time
}

// New loads the state file at path (if it exists) and builds the first
// Filter from base minus the disabled words. An empty path keeps everything
// in memory.
func New(base []wordlist.Dictionary, path string) (*Service, error) {
	s := &Service{
		base:    base,
		norms:   make([][]string, len(base)),
		known:   make(map[key]string),
		langs:   make(map[string]struct{}),
		added:   make(map[key]*addedWord),
		words:   make(map[key]*WordState),
		reports: make(map[key]*reportRecord),
		path:    path,
		now:     time.Now,
	}
	for i, d := range base {
		code := wordlist.NormalizeCode(d.Lang)
		s.langs[code] = struct{}{}
		s.norms[i] = make([]string, len(d.Words))
		for j, w := range d.Words {
			n := normalizer.String(w)
			s.norms[i][j] = n
			if _, dup := s.known[key{code, n}]; !dup {
				s.known[key{code, n}] = d.File
			}
		}
	}

	if err := s.load(); err != nil {
		return nil, err
	}
	if s.salt == "" {
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return nil, fmt.Errorf("generate salt: %w", err)
		}
		s.salt = hex.EncodeToString(salt)
		s.dirty = true
	}
	if err := s.rebuild(); err != nil {
		return nil, err
	}
	return s, nil
}

// Filter returns the current Filter snapshot. It never blocks.
func (s *Service) Filter() *filter.Filter { return s.current.Load() }

// Report registers a complaint that word is not profanity. Each reporter
// (e.g. a client IP) counts once per word; counted reports whether this call
// increased the counter.
func (s *Service) Report(lang, word, reporter, reason string) (r Report, counted bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	k, err := s.resolve(lang, word)
	if err != nil {
		return Report{}, false, err
	}
	if st := s.status(k); st != StatusActive {
		return Report{}, false, ErrNotActive
	}

	now := s.now().UTC()
	rec := s.reports[k]
	if rec == nil {
		rec = &reportRecord{
			Language: k.lang, Word: k.word,
			FirstReportedAt: now, seen: make(map[string]struct{}),
		}
		s.reports[k] = rec
	}

	id := s.hash(reporter)
	if _, dup := rec.seen[id]; !dup && len(rec.Reporters) < maxReporters {
		rec.Reporters = append(rec.Reporters, id)
		rec.seen[id] = struct{}{}
		counted = true
	}
	if reason = truncate(strings.TrimSpace(reason), maxReasonLen); reason != "" && counted {
		rec.Reasons = append(rec.Reasons, reason)
		if len(rec.Reasons) > maxReasons {
			rec.Reasons = rec.Reasons[len(rec.Reasons)-maxReasons:]
		}
	}
	rec.LastReportedAt = now
	s.dirty = true

	return s.view(k, rec), counted, nil
}

// Reports lists every reported word, most reported first.
func (s *Service) Reports() []Report {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Report, 0, len(s.reports))
	for k, rec := range s.reports {
		out = append(out, s.view(k, rec))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Reports != out[j].Reports {
			return out[i].Reports > out[j].Reports
		}
		return out[i].LastReportedAt.After(out[j].LastReportedAt)
	})
	return out
}

// Words lists every admin decision (quarantined and removed words).
// To list the dictionary itself, use Entries.
func (s *Service) Words() []WordState {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]WordState, 0, len(s.words))
	for _, w := range s.words {
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Language != out[j].Language {
			return out[i].Language < out[j].Language
		}
		return out[i].Word < out[j].Word
	})
	return out
}

// Quarantine stops censoring word while it is under review. It is
// reversible with Restore. Reports are kept as evidence for the review.
func (s *Service) Quarantine(lang, word string) (WordState, error) {
	return s.decide(lang, word, StatusQuarantined)
}

// Remove stops censoring word for good and closes its reports. A word that
// only exists because it was added through the API is deleted entirely.
func (s *Service) Remove(lang, word string) (WordState, error) {
	s.mu.Lock()
	k, err := s.resolve(lang, word)
	if err != nil {
		s.mu.Unlock()
		return WordState{}, err
	}
	if _, inBase := s.known[k]; inBase {
		s.mu.Unlock()
		return s.decide(lang, word, StatusRemoved)
	}
	defer s.mu.Unlock()

	prevAdded, prevWord, prevReports := s.added[k], s.words[k], s.reports[k]
	delete(s.added, k)
	delete(s.words, k)
	delete(s.reports, k)
	if err := s.commit(); err != nil {
		s.added[k] = prevAdded
		if prevWord != nil {
			s.words[k] = prevWord
		}
		if prevReports != nil {
			s.reports[k] = prevReports
		}
		return WordState{}, err
	}
	return WordState{Language: k.lang, Word: k.word, Status: StatusRemoved, UpdatedAt: s.now().UTC()}, nil
}

// Add registers new words for an existing language. Each word is validated
// on its own: invalid or already known words are reported in Skipped and do
// not prevent the others from being added. All accepted words are applied
// with a single Trie rebuild and a single save.
func (s *Service) Add(lang string, words []string) (AddResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	code := wordlist.NormalizeCode(lang)
	if _, ok := s.langs[code]; !ok {
		return AddResult{}, fmt.Errorf("%w %q", ErrUnknownLanguage, lang)
	}

	res := AddResult{Added: []Entry{}, Skipped: []Skipped{}}
	now := s.now().UTC()
	var newKeys []key
	for _, raw := range words {
		w := strings.TrimSpace(raw)
		k := key{code, normalizer.String(w)}
		switch {
		case k.word == "":
			res.Skipped = append(res.Skipped, Skipped{raw, "inválida: vazia depois da normalização"})
		case utf8.RuneCountInString(w) > MaxWordLen:
			res.Skipped = append(res.Skipped, Skipped{raw, fmt.Sprintf("muito longa (máx. %d caracteres)", MaxWordLen)})
		case s.exists(k):
			res.Skipped = append(res.Skipped, Skipped{raw, s.existsReason(k)})
		default:
			aw := &addedWord{Language: code, Word: w, AddedAt: now}
			s.added[k] = aw
			newKeys = append(newKeys, k)
			res.Added = append(res.Added, s.entry(k, w, SourceAPI))
		}
	}
	if len(newKeys) == 0 {
		return res, nil
	}
	if err := s.commit(); err != nil {
		for _, k := range newKeys {
			delete(s.added, k)
		}
		return AddResult{}, err
	}
	return res, nil
}

// Entries lists the words of the dictionaries, one per source line (plus
// the words added through the API), sorted by language and word.
func (s *Service) Entries(f EntryFilter) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	lang := wordlist.NormalizeCode(f.Language)
	if lang != "" {
		if _, ok := s.langs[lang]; !ok {
			return nil, fmt.Errorf("%w %q", ErrUnknownLanguage, f.Language)
		}
	}
	switch f.Status {
	case "", StatusActive, StatusQuarantined, StatusRemoved:
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidStatus, f.Status)
	}
	query := normalizer.String(f.Query)

	keep := func(e Entry) bool {
		return (lang == "" || e.Language == lang) &&
			(f.Status == "" || e.Status == f.Status) &&
			(f.Source == "" || strings.EqualFold(e.Source, f.Source)) &&
			(query == "" || strings.Contains(e.Normalized, query))
	}

	out := []Entry{}
	for i, d := range s.base {
		code := wordlist.NormalizeCode(d.Lang)
		for j, w := range d.Words {
			if e := s.entry(key{code, s.norms[i][j]}, w, d.File); keep(e) {
				out = append(out, e)
			}
		}
	}
	for k, aw := range s.added {
		if e := s.entry(k, aw.Word, SourceAPI); keep(e) {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Language != b.Language {
			return a.Language < b.Language
		}
		if a.Normalized != b.Normalized {
			return a.Normalized < b.Normalized
		}
		return a.Word < b.Word
	})
	return out, nil
}

func (s *Service) entry(k key, word, source string) Entry {
	e := Entry{
		Language:   k.lang,
		Word:       word,
		Normalized: k.word,
		Source:     source,
		Status:     s.status(k),
	}
	if rec := s.reports[k]; rec != nil {
		e.Reports = len(rec.Reporters)
	}
	if aw := s.added[k]; aw != nil && source == SourceAPI {
		t := aw.AddedAt
		e.AddedAt = &t
	}
	if ws := s.words[k]; ws != nil {
		t := ws.UpdatedAt
		e.UpdatedAt = &t
	}
	return e
}

func (s *Service) exists(k key) bool {
	_, inBase := s.known[k]
	_, inAPI := s.added[k]
	return inBase || inAPI
}

func (s *Service) existsReason(k key) string {
	src := s.known[k]
	if src == "" {
		src = SourceAPI
	}
	switch s.status(k) {
	case StatusQuarantined:
		return fmt.Sprintf("já existe (%s), mas está em quarentena: use restore", src)
	case StatusRemoved:
		return fmt.Sprintf("já existe (%s), mas foi removida: use restore", src)
	default:
		return fmt.Sprintf("já existe (%s)", src)
	}
}

// Restore re-activates a quarantined or removed word and clears its reports:
// the admin has decided that the word IS profanity.
func (s *Service) Restore(lang, word string) (WordState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	k, err := s.resolve(lang, word)
	if err != nil {
		return WordState{}, err
	}
	prev, ok := s.words[k]
	if !ok {
		return WordState{}, ErrAlreadyActive
	}
	prevReports := s.reports[k]

	delete(s.words, k)
	delete(s.reports, k)
	if err := s.commit(); err != nil {
		s.words[k] = prev // roll back so memory matches what is on disk
		if prevReports != nil {
			s.reports[k] = prevReports
		}
		return WordState{}, err
	}
	return WordState{Language: k.lang, Word: k.word, Status: StatusActive, UpdatedAt: s.now().UTC()}, nil
}

func (s *Service) decide(lang, word string, to Status) (WordState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	k, err := s.resolve(lang, word)
	if err != nil {
		return WordState{}, err
	}
	prev := s.words[k]
	if prev != nil {
		if prev.Status == to {
			return *prev, nil // idempotent
		}
		if prev.Status == StatusRemoved {
			return WordState{}, ErrAlreadyRemoved
		}
	}
	prevReports := s.reports[k]

	st := &WordState{Language: k.lang, Word: k.word, Status: to, UpdatedAt: s.now().UTC()}
	s.words[k] = st
	if to == StatusRemoved {
		delete(s.reports, k)
	}
	if err := s.commit(); err != nil {
		if prev != nil {
			s.words[k] = prev
		} else {
			delete(s.words, k)
		}
		if prevReports != nil {
			s.reports[k] = prevReports
		}
		return WordState{}, err
	}
	return *st, nil
}

// commit applies an admin decision: build the new Filter, persist the state,
// and only then publish the Filter. If anything fails, nothing is published
// and the caller rolls its in-memory change back, so memory, disk and the
// live Trie never disagree. Decisions are saved synchronously: when the API
// answers 200, the change is on disk.
func (s *Service) commit() error {
	f, err := s.build()
	if err != nil {
		return err
	}
	if err := s.save(); err != nil {
		return err
	}
	s.current.Store(f)
	return nil
}

// rebuild builds and publishes a Filter. Used once, by New.
func (s *Service) rebuild() error {
	f, err := s.build()
	if err != nil {
		return err
	}
	s.current.Store(f)
	return nil
}

// build creates a Filter from the base lists minus disabled words.
// Must be called with mu held (or during New).
func (s *Service) build() (*filter.Filter, error) {
	dicts := make([]wordlist.Dictionary, len(s.base))
	for i, d := range s.base {
		code := wordlist.NormalizeCode(d.Lang)
		words := make([]string, 0, len(d.Words))
		for j, w := range d.Words {
			if _, disabled := s.words[key{code, s.norms[i][j]}]; !disabled {
				words = append(words, w)
			}
		}
		for k, aw := range s.added {
			if _, disabled := s.words[k]; k.lang == code && !disabled {
				words = append(words, aw.Word)
			}
		}
		// Language order and codes never change, so the LangSet bits of the
		// new Filter are identical to the old one's.
		dicts[i] = wordlist.Dictionary{Lang: d.Lang, File: d.File, Words: words}
	}
	return filter.New(dicts)
}

// Flush saves pending report changes. Call it periodically and on shutdown.
func (s *Service) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	return s.save()
}

func (s *Service) resolve(lang, word string) (key, error) {
	code := wordlist.NormalizeCode(lang)
	if _, ok := s.langs[code]; !ok {
		return key{}, fmt.Errorf("%w %q", ErrUnknownLanguage, lang)
	}
	k := key{code, normalizer.String(word)}
	if !s.exists(k) || k.word == "" {
		return key{}, fmt.Errorf("%w: %q (%s)", ErrWordNotFound, word, code)
	}
	return k, nil
}

func (s *Service) status(k key) Status {
	if w, ok := s.words[k]; ok {
		return w.Status
	}
	return StatusActive
}

func (s *Service) view(k key, rec *reportRecord) Report {
	return Report{
		Language:        k.lang,
		Word:            k.word,
		Status:          s.status(k),
		Reports:         len(rec.Reporters),
		Reasons:         append([]string(nil), rec.Reasons...),
		FirstReportedAt: rec.FirstReportedAt,
		LastReportedAt:  rec.LastReportedAt,
	}
}

// hash turns a reporter id (an IP address) into a salted SHA-256 digest, so
// the state file never stores personal data. The salt is random per
// installation and persisted, so the same IP maps to the same hash across
// restarts (which keeps the one-vote-per-IP rule working).
func (s *Service) hash(id string) string {
	sum := sha256.Sum256([]byte(s.salt + ":" + id))
	return hex.EncodeToString(sum[:16])
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

func (s *Service) load() error {
	if s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // first run
	}
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}

	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("parse state %s: %w", s.path, err)
	}
	if snap.Version != stateVersion {
		return fmt.Errorf("state %s: unsupported version %d", s.path, snap.Version)
	}

	s.salt = snap.Salt
	for _, w := range snap.Words {
		if w != nil {
			s.words[key{w.Language, w.Word}] = w
		}
	}
	for _, a := range snap.Added {
		if a != nil {
			s.added[key{a.Language, normalizer.String(a.Word)}] = a
		}
	}
	for _, r := range snap.Reports {
		if r == nil {
			continue
		}
		r.seen = make(map[string]struct{}, len(r.Reporters))
		for _, id := range r.Reporters {
			r.seen[id] = struct{}{}
		}
		s.reports[key{r.Language, r.Word}] = r
	}
	return nil
}

// save writes the state atomically: a crash mid-write leaves either the old
// file or the new one, never a truncated mix.
func (s *Service) save() error {
	if s.path == "" {
		s.dirty = false
		return nil
	}

	snap := snapshot{
		Version: stateVersion,
		Salt:    s.salt,
		Words:   make([]*WordState, 0, len(s.words)),
		Added:   make([]*addedWord, 0, len(s.added)),
		Reports: make([]*reportRecord, 0, len(s.reports)),
	}
	for _, a := range s.added {
		snap.Added = append(snap.Added, a)
	}
	sort.Slice(snap.Added, func(i, j int) bool {
		return snap.Added[i].Language+snap.Added[i].Word < snap.Added[j].Language+snap.Added[j].Word
	})
	for _, w := range s.words {
		snap.Words = append(snap.Words, w)
	}
	for _, r := range s.reports {
		snap.Reports = append(snap.Reports, r)
	}
	// Deterministic output: easy to diff and to review.
	sort.Slice(snap.Words, func(i, j int) bool {
		return snap.Words[i].Language+snap.Words[i].Word < snap.Words[j].Language+snap.Words[j].Word
	})
	sort.Slice(snap.Reports, func(i, j int) bool {
		return snap.Reports[i].Language+snap.Reports[i].Word < snap.Reports[j].Language+snap.Reports[j].Word
	})

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*.json")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	s.dirty = false
	return nil
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max])
}
