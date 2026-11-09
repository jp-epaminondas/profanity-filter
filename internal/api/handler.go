// Package api exposes the profanity filter over HTTP.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jp-epaminondas/language-filter/internal/clock"
	"github.com/jp-epaminondas/language-filter/internal/filter"
	"github.com/jp-epaminondas/language-filter/internal/moderation"
	"github.com/jp-epaminondas/language-filter/web"
)

// Request limits. They bound the work a single request can cause, so one
// client cannot monopolize the CPU or memory of the server.
const (
	// MaxBodyBytes limits POST /v1/filter. Profanity filtering targets chat
	// messages, comments and usernames, not whole documents.
	MaxBodyBytes = 64 << 10 // 64 KiB

	// MaxBatchTexts and MaxBatchBodyBytes limit POST /v1/filter/batch.
	// 100 texts at a few µs each stay well under a millisecond of CPU.
	MaxBatchTexts     = 100
	MaxBatchBodyBytes = 1 << 20 // 1 MiB

	// MaxTextBytes limits each text of a batch, matching the single endpoint.
	MaxTextBytes = MaxBodyBytes
)

// FilterRequest is the body of POST /v1/filter.
type FilterRequest struct {
	// Text is a pointer so a missing field can be told apart from "".
	Text *string `json:"text"`
	// Languages selects the dictionaries to apply, e.g. ["pt-br", "en"].
	// When omitted, the server default is used.
	Languages []string `json:"languages,omitempty"`
	MaskOptions
}

// MaskOptions choose how detected words are replaced. Shared by the single
// and the batch endpoints.
type MaskOptions struct {
	// Mask is "full" (default: "*****") or "first" (keep the first letter:
	// "p****").
	Mask string `json:"mask,omitempty"`
	// MaskChar is the replacement character, default "*".
	MaskChar string `json:"mask_char,omitempty"`
}

// MatchView describes one detected word.
type MatchView struct {
	// Text is the word as written in the input, e.g. "P0RRR4".
	Text string `json:"text"`
	// Word is the dictionary entry it matched, e.g. "porra".
	Word string `json:"word"`
	// Start and End are positions in characters (Unicode code points), not
	// bytes, in both the input and censored_text: masking never changes the
	// length of the text in characters.
	Start     int      `json:"start"`
	End       int      `json:"end"`
	Languages []string `json:"languages"`
}

// FilterResponse is the body returned by POST /v1/filter.
type FilterResponse struct {
	HasProfanity bool        `json:"has_profanity"`
	CensoredText string      `json:"censored_text"`
	Matches      []MatchView `json:"matches"`
	// Languages are the dictionaries applied to this request.
	Languages []string `json:"languages"`
	// DetectedLanguages are the dictionaries that matched at least one word.
	DetectedLanguages []string `json:"detected_languages"`
	// ElapsedTime is the human-readable processing time, e.g. "3.1µs".
	ElapsedTime string `json:"elapsed_time"`
	// ElapsedTimeNs is the same value in nanoseconds, for machines.
	ElapsedTimeNs int64 `json:"elapsed_time_ns"`
}

// BatchRequest is the body of POST /v1/filter/batch.
type BatchRequest struct {
	Texts     []string `json:"texts"`
	Languages []string `json:"languages,omitempty"`
	MaskOptions
}

// BatchItem is the result for one text of a batch, in the same order.
type BatchItem struct {
	HasProfanity      bool        `json:"has_profanity"`
	CensoredText      string      `json:"censored_text"`
	Matches           []MatchView `json:"matches"`
	DetectedLanguages []string    `json:"detected_languages"`
}

// BatchResponse is the body returned by POST /v1/filter/batch.
type BatchResponse struct {
	Results      []BatchItem `json:"results"`
	Count        int         `json:"count"`
	ProfaneCount int         `json:"profane_count"`
	Languages    []string    `json:"languages"`
	// ElapsedTime covers the whole batch.
	ElapsedTime   string `json:"elapsed_time"`
	ElapsedTimeNs int64  `json:"elapsed_time_ns"`
}

// LanguagesResponse is the body returned by GET /v1/languages.
type LanguagesResponse struct {
	Languages []filter.Language `json:"languages"`
	Default   []string          `json:"default"`
}

// ErrorResponse is returned for every non-2xx answer.
type ErrorResponse struct {
	Error string `json:"error"`
}

// HealthResponse is the body returned by GET /health.
type HealthResponse struct {
	Status    string   `json:"status"`
	Languages []string `json:"languages"`
	Words     int      `json:"words"`
	TrieNodes int      `json:"trie_nodes"`
}

// Config holds the settings of the HTTP layer.
type Config struct {
	// Defaults is the language set used when a request does not choose.
	Defaults filter.LangSet
	// AdminToken protects /v1/admin/*. Empty disables those routes.
	AdminToken string
}

// Handler holds the dependencies of the HTTP layer.
type Handler struct {
	svc *moderation.Service
	cfg Config
	log *slog.Logger
}

// NewRouter wires every route and middleware into a single http.Handler.
func NewRouter(svc *moderation.Service, cfg Config, log *slog.Logger) http.Handler {
	h := &Handler{svc: svc, cfg: cfg, log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/filter", h.handleFilter)
	mux.HandleFunc("POST /v1/filter/batch", h.handleBatch)
	mux.HandleFunc("GET /v1/languages", h.handleLanguages)
	mux.HandleFunc("POST /v1/reports", h.handleReport)
	mux.HandleFunc("GET /health", h.handleHealth)

	// Playground: a page to try the filter in the browser.
	mux.Handle("GET /{$}", web.Page())
	mux.Handle("GET /static/", web.Static())

	mux.HandleFunc("GET /v1/admin/reports", h.admin(h.handleAdminReports))
	mux.HandleFunc("GET /v1/admin/words", h.admin(h.handleListWords))
	mux.HandleFunc("POST /v1/admin/words", h.admin(h.handleAddWords))
	mux.HandleFunc("POST /v1/admin/words/{language}/{word}/quarantine", h.admin(h.handleQuarantine))
	mux.HandleFunc("POST /v1/admin/words/{language}/{word}/restore", h.admin(h.handleRestore))
	mux.HandleFunc("DELETE /v1/admin/words/{language}/{word}", h.admin(h.handleRemove))

	return recoverer(log, mux)
}

func (h *Handler) handleFilter(w http.ResponseWriter, r *http.Request) {
	var req FilterRequest
	if !decodeJSON(w, r, MaxBodyBytes, &req) {
		return
	}
	if req.Text == nil {
		writeError(w, http.StatusBadRequest, `o campo "text" é obrigatório`)
		return
	}

	f := h.svc.Filter() // one snapshot for the whole request
	langs, ok := h.resolveLanguages(w, f, req.Languages)
	if !ok {
		return
	}
	m, ok := parseMask(w, req.MaskOptions)
	if !ok {
		return
	}

	// Only the algorithm is timed: normalization, Trie scan and masking.
	// JSON decoding and network I/O are deliberately left out.
	sw := clock.Start()
	res := f.CensorWith(*req.Text, langs, m)
	elapsed := sw.Elapsed()

	setServerTiming(w, elapsed)
	writeJSON(w, http.StatusOK, FilterResponse{
		HasProfanity:      res.HasProfanity,
		CensoredText:      res.Censored,
		Matches:           matchViews(f, *req.Text, res.Matches),
		Languages:         f.Codes(langs),
		DetectedLanguages: f.Codes(res.Detected),
		ElapsedTime:       elapsed.String(),
		ElapsedTimeNs:     elapsed.Nanoseconds(),
	})
}

// handleBatch filters up to MaxBatchTexts texts in one call. Texts are
// processed sequentially: each one takes a few microseconds, so spawning
// goroutines would cost more than it saves.
func (h *Handler) handleBatch(w http.ResponseWriter, r *http.Request) {
	var req BatchRequest
	if !decodeJSON(w, r, MaxBatchBodyBytes, &req) {
		return
	}
	switch {
	case req.Texts == nil:
		writeError(w, http.StatusBadRequest, `o campo "texts" é obrigatório`)
		return
	case len(req.Texts) == 0:
		writeError(w, http.StatusBadRequest, `"texts" não pode ser uma lista vazia`)
		return
	case len(req.Texts) > MaxBatchTexts:
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf(`"texts" aceita no máximo %d itens (recebido: %d)`, MaxBatchTexts, len(req.Texts)))
		return
	}
	for i, t := range req.Texts {
		if len(t) > MaxTextBytes {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("texts[%d] excede o limite de %d bytes", i, MaxTextBytes))
			return
		}
	}

	// A single snapshot for the whole batch: even if an admin changes the
	// dictionary mid-request, every text is judged by the same rules.
	f := h.svc.Filter()
	langs, ok := h.resolveLanguages(w, f, req.Languages)
	if !ok {
		return
	}
	m, ok := parseMask(w, req.MaskOptions)
	if !ok {
		return
	}

	// The timed loop only runs the algorithm; building the JSON views
	// (language codes, character offsets) happens after the clock stops.
	results := make([]filter.Result, len(req.Texts))
	sw := clock.Start()
	for i, t := range req.Texts {
		results[i] = f.CensorWith(t, langs, m)
	}
	elapsed := sw.Elapsed()

	resp := BatchResponse{
		Results:   make([]BatchItem, len(req.Texts)),
		Count:     len(req.Texts),
		Languages: f.Codes(langs),
	}
	for i, res := range results {
		resp.Results[i] = BatchItem{
			HasProfanity:      res.HasProfanity,
			CensoredText:      res.Censored,
			Matches:           matchViews(f, req.Texts[i], res.Matches),
			DetectedLanguages: f.Codes(res.Detected),
		}
		if res.HasProfanity {
			resp.ProfaneCount++
		}
	}
	resp.ElapsedTime = elapsed.String()
	resp.ElapsedTimeNs = elapsed.Nanoseconds()

	setServerTiming(w, elapsed)
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) handleLanguages(w http.ResponseWriter, _ *http.Request) {
	f := h.svc.Filter()
	writeJSON(w, http.StatusOK, LanguagesResponse{
		Languages: f.Languages(),
		Default:   f.Codes(h.cfg.Defaults),
	})
}

func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	f := h.svc.Filter()
	writeJSON(w, http.StatusOK, HealthResponse{
		Status:    "ok",
		Languages: f.Codes(f.All()),
		Words:     f.Words(),
		TrieNodes: f.Nodes(),
	})
}

// resolveLanguages returns the request's language set, or the server default
// when the request does not choose. It writes a 400 and returns false on
// invalid input.
func (h *Handler) resolveLanguages(w http.ResponseWriter, f *filter.Filter, codes []string) (filter.LangSet, bool) {
	if codes == nil {
		return h.cfg.Defaults, true
	}
	if len(codes) == 0 {
		writeError(w, http.StatusBadRequest, `"languages" não pode ser uma lista vazia`)
		return 0, false
	}
	langs, err := f.Resolve(codes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return 0, false
	}
	return langs, true
}

// parseMask validates the mask options. It writes a 400 and returns false on
// invalid input.
func parseMask(w http.ResponseWriter, o MaskOptions) (filter.Mask, bool) {
	var m filter.Mask
	switch o.Mask {
	case "", "full":
	case "first":
		m.KeepFirst = true
	default:
		writeError(w, http.StatusBadRequest, `"mask" deve ser "full" ou "first"`)
		return m, false
	}
	if o.MaskChar != "" {
		r, size := utf8.DecodeRuneInString(o.MaskChar)
		if size != len(o.MaskChar) || r == utf8.RuneError || unicode.IsSpace(r) || unicode.IsControl(r) {
			writeError(w, http.StatusBadRequest, `"mask_char" deve ser exatamente um caractere visível`)
			return m, false
		}
		m.Char = r
	}
	return m, true
}

// matchViews converts byte offsets into character offsets in one forward
// pass, so the whole conversion is O(len(text)) even with many matches.
func matchViews(f *filter.Filter, text string, matches []filter.Match) []MatchView {
	out := make([]MatchView, len(matches))
	pos, chars := 0, 0
	for i, m := range matches {
		chars += utf8.RuneCountInString(text[pos:m.Start])
		start := chars
		chars += utf8.RuneCountInString(text[m.Start:m.End])
		pos = m.End
		out[i] = MatchView{
			Text:      text[m.Start:m.End],
			Word:      m.Word,
			Start:     start,
			End:       chars,
			Languages: f.Codes(m.Langs),
		}
	}
	return out
}

// decodeJSON reads a JSON body of at most limit bytes into v. It writes the
// error response itself and returns false when the body is invalid.
func decodeJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	if err := dec.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("corpo excede o limite de %d bytes", limit))
			return false
		}
		writeError(w, http.StatusBadRequest, "JSON inválido")
		return false
	}
	return true
}

// setServerTiming adds a header shown in the browser DevTools "Timing" tab.
func setServerTiming(w http.ResponseWriter, d time.Duration) {
	w.Header().Set("Server-Timing", fmt.Sprintf("filter;dur=%.4f", float64(d.Nanoseconds())/1e6))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg})
}

// recoverer turns a panic in any handler into a 500 instead of a dropped
// connection, and logs it.
func recoverer(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Error("panic serving request", "path", r.URL.Path, "panic", v)
				writeError(w, http.StatusInternalServerError, "erro interno")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
