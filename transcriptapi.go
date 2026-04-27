// Package transcriptapi fetches YouTube video transcripts by reverse-engineering
// the InnerTube player API. It supports human-written and auto-generated captions,
// language fallback, and on-the-fly translation.
//
// The zero value of API is usable. All public methods accept a context.Context
// and respect its deadline and cancellation.
package transcriptapi

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Tunable defaults; can be overridden per-API instance.
const (
	DefaultMaxResponseBytes = 10 << 20 // 10 MiB
	DefaultMaxRetries       = 3
	DefaultRequestTimeout   = 30 * time.Second
)

const (
	defaultDesktopUA            = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	defaultAndroidUA            = "com.google.android.youtube/20.10.38 (Linux; U; Android 13) gzip"
	defaultAndroidClientVersion = "20.10.38"
)

const (
	watchURL        = "https://www.youtube.com/watch?v=%s"
	innertubeAPIURL = "https://www.youtube.com/youtubei/v1/player?key=%s"
)

var (
	ErrInvalidVideoID      = errors.New("invalid YouTube video ID")
	ErrTranscriptsDisabled = errors.New("transcripts are disabled for this video")
	ErrNoTranscriptFound   = errors.New("no transcript found for the requested languages")
	ErrIPBlocked           = errors.New("YouTube is blocking requests from this IP")
	ErrVideoUnavailable    = errors.New("video unavailable")
	ErrPoTokenRequired     = errors.New("PO token required for this transcript")
	ErrNotTranslatable     = errors.New("transcript is not translatable")
	ErrResponseTooLarge    = errors.New("response exceeded maximum size")
)

var (
	videoIDRegex = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	apiKeyRegex  = regexp.MustCompile(`"INNERTUBE_API_KEY":\s*"([a-zA-Z0-9_-]+)"`)
	htmlTagRegex = regexp.MustCompile(`<[^>]*>`)

	recaptchaToken = []byte(`class="g-recaptcha"`)

	defaultHTTPClient = &http.Client{Timeout: DefaultRequestTimeout}
)

// Logger is an optional sink for debug events. *log.Logger satisfies it.
type Logger interface {
	Printf(format string, args ...any)
}

// Snippet is one timed line of a transcript.
type Snippet struct {
	Text     string  `json:"text"`
	Start    float64 `json:"start"`
	Duration float64 `json:"duration"`
}

// FetchedTranscript is a transcript that has been downloaded and parsed.
type FetchedTranscript struct {
	Snippets     []Snippet
	VideoID      string
	Language     string
	LanguageCode string
	IsGenerated  bool
}

// Transcript is a handle to a single transcript track. Call Fetch to download.
type Transcript struct {
	api          *API
	VideoID      string
	URL          string
	Language     string
	LanguageCode string
	IsGenerated  bool
	Translatable bool
}

// TranscriptList groups all available transcripts for a video. Manual and
// Generated are keyed by language code; All preserves YouTube's original order
// and contains every track including duplicates.
type TranscriptList struct {
	VideoID   string
	Manual    map[string]*Transcript
	Generated map[string]*Transcript
	All       []*Transcript
}

// API is the entry point. Zero value is usable.
type API struct {
	HTTPClient    *http.Client // shared client; defaults to a 30s-timeout client
	UserAgent     string       // overrides desktop UA used for the watch-page scrape
	AndroidUA     string       // overrides Android UA used for the InnerTube call
	ClientVersion string       // overrides the Android clientVersion (bump when YouTube rotates)
	POToken       string       // optional; appended as &pot= when a track requires it
	Logger        Logger       // optional debug logger
	MaxRetries    int          // 0 → DefaultMaxRetries; negative → no retry
	Proxy         ProxyConfig  // optional; routes requests through an HTTP proxy, with block-retries

	proxyClientOnce sync.Once
	proxyClient     *http.Client
}

func (a *API) httpClient() *http.Client {
	if a.HTTPClient != nil {
		return a.HTTPClient
	}
	if a.Proxy != nil {
		a.proxyClientOnce.Do(a.initProxyClient)
		return a.proxyClient
	}
	return defaultHTTPClient
}

func (a *API) initProxyClient() {
	var tr *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = base.Clone()
	} else {
		tr = &http.Transport{}
	}
	tr.Proxy = a.Proxy.ProxyURL
	tr.DisableKeepAlives = a.Proxy.PreventKeepAlives()
	a.proxyClient = &http.Client{
		Transport: tr,
		Timeout:   DefaultRequestTimeout,
	}
}

func (a *API) blockAttempts() int {
	if a.Proxy == nil {
		return 1
	}
	n := a.Proxy.RetriesWhenBlocked()
	if n < 0 {
		n = 0
	}
	return n + 1
}

// retryOnBlock invokes op, retrying when it returns ErrIPBlocked, up to the
// limit defined by the configured ProxyConfig. Without a proxy, op runs once.
func (a *API) retryOnBlock(ctx context.Context, op func() error) error {
	attempts := a.blockAttempts()
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			backoff := time.Duration(1<<uint(i-1)) * 250 * time.Millisecond
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		err := op()
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrIPBlocked) {
			return err
		}
		lastErr = err
		a.logf("transcriptapi: IP blocked on attempt %d/%d, retrying", i+1, attempts)
	}
	return lastErr
}

func (a *API) desktopUA() string {
	if a.UserAgent != "" {
		return a.UserAgent
	}
	return defaultDesktopUA
}

func (a *API) androidUA() string {
	if a.AndroidUA != "" {
		return a.AndroidUA
	}
	return defaultAndroidUA
}

func (a *API) clientVersion() string {
	if a.ClientVersion != "" {
		return a.ClientVersion
	}
	return defaultAndroidClientVersion
}

func (a *API) maxAttempts() int {
	if a.MaxRetries < 0 {
		return 1
	}
	if a.MaxRetries == 0 {
		return DefaultMaxRetries
	}
	return a.MaxRetries
}

func (a *API) logf(format string, args ...any) {
	if a.Logger != nil {
		a.Logger.Printf(format, args...)
	}
}

// ValidateVideoID reports whether id is a syntactically valid YouTube video ID.
func ValidateVideoID(id string) error {
	if !videoIDRegex.MatchString(id) {
		return fmt.Errorf("%w: %q", ErrInvalidVideoID, id)
	}
	return nil
}

// Fetch is a shortcut for List + FindTranscript + Fetch.
func (a *API) Fetch(ctx context.Context, videoID string, languages ...string) (*FetchedTranscript, error) {
	if len(languages) == 0 {
		languages = []string{"en"}
	}
	list, err := a.List(ctx, videoID)
	if err != nil {
		return nil, err
	}
	t, err := list.FindTranscript(languages)
	if err != nil {
		return nil, err
	}
	return t.Fetch(ctx)
}

// List retrieves the available transcripts for a video.
func (a *API) List(ctx context.Context, videoID string) (*TranscriptList, error) {
	if err := ValidateVideoID(videoID); err != nil {
		return nil, err
	}
	var list *TranscriptList
	err := a.retryOnBlock(ctx, func() error {
		apiKey, err := a.fetchInnertubeAPIKey(ctx, videoID)
		if err != nil {
			return err
		}
		pr, err := a.fetchCaptionsJSON(ctx, videoID, apiKey)
		if err != nil {
			return err
		}
		list = a.buildTranscriptList(videoID, pr)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return list, nil
}

// doRequest sends req with retries on network errors and 5xx. The request body,
// if any, must be repeatable — http.NewRequestWithContext sets GetBody for the
// common readers (bytes.Reader, strings.Reader, bytes.Buffer).
func (a *API) doRequest(ctx context.Context, req *http.Request) ([]byte, error) {
	attempts := a.maxAttempts()
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * 250 * time.Millisecond
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			if req.GetBody != nil {
				body, err := req.GetBody()
				if err != nil {
					return nil, err
				}
				req.Body = body
			}
		}
		body, retry, err := a.doOnce(req)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !retry {
			return nil, err
		}
		a.logf("transcriptapi: %s %s attempt %d/%d failed: %v", req.Method, req.URL, attempt+1, attempts, err)
	}
	return nil, lastErr
}

func (a *API) doOnce(req *http.Request) (body []byte, retry bool, err error) {
	resp, err := a.httpClient().Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, false, err
		}
		return nil, true, err
	}
	defer resp.Body.Close()

	body, err = io.ReadAll(io.LimitReader(resp.Body, DefaultMaxResponseBytes+1))
	if err != nil {
		return nil, true, err
	}
	if int64(len(body)) > DefaultMaxResponseBytes {
		return nil, false, fmt.Errorf("%w: %s %s", ErrResponseTooLarge, req.Method, req.URL)
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, false, ErrIPBlocked
	case resp.StatusCode >= 500:
		return nil, true, fmt.Errorf("%s %s: status %d", req.Method, req.URL, resp.StatusCode)
	case resp.StatusCode >= 400:
		return nil, false, fmt.Errorf("%s %s: status %d", req.Method, req.URL, resp.StatusCode)
	}
	return body, false, nil
}

func (a *API) get(ctx context.Context, url, userAgent string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Language", "en-US")
	req.Header.Set("User-Agent", userAgent)
	return a.doRequest(ctx, req)
}

func (a *API) fetchInnertubeAPIKey(ctx context.Context, videoID string) (string, error) {
	body, err := a.get(ctx, fmt.Sprintf(watchURL, videoID), a.desktopUA())
	if err != nil {
		return "", err
	}
	m := apiKeyRegex.FindSubmatch(body)
	if len(m) != 2 {
		if bytes.Contains(body, recaptchaToken) {
			return "", ErrIPBlocked
		}
		return "", errors.New("could not extract INNERTUBE_API_KEY from watch page")
	}
	return string(m[1]), nil
}

type playerResponse struct {
	PlayabilityStatus struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	} `json:"playabilityStatus"`
	Captions struct {
		PlayerCaptionsTracklistRenderer struct {
			CaptionTracks []struct {
				BaseURL string `json:"baseUrl"`
				Name    struct {
					Runs []struct {
						Text string `json:"text"`
					} `json:"runs"`
				} `json:"name"`
				LanguageCode   string `json:"languageCode"`
				Kind           string `json:"kind"`
				IsTranslatable bool   `json:"isTranslatable"`
			} `json:"captionTracks"`
		} `json:"playerCaptionsTracklistRenderer"`
	} `json:"captions"`
}

func (a *API) fetchCaptionsJSON(ctx context.Context, videoID, apiKey string) (*playerResponse, error) {
	payload, err := json.Marshal(map[string]any{
		"context": map[string]any{
			"client": map[string]any{
				"clientName":    "WEB",
				"clientVersion": "2.20241120.01.00",
			},
		},
		"videoId": videoID,
	})
	if err != nil {
		return nil, fmt.Errorf("encode innertube payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf(innertubeAPIURL, apiKey), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Language", "en-US")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", a.desktopUA())

	body, err := a.doRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	var pr playerResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("decode player response: %w", err)
	}

	switch pr.PlayabilityStatus.Status {
	case "", "OK":
	case "ERROR":
		return nil, fmt.Errorf("%w: %s", ErrVideoUnavailable, pr.PlayabilityStatus.Reason)
	default:
		return nil, fmt.Errorf("video unplayable (%s): %s", pr.PlayabilityStatus.Status, pr.PlayabilityStatus.Reason)
	}

	if len(pr.Captions.PlayerCaptionsTracklistRenderer.CaptionTracks) == 0 {
		return nil, ErrTranscriptsDisabled
	}
	return &pr, nil
}

func (a *API) buildTranscriptList(videoID string, pr *playerResponse) *TranscriptList {
	list := &TranscriptList{
		VideoID:   videoID,
		Manual:    map[string]*Transcript{},
		Generated: map[string]*Transcript{},
	}
	for _, track := range pr.Captions.PlayerCaptionsTracklistRenderer.CaptionTracks {
		name := ""
		if len(track.Name.Runs) > 0 {
			name = track.Name.Runs[0].Text
		}
		t := &Transcript{
			api:          a,
			VideoID:      videoID,
			URL:          strings.Replace(track.BaseURL, "&fmt=srv3", "", 1),
			Language:     name,
			LanguageCode: track.LanguageCode,
			IsGenerated:  track.Kind == "asr",
			Translatable: track.IsTranslatable,
		}
		list.All = append(list.All, t)
		// First-write-wins on duplicate language codes; All preserves all of them.
		if t.IsGenerated {
			if _, dup := list.Generated[t.LanguageCode]; !dup {
				list.Generated[t.LanguageCode] = t
			} else {
				a.logf("transcriptapi: duplicate generated track for %q on %s", t.LanguageCode, videoID)
			}
		} else {
			if _, dup := list.Manual[t.LanguageCode]; !dup {
				list.Manual[t.LanguageCode] = t
			} else {
				a.logf("transcriptapi: duplicate manual track for %q on %s", t.LanguageCode, videoID)
			}
		}
	}
	return list
}

// FindTranscript searches for a transcript matching one of the given language
// codes (in priority order). Manual transcripts are preferred over generated.
func (l *TranscriptList) FindTranscript(languages []string) (*Transcript, error) {
	for _, lang := range languages {
		if t, ok := l.Manual[lang]; ok {
			return t, nil
		}
	}
	for _, lang := range languages {
		if t, ok := l.Generated[lang]; ok {
			return t, nil
		}
	}
	return nil, fmt.Errorf("%w: requested %v", ErrNoTranscriptFound, languages)
}

// Translate returns a copy of t configured to fetch the transcript translated
// into targetLang. Returns ErrNotTranslatable if YouTube did not flag the track
// as translatable.
func (t *Transcript) Translate(targetLang string) (*Transcript, error) {
	if !t.Translatable {
		return nil, fmt.Errorf("%w: %s", ErrNotTranslatable, t.LanguageCode)
	}
	tt := *t
	tt.URL = appendQuery(t.URL, "tlang", targetLang)
	tt.LanguageCode = targetLang
	tt.Language = targetLang
	return &tt, nil
}

func appendQuery(rawURL, key, value string) string {
	sep := "&"
	if !strings.Contains(rawURL, "?") {
		sep = "?"
	}
	return rawURL + sep + key + "=" + value
}

type xmlTranscript struct {
	Texts []struct {
		Start    float64 `xml:"start,attr"`
		Duration float64 `xml:"dur,attr"`
		Text     string  `xml:",chardata"`
	} `xml:"text"`
}

// Fetch downloads the transcript XML and returns parsed snippets.
func (t *Transcript) Fetch(ctx context.Context) (*FetchedTranscript, error) {
	api := t.api
	if api == nil {
		api = &API{}
	}
	var ft *FetchedTranscript
	err := api.retryOnBlock(ctx, func() error {
		var err error
		ft, err = t.fetchOnce(ctx, api)
		return err
	})
	if err != nil {
		return nil, err
	}
	return ft, nil
}

func (t *Transcript) fetchOnce(ctx context.Context, api *API) (*FetchedTranscript, error) {
	url := t.URL
	if strings.Contains(url, "&exp=xpe") {
		if api.POToken == "" {
			return nil, ErrPoTokenRequired
		}
		url = appendQuery(url, "pot", api.POToken)
	}
	body, err := api.get(ctx, url, api.desktopUA())
	if err != nil {
		return nil, err
	}
	var x xmlTranscript
	if err := xml.Unmarshal(body, &x); err != nil {
		return nil, fmt.Errorf("parse transcript xml: %w", err)
	}
	snippets := make([]Snippet, 0, len(x.Texts))
	for _, line := range x.Texts {
		if line.Text == "" {
			continue
		}
		text := htmlTagRegex.ReplaceAllString(html.UnescapeString(line.Text), "")
		snippets = append(snippets, Snippet{
			Text:     text,
			Start:    line.Start,
			Duration: line.Duration,
		})
	}
	return &FetchedTranscript{
		Snippets:     snippets,
		VideoID:      t.VideoID,
		Language:     t.Language,
		LanguageCode: t.LanguageCode,
		IsGenerated:  t.IsGenerated,
	}, nil
}
