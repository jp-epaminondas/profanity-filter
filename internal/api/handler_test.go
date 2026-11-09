package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/jp-epaminondas/language-filter/internal/filter"
	"github.com/jp-epaminondas/language-filter/internal/moderation"
	"github.com/jp-epaminondas/language-filter/internal/wordlist"
)

const testToken = "s3cret"

var testDicts = []wordlist.Dictionary{
	{Lang: "pt-br", Words: []string{"merda", "porra", "cu", "vaca"}},
	{Lang: "en", Words: []string{"shit"}},
}

// newTestServer starts the API with pt-br as the only default language and
// admin routes enabled.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newTestServerWithToken(t, testToken)
}

func newTestServerWithToken(t *testing.T, token string) *httptest.Server {
	t.Helper()
	svc, err := moderation.New(testDicts, "") // in memory
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := svc.Filter().Resolve([]string{"pt-br"})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewRouter(svc, Config{Defaults: defaults, AdminToken: token}, log))
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func post(t *testing.T, srv *httptest.Server, body string) *http.Response {
	t.Helper()
	return do(t, srv, http.MethodPost, "/v1/filter", "", body)
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func expectStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d (body: %s)", resp.StatusCode, want, body)
	}
}

// ---------------------------------------------------------------------------
// POST /v1/filter
// ---------------------------------------------------------------------------

func TestFilterEndpoint(t *testing.T) {
	srv := newTestServer(t)
	resp := post(t, srv, `{"text": "que m3rd4 é essa?"}`)
	expectStatus(t, resp, http.StatusOK)

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	if resp.Header.Get("Server-Timing") == "" {
		t.Error("missing Server-Timing header")
	}

	got := decode[FilterResponse](t, resp)
	if !got.HasProfanity {
		t.Error("has_profanity = false")
	}
	if got.CensoredText != "que ***** é essa?" {
		t.Errorf("censored_text = %q", got.CensoredText)
	}
	if !reflect.DeepEqual(got.Languages, []string{"pt-br"}) {
		t.Errorf("languages = %v, want the server default [pt-br]", got.Languages)
	}
	if !reflect.DeepEqual(got.DetectedLanguages, []string{"pt-br"}) {
		t.Errorf("detected_languages = %v", got.DetectedLanguages)
	}
	if got.ElapsedTime == "" || got.ElapsedTimeNs <= 0 {
		t.Errorf("elapsed_time not set: %+v", got)
	}
}

func TestFilterEndpointLanguages(t *testing.T) {
	srv := newTestServer(t)

	tests := []struct {
		name     string
		body     string
		censored string
		detected []string
	}{
		{"default (pt-br only)", `{"text": "cu later, shit"}`, "** later, shit", []string{"pt-br"}},
		{"english only", `{"text": "cu later, shit", "languages": ["en"]}`, "cu later, ****", []string{"en"}},
		{"both, any casing", `{"text": "cu later, shit", "languages": ["PT-BR", "en"]}`, "** later, ****", []string{"en", "pt-br"}},
		{"clean", `{"text": "tudo certo", "languages": ["en"]}`, "tudo certo", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := post(t, srv, tt.body)
			expectStatus(t, resp, http.StatusOK)
			got := decode[FilterResponse](t, resp)
			if got.CensoredText != tt.censored {
				t.Errorf("censored_text = %q, want %q", got.CensoredText, tt.censored)
			}
			if !reflect.DeepEqual(got.DetectedLanguages, tt.detected) {
				t.Errorf("detected_languages = %#v, want %#v", got.DetectedLanguages, tt.detected)
			}
		})
	}
}

func TestFilterEndpointErrors(t *testing.T) {
	srv := newTestServer(t)

	tests := []struct {
		name   string
		body   string
		status int
	}{
		{"invalid json", `{"text": `, http.StatusBadRequest},
		{"missing text", `{}`, http.StatusBadRequest},
		{"wrong type", `{"text": 42}`, http.StatusBadRequest},
		{"unknown language", `{"text": "oi", "languages": ["klingon"]}`, http.StatusBadRequest},
		{"empty languages", `{"text": "oi", "languages": []}`, http.StatusBadRequest},
		{"too large", `{"text": "` + strings.Repeat("a", MaxBodyBytes) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := post(t, srv, tt.body)
			expectStatus(t, resp, tt.status)
			if e := decode[ErrorResponse](t, resp); e.Error == "" {
				t.Error("expected a JSON error message")
			}
		})
	}
}

func TestMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t)
	expectStatus(t, do(t, srv, http.MethodGet, "/v1/filter", "", ""), http.StatusMethodNotAllowed)
}

func TestFilterMatches(t *testing.T) {
	srv := newTestServer(t)
	// "ção" and "é" are multi-byte: offsets must count characters, not bytes.
	resp := post(t, srv, `{"text": "Ação é uma M3RRRDA, porra"}`)
	expectStatus(t, resp, http.StatusOK)
	got := decode[FilterResponse](t, resp)

	want := []MatchView{
		{Text: "M3RRRDA", Word: "merda", Start: 11, End: 18, Languages: []string{"pt-br"}},
		{Text: "porra", Word: "porra", Start: 20, End: 25, Languages: []string{"pt-br"}},
	}
	if !reflect.DeepEqual(got.Matches, want) {
		t.Fatalf("matches = %+v\nwant      %+v", got.Matches, want)
	}
	// The offsets point at the same characters in the input and the output.
	in, out := []rune("Ação é uma M3RRRDA, porra"), []rune(got.CensoredText)
	for _, m := range got.Matches {
		if string(in[m.Start:m.End]) != m.Text || strings.Trim(string(out[m.Start:m.End]), "*") != "" {
			t.Errorf("offsets %d..%d do not line up: %q / %q", m.Start, m.End, in[m.Start:m.End], out[m.Start:m.End])
		}
	}

	clean := decode[FilterResponse](t, post(t, srv, `{"text": "tudo certo"}`))
	if clean.Matches == nil || len(clean.Matches) != 0 {
		t.Errorf("clean text must return an empty list, got %#v", clean.Matches)
	}
}

func TestFilterMaskOptions(t *testing.T) {
	srv := newTestServer(t)

	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"default":       {`{"text": "que merda"}`, "que *****"},
		"full":          {`{"text": "que merda", "mask": "full"}`, "que *****"},
		"first":         {`{"text": "que merda", "mask": "first"}`, "que m****"},
		"custom char":   {`{"text": "que merda", "mask_char": "#"}`, "que #####"},
		"unicode char":  {`{"text": "que merda", "mask_char": "█"}`, "que █████"},
		"first + char":  {`{"text": "que merda", "mask": "first", "mask_char": "-"}`, "que m----"},
		"batch + first": {`{"texts": ["que merda"], "mask": "first"}`, "que m****"},
	} {
		t.Run(name, func(t *testing.T) {
			path := "/v1/filter"
			if strings.Contains(tc.body, `"texts"`) {
				path = "/v1/filter/batch"
			}
			resp := do(t, srv, http.MethodPost, path, "", tc.body)
			expectStatus(t, resp, http.StatusOK)
			var got string
			if path == "/v1/filter" {
				got = decode[FilterResponse](t, resp).CensoredText
			} else {
				got = decode[BatchResponse](t, resp).Results[0].CensoredText
			}
			if got != tc.want {
				t.Errorf("censored_text = %q, want %q", got, tc.want)
			}
		})
	}

	for name, body := range map[string]string{
		"unknown mode":   `{"text": "x", "mask": "half"}`,
		"two chars":      `{"text": "x", "mask_char": "**"}`,
		"space char":     `{"text": "x", "mask_char": " "}`,
		"control char":   `{"text": "x", "mask_char": "\n"}`,
		"batch bad mode": `{"texts": ["x"], "mask": "half"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := "/v1/filter"
			if strings.Contains(body, `"texts"`) {
				path = "/v1/filter/batch"
			}
			expectStatus(t, do(t, srv, http.MethodPost, path, "", body), http.StatusBadRequest)
		})
	}
}

// ---------------------------------------------------------------------------
// POST /v1/filter/batch
// ---------------------------------------------------------------------------

func TestBatchEndpoint(t *testing.T) {
	srv := newTestServer(t)
	resp := do(t, srv, http.MethodPost, "/v1/filter/batch", "",
		`{"texts": ["que merda", "tudo certo", "shit happens"], "languages": ["pt-br", "en"]}`)
	expectStatus(t, resp, http.StatusOK)

	got := decode[BatchResponse](t, resp)
	want := []BatchItem{
		{HasProfanity: true, CensoredText: "que *****", DetectedLanguages: []string{"pt-br"},
			Matches: []MatchView{{Text: "merda", Word: "merda", Start: 4, End: 9, Languages: []string{"pt-br"}}}},
		{HasProfanity: false, CensoredText: "tudo certo", DetectedLanguages: []string{}, Matches: []MatchView{}},
		{HasProfanity: true, CensoredText: "**** happens", DetectedLanguages: []string{"en"},
			Matches: []MatchView{{Text: "shit", Word: "shit", Start: 0, End: 4, Languages: []string{"en"}}}},
	}
	if !reflect.DeepEqual(got.Results, want) {
		t.Errorf("results = %+v\nwant      %+v", got.Results, want)
	}
	if got.Count != 3 || got.ProfaneCount != 2 {
		t.Errorf("count = %d, profane_count = %d", got.Count, got.ProfaneCount)
	}
	if got.ElapsedTimeNs <= 0 {
		t.Error("elapsed_time not set")
	}
}

func TestBatchLimits(t *testing.T) {
	srv := newTestServer(t)

	many := `"` + strings.Repeat(`a", "`, MaxBatchTexts) + `a"` // MaxBatchTexts+1 items
	tests := []struct {
		name   string
		body   string
		status int
	}{
		{"missing texts", `{}`, http.StatusBadRequest},
		{"empty texts", `{"texts": []}`, http.StatusBadRequest},
		{"too many texts", `{"texts": [` + many + `]}`, http.StatusBadRequest},
		{"one text too big", `{"texts": ["ok", "` + strings.Repeat("a", MaxTextBytes+1) + `"]}`, http.StatusBadRequest},
		{"body too big", `{"texts": ["` + strings.Repeat("a", MaxBatchBodyBytes) + `"]}`, http.StatusRequestEntityTooLarge},
		{"unknown language", `{"texts": ["oi"], "languages": ["xx"]}`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expectStatus(t, do(t, srv, http.MethodPost, "/v1/filter/batch", "", tt.body), tt.status)
		})
	}

	// Exactly at the limit is accepted.
	exact := `"` + strings.Repeat(`a", "`, MaxBatchTexts-1) + `a"`
	expectStatus(t, do(t, srv, http.MethodPost, "/v1/filter/batch", "", `{"texts": [`+exact+`]}`), http.StatusOK)
}

// ---------------------------------------------------------------------------
// Reports and admin
// ---------------------------------------------------------------------------

func TestReportEndpoint(t *testing.T) {
	srv := newTestServer(t)

	resp := do(t, srv, http.MethodPost, "/v1/reports", "",
		`{"word": "vaca", "language": "pt-br", "reason": "é um animal"}`)
	expectStatus(t, resp, http.StatusAccepted)
	first := decode[ReportResponse](t, resp)
	if !first.Counted || first.Reports != 1 || first.Status != moderation.StatusActive {
		t.Errorf("first report = %+v", first)
	}

	// Same client again: accepted, but not counted twice.
	again := decode[ReportResponse](t, do(t, srv, http.MethodPost, "/v1/reports", "",
		`{"word": "VACA", "language": "pt-br"}`))
	if again.Counted || again.Reports != 1 {
		t.Errorf("duplicate report = %+v", again)
	}

	for name, tc := range map[string]struct {
		body   string
		status int
	}{
		"missing word":     {`{"language": "pt-br"}`, http.StatusBadRequest},
		"unknown language": {`{"word": "vaca", "language": "xx"}`, http.StatusBadRequest},
		"not a dictionary": {`{"word": "abacaxi", "language": "pt-br"}`, http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			expectStatus(t, do(t, srv, http.MethodPost, "/v1/reports", "", tc.body), tc.status)
		})
	}
}

func TestAdminAuth(t *testing.T) {
	srv := newTestServer(t)
	expectStatus(t, do(t, srv, http.MethodGet, "/v1/admin/reports", "", ""), http.StatusUnauthorized)
	expectStatus(t, do(t, srv, http.MethodGet, "/v1/admin/reports", "wrong", ""), http.StatusUnauthorized)
	expectStatus(t, do(t, srv, http.MethodGet, "/v1/admin/reports", testToken, ""), http.StatusOK)

	disabled := newTestServerWithToken(t, "")
	expectStatus(t, do(t, disabled, http.MethodGet, "/v1/admin/reports", "anything", ""), http.StatusForbidden)
}

func TestModerationFlow(t *testing.T) {
	srv := newTestServer(t)
	censored := func() string {
		return decode[FilterResponse](t, post(t, srv, `{"text": "que vaca"}`)).CensoredText
	}
	if got := censored(); got != "que ****" {
		t.Fatalf("before: %q", got)
	}

	// 1. A user complains; the admin sees it in the queue.
	do(t, srv, http.MethodPost, "/v1/reports", "", `{"word": "vaca", "language": "pt-br"}`)
	queue := decode[ReportsResponse](t, do(t, srv, http.MethodGet, "/v1/admin/reports", testToken, ""))
	if len(queue.Reports) != 1 || queue.Reports[0].Word != "vaca" {
		t.Fatalf("queue = %+v", queue)
	}

	// 2. Quarantine: the word stops being censored immediately.
	resp := do(t, srv, http.MethodPost, "/v1/admin/words/pt-br/vaca/quarantine", testToken, "")
	expectStatus(t, resp, http.StatusOK)
	if got := censored(); got != "que vaca" {
		t.Errorf("after quarantine: %q", got)
	}
	words := decode[WordsResponse](t, do(t, srv, http.MethodGet, "/v1/admin/words?status=quarantined", testToken, ""))
	if words.Count != 1 || words.Words[0].Word != "vaca" {
		t.Errorf("words = %+v", words)
	}

	// 3. Restore: back to normal.
	expectStatus(t, do(t, srv, http.MethodPost, "/v1/admin/words/pt-br/vaca/restore", testToken, ""), http.StatusOK)
	if got := censored(); got != "que ****" {
		t.Errorf("after restore: %q", got)
	}

	// 4. Remove for good; a second restore of an active word is a conflict.
	expectStatus(t, do(t, srv, http.MethodDelete, "/v1/admin/words/pt-br/vaca", testToken, ""), http.StatusOK)
	if got := censored(); got != "que vaca" {
		t.Errorf("after remove: %q", got)
	}
	expectStatus(t, do(t, srv, http.MethodPost, "/v1/admin/words/pt-br/vaca/quarantine", testToken, ""), http.StatusConflict)
	expectStatus(t, do(t, srv, http.MethodPost, "/v1/reports", "", `{"word": "vaca", "language": "pt-br"}`), http.StatusConflict)

	// Unknown words are 404, not silently accepted.
	expectStatus(t, do(t, srv, http.MethodDelete, "/v1/admin/words/pt-br/abacaxi", testToken, ""), http.StatusNotFound)
}

func TestAddAndListWords(t *testing.T) {
	srv := newTestServer(t)

	resp := do(t, srv, http.MethodPost, "/v1/admin/words", testToken,
		`{"language": "pt-br", "words": ["otário", "merda", ""]}`)
	expectStatus(t, resp, http.StatusCreated)
	res := decode[moderation.AddResult](t, resp)
	if len(res.Added) != 1 || res.Added[0].Word != "otário" || len(res.Skipped) != 2 {
		t.Errorf("add result = %+v", res)
	}

	// Takes effect immediately.
	got := decode[FilterResponse](t, post(t, srv, `{"text": "seu 0t4r10"}`))
	if got.CensoredText != "seu ******" {
		t.Errorf("censored_text = %q", got.CensoredText)
	}

	// Nothing new: 200 instead of 201.
	expectStatus(t, do(t, srv, http.MethodPost, "/v1/admin/words", testToken,
		`{"language": "pt-br", "words": ["otario"]}`), http.StatusOK)

	list := decode[WordsResponse](t, do(t, srv, http.MethodGet, "/v1/admin/words?source=api", testToken, ""))
	if list.Count != 1 || list.Words[0].Source != "api" || list.Words[0].Language != "pt-br" {
		t.Errorf("source=api list = %+v", list)
	}
	all := decode[WordsResponse](t, do(t, srv, http.MethodGet, "/v1/admin/words", testToken, ""))
	if all.Count != 6 { // 4 pt-br + 1 en from testDicts + 1 added
		t.Errorf("full list count = %d, want 6", all.Count)
	}

	for name, tc := range map[string]struct {
		method, path, body string
		status             int
	}{
		"no auth":          {http.MethodPost, "/v1/admin/words", `{"language": "pt-br", "words": ["x"]}`, http.StatusUnauthorized},
		"unknown language": {http.MethodPost, "/v1/admin/words", `{"language": "xx", "words": ["x"]}`, http.StatusBadRequest},
		"missing language": {http.MethodPost, "/v1/admin/words", `{"words": ["x"]}`, http.StatusBadRequest},
		"empty words":      {http.MethodPost, "/v1/admin/words", `{"language": "pt-br", "words": []}`, http.StatusBadRequest},
		"too many words":   {http.MethodPost, "/v1/admin/words", `{"language": "pt-br", "words": [` + strings.Repeat(`"a",`, MaxAddWords) + `"a"]}`, http.StatusBadRequest},
		"bad status":       {http.MethodGet, "/v1/admin/words?status=banana", "", http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			token := testToken
			if name == "no auth" {
				token = ""
			}
			expectStatus(t, do(t, srv, tc.method, tc.path, token, tc.body), tc.status)
		})
	}
}

// ---------------------------------------------------------------------------
// GET /v1/languages, /health
// ---------------------------------------------------------------------------

func TestLanguagesEndpoint(t *testing.T) {
	srv := newTestServer(t)
	got := decode[LanguagesResponse](t, do(t, srv, http.MethodGet, "/v1/languages", "", ""))
	want := LanguagesResponse{
		Languages: []filter.Language{{Code: "pt-br", Words: 4}, {Code: "en", Words: 1}},
		Default:   []string{"pt-br"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("languages = %+v, want %+v", got, want)
	}
}

func TestPlayground(t *testing.T) {
	srv := newTestServer(t)

	page := do(t, srv, http.MethodGet, "/", "", "")
	expectStatus(t, page, http.StatusOK)
	if ct := page.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	csp := page.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP must allow only same-origin scripts, got %q", csp)
	}
	body, _ := io.ReadAll(page.Body)
	if !strings.Contains(string(body), `src="/static/app.js"`) || strings.Contains(string(body), "style=") {
		t.Error("page must load app.js and have no inline styles (the CSP would block them)")
	}

	for path, ctype := range map[string]string{
		"/static/app.js":    "javascript",
		"/static/style.css": "text/css",
	} {
		resp := do(t, srv, http.MethodGet, path, "", "")
		expectStatus(t, resp, http.StatusOK)
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, ctype) {
			t.Errorf("%s Content-Type = %q", path, ct)
		}
	}

	// Only "/" is the page: unknown paths stay 404 instead of serving HTML.
	expectStatus(t, do(t, srv, http.MethodGet, "/nope", "", ""), http.StatusNotFound)
	expectStatus(t, do(t, srv, http.MethodGet, "/static/missing.js", "", ""), http.StatusNotFound)
}

func TestHealth(t *testing.T) {
	srv := newTestServer(t)
	got := decode[HealthResponse](t, do(t, srv, http.MethodGet, "/health", "", ""))
	if got.Status != "ok" || got.Words != 5 || len(got.Languages) != 2 {
		t.Errorf("health = %+v", got)
	}
}
