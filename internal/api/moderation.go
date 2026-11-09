package api

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/jp-epaminondas/language-filter/internal/moderation"
)

// MaxReportBodyBytes limits POST /v1/reports.
const MaxReportBodyBytes = 4 << 10 // 4 KiB

// Limits of POST /v1/admin/words.
const (
	MaxAddWords        = 100
	MaxAddWordsBodyKiB = 16
)

// ReportRequest is the body of POST /v1/reports: a user claims that a
// censored word is not profanity.
type ReportRequest struct {
	Word     string `json:"word"`
	Language string `json:"language"`
	Reason   string `json:"reason,omitempty"`
}

// ReportResponse is returned by POST /v1/reports.
type ReportResponse struct {
	moderation.Report
	// Counted is false when this client had already reported the word:
	// each client counts once per word.
	Counted bool `json:"counted"`
}

// ReportsResponse is returned by GET /v1/admin/reports.
type ReportsResponse struct {
	Reports []moderation.Report `json:"reports"`
}

// WordsResponse is returned by GET /v1/admin/words.
type WordsResponse struct {
	Count int                `json:"count"`
	Words []moderation.Entry `json:"words"`
}

// AddWordsRequest is the body of POST /v1/admin/words.
type AddWordsRequest struct {
	Language string   `json:"language"`
	Words    []string `json:"words"`
}

func (h *Handler) handleReport(w http.ResponseWriter, r *http.Request) {
	var req ReportRequest
	if !decodeJSON(w, r, MaxReportBodyBytes, &req) {
		return
	}
	if strings.TrimSpace(req.Word) == "" || strings.TrimSpace(req.Language) == "" {
		writeError(w, http.StatusBadRequest, `os campos "word" e "language" são obrigatórios`)
		return
	}

	rep, counted, err := h.svc.Report(req.Language, req.Word, clientIP(r), req.Reason)
	if err != nil {
		h.writeModerationError(w, err)
		return
	}
	// 202: the complaint was queued for review, nothing changed yet.
	writeJSON(w, http.StatusAccepted, ReportResponse{Report: rep, Counted: counted})
}

func (h *Handler) handleAdminReports(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, ReportsResponse{Reports: h.svc.Reports()})
}

// handleListWords lists the dictionary words with their source and status.
// Query string filters: language, status, source (file name or "api") and
// q (substring, matched against the normalized word).
func (h *Handler) handleListWords(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	entries, err := h.svc.Entries(moderation.EntryFilter{
		Language: q.Get("language"),
		Status:   moderation.Status(q.Get("status")),
		Source:   q.Get("source"),
		Query:    q.Get("q"),
	})
	if err != nil {
		h.writeModerationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, WordsResponse{Count: len(entries), Words: entries})
}

// handleAddWords registers new words. Each word is accepted or skipped on
// its own; the answer is 201 when at least one word was added, 200 otherwise.
func (h *Handler) handleAddWords(w http.ResponseWriter, r *http.Request) {
	var req AddWordsRequest
	if !decodeJSON(w, r, MaxAddWordsBodyKiB<<10, &req) {
		return
	}
	switch {
	case strings.TrimSpace(req.Language) == "":
		writeError(w, http.StatusBadRequest, `o campo "language" é obrigatório`)
		return
	case len(req.Words) == 0:
		writeError(w, http.StatusBadRequest, `"words" deve ter ao menos uma palavra`)
		return
	case len(req.Words) > MaxAddWords:
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf(`"words" aceita no máximo %d itens (recebido: %d)`, MaxAddWords, len(req.Words)))
		return
	}

	res, err := h.svc.Add(req.Language, req.Words)
	if err != nil {
		h.writeModerationError(w, err)
		return
	}
	for _, e := range res.Added {
		h.log.Info("admin action", "action", "add", "lang", e.Language, "word", e.Word, "ip", clientIP(r))
	}
	status := http.StatusOK
	if len(res.Added) > 0 {
		status = http.StatusCreated
	}
	writeJSON(w, status, res)
}

func (h *Handler) handleQuarantine(w http.ResponseWriter, r *http.Request) {
	h.adminAction(w, r, "quarantine", h.svc.Quarantine)
}

func (h *Handler) handleRestore(w http.ResponseWriter, r *http.Request) {
	h.adminAction(w, r, "restore", h.svc.Restore)
}

func (h *Handler) handleRemove(w http.ResponseWriter, r *http.Request) {
	h.adminAction(w, r, "remove", h.svc.Remove)
}

func (h *Handler) adminAction(w http.ResponseWriter, r *http.Request, action string,
	fn func(lang, word string) (moderation.WordState, error)) {
	lang, word := r.PathValue("language"), r.PathValue("word")
	st, err := fn(lang, word)
	if err != nil {
		h.writeModerationError(w, err)
		return
	}
	// Audit trail: every dictionary change is logged with its origin.
	h.log.Info("admin action",
		"action", action, "lang", st.Language, "word", st.Word, "status", st.Status, "ip", clientIP(r))
	writeJSON(w, http.StatusOK, st)
}

// admin protects a route with the ADMIN_TOKEN bearer token. The comparison
// runs in constant time, so response timing does not leak the token.
func (h *Handler) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.AdminToken == "" {
			writeError(w, http.StatusForbidden, "rotas de admin desabilitadas: defina ADMIN_TOKEN")
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(h.cfg.AdminToken)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
			writeError(w, http.StatusUnauthorized, "token de admin ausente ou inválido")
			return
		}
		next(w, r)
	}
}

func (h *Handler) writeModerationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, moderation.ErrUnknownLanguage),
		errors.Is(err, moderation.ErrInvalidStatus):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, moderation.ErrWordNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, moderation.ErrNotActive),
		errors.Is(err, moderation.ErrAlreadyRemoved),
		errors.Is(err, moderation.ErrAlreadyActive):
		writeError(w, http.StatusConflict, err.Error())
	default:
		h.log.Error("moderation failed", "err", err)
		writeError(w, http.StatusInternalServerError, "erro interno ao salvar a moderação")
	}
}

// clientIP identifies the caller for the one-report-per-client rule.
// Behind a reverse proxy, RemoteAddr is the proxy: in that setup this should
// read X-Forwarded-For from a trusted proxy instead.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
