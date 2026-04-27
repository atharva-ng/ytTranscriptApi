package transcriptapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateVideoID(t *testing.T) {
	cases := []struct {
		id string
		ok bool
	}{
		{"dQw4w9WgXcQ", true},
		{"abc-DEF_123", true},
		{"short", false},
		{"toolongtoolong", false},
		{"has spaces!", false},
		{"", false},
	}
	for _, c := range cases {
		err := ValidateVideoID(c.id)
		if c.ok && err != nil {
			t.Errorf("ValidateVideoID(%q) = %v, want nil", c.id, err)
		}
		if !c.ok && err == nil {
			t.Errorf("ValidateVideoID(%q) = nil, want error", c.id)
		}
	}
}

func TestFindTranscriptPrefersManual(t *testing.T) {
	manual := &Transcript{LanguageCode: "en", IsGenerated: false}
	auto := &Transcript{LanguageCode: "en", IsGenerated: true}
	list := &TranscriptList{
		Manual:    map[string]*Transcript{"en": manual},
		Generated: map[string]*Transcript{"en": auto},
	}
	got, err := list.FindTranscript([]string{"en"})
	if err != nil {
		t.Fatal(err)
	}
	if got != manual {
		t.Errorf("expected manual transcript, got generated=%v", got.IsGenerated)
	}
}

func TestFindTranscriptLanguagePriority(t *testing.T) {
	en := &Transcript{LanguageCode: "en"}
	es := &Transcript{LanguageCode: "es"}
	list := &TranscriptList{
		Manual:    map[string]*Transcript{"en": en, "es": es},
		Generated: map[string]*Transcript{},
	}
	got, _ := list.FindTranscript([]string{"de", "es", "en"})
	if got != es {
		t.Errorf("expected es, got %v", got.LanguageCode)
	}
}

func TestFindTranscriptFallsBackToGenerated(t *testing.T) {
	auto := &Transcript{LanguageCode: "en", IsGenerated: true}
	list := &TranscriptList{
		Manual:    map[string]*Transcript{},
		Generated: map[string]*Transcript{"en": auto},
	}
	got, err := list.FindTranscript([]string{"en"})
	if err != nil || got != auto {
		t.Errorf("expected auto transcript, got=%v err=%v", got, err)
	}
}

func TestFindTranscriptNoMatch(t *testing.T) {
	list := &TranscriptList{
		Manual:    map[string]*Transcript{"en": {LanguageCode: "en"}},
		Generated: map[string]*Transcript{},
	}
	_, err := list.FindTranscript([]string{"de"})
	if !errors.Is(err, ErrNoTranscriptFound) {
		t.Errorf("expected ErrNoTranscriptFound, got %v", err)
	}
}

func TestTranslateNotTranslatable(t *testing.T) {
	tr := &Transcript{Translatable: false, LanguageCode: "en"}
	_, err := tr.Translate("es")
	if !errors.Is(err, ErrNotTranslatable) {
		t.Errorf("expected ErrNotTranslatable, got %v", err)
	}
}

func TestTranslateAppendsTlang(t *testing.T) {
	tr := &Transcript{Translatable: true, LanguageCode: "en", URL: "https://example/timedtext?v=abc"}
	got, err := tr.Translate("es")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.URL, "tlang=es") {
		t.Errorf("expected tlang=es in URL, got %q", got.URL)
	}
	if got.LanguageCode != "es" {
		t.Errorf("expected LanguageCode=es, got %q", got.LanguageCode)
	}
}

func TestAppendQuery(t *testing.T) {
	cases := []struct{ in, key, val, want string }{
		{"http://x", "a", "1", "http://x?a=1"},
		{"http://x?b=2", "a", "1", "http://x?b=2&a=1"},
	}
	for _, c := range cases {
		got := appendQuery(c.in, c.key, c.val)
		if got != c.want {
			t.Errorf("appendQuery(%q,%q,%q) = %q, want %q", c.in, c.key, c.val, got, c.want)
		}
	}
}

func TestBuildTranscriptListSplitsKindsAndDedupes(t *testing.T) {
	pr := &playerResponse{}
	tracks := &pr.Captions.PlayerCaptionsTracklistRenderer.CaptionTracks
	add := func(lang, kind, name string) {
		var entry struct {
			BaseURL string `json:"baseUrl"`
			Name    struct {
				Runs []struct {
					Text string `json:"text"`
				} `json:"runs"`
			} `json:"name"`
			LanguageCode   string `json:"languageCode"`
			Kind           string `json:"kind"`
			IsTranslatable bool   `json:"isTranslatable"`
		}
		entry.BaseURL = "https://t/" + lang + "?fmt=srv3" // prefix without &
		entry.LanguageCode = lang
		entry.Kind = kind
		entry.IsTranslatable = true
		entry.Name.Runs = append(entry.Name.Runs, struct {
			Text string `json:"text"`
		}{Text: name})
		*tracks = append(*tracks, entry)
	}
	add("en", "", "English")
	add("en", "", "English (duplicate)") // should be ignored by Manual map
	add("es", "asr", "Spanish (auto)")

	a := &API{}
	list := a.buildTranscriptList("vidvidvidvi", pr)

	if len(list.All) != 3 {
		t.Errorf("expected 3 in All, got %d", len(list.All))
	}
	if list.Manual["en"].Language != "English" {
		t.Errorf("expected first English to win, got %q", list.Manual["en"].Language)
	}
	if _, ok := list.Generated["es"]; !ok {
		t.Errorf("expected Spanish auto-track in Generated")
	}
	if _, ok := list.Manual["es"]; ok {
		t.Errorf("did not expect Spanish auto-track in Manual")
	}
}

func TestFetchHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := &API{}
	_, err := a.Fetch(ctx, "dQw4w9WgXcQ")
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestFetchRejectsBadVideoID(t *testing.T) {
	a := &API{}
	_, err := a.Fetch(context.Background(), "not-an-id")
	if !errors.Is(err, ErrInvalidVideoID) {
		t.Errorf("expected ErrInvalidVideoID, got %v", err)
	}
}

func TestPlayabilityErrorBecomesVideoUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"playabilityStatus": map[string]any{
				"status": "ERROR",
				"reason": "Video unavailable",
			},
		})
	}))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader("{}"))
	a := &API{HTTPClient: srv.Client(), MaxRetries: 1}
	body, err := a.doRequest(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var pr playerResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		t.Fatal(err)
	}
	if pr.PlayabilityStatus.Status != "ERROR" {
		t.Errorf("expected ERROR status, got %q", pr.PlayabilityStatus.Status)
	}
}

func TestDoRequestRetriesOn5xx(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	a := &API{HTTPClient: srv.Client(), MaxRetries: 4}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	body, err := a.doRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if string(body) != "ok" {
		t.Errorf("got %q, want %q", body, "ok")
	}
	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

func TestDoRequestDoesNotRetry429(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	a := &API{HTTPClient: srv.Client(), MaxRetries: 4}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	_, err := a.doRequest(context.Background(), req)
	if !errors.Is(err, ErrIPBlocked) {
		t.Errorf("expected ErrIPBlocked, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 call (no retry on 429), got %d", calls)
	}
}
