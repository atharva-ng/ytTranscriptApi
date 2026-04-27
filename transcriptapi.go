package transcriptapi

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const defaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

const (
	watchURL        = "https://www.youtube.com/watch?v=%s"
	innertubeAPIURL = "https://www.youtube.com/youtubei/v1/player?key=%s"
)

var innertubeContext = map[string]any{
	"client": map[string]any{
		"clientName":    "ANDROID",
		"clientVersion": "20.10.38",
	},
}

var (
	ErrTranscriptsDisabled = errors.New("transcripts are disabled for this video")
	ErrNoTranscriptFound   = errors.New("no transcript found for the requested languages")
	ErrIPBlocked           = errors.New("YouTube is blocking requests from this IP")
	ErrVideoUnavailable    = errors.New("video unavailable")
	ErrPoTokenRequired     = errors.New("PO token required for this transcript")
)

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
	client       *http.Client
	VideoID      string
	URL          string
	Language     string
	LanguageCode string
	IsGenerated  bool
	Translatable bool
}

// TranscriptList groups all available transcripts for a video.
type TranscriptList struct {
	VideoID   string
	Manual    map[string]*Transcript
	Generated map[string]*Transcript
}

// API is the entry point. Zero value is usable; it will create its own client.
type API struct {
	HTTPClient *http.Client
}

func (a *API) httpClient() *http.Client {
	if a.HTTPClient != nil {
		return a.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Fetch is a shortcut for List(videoID).FindTranscript(languages).Fetch().
func (a *API) Fetch(videoID string, languages ...string) (*FetchedTranscript, error) {
	if len(languages) == 0 {
		languages = []string{"en"}
	}
	list, err := a.List(videoID)
	if err != nil {
		return nil, err
	}
	t, err := list.FindTranscript(languages)
	if err != nil {
		return nil, err
	}
	return t.Fetch()
}

// List retrieves the available transcripts for a video.
func (a *API) List(videoID string) (*TranscriptList, error) {
	client := a.httpClient()

	apiKey, err := fetchInnertubeAPIKey(client, videoID)
	if err != nil {
		return nil, err
	}
	captions, err := fetchCaptionsJSON(client, videoID, apiKey)
	if err != nil {
		return nil, err
	}
	return buildTranscriptList(client, videoID, captions), nil
}

func doGet(client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Language", "en-US")
	req.Header.Set("User-Agent", defaultUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, ErrIPBlocked
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

var apiKeyRegex = regexp.MustCompile(`"INNERTUBE_API_KEY":\s*"([a-zA-Z0-9_-]+)"`)

func fetchInnertubeAPIKey(client *http.Client, videoID string) (string, error) {
	body, err := doGet(client, fmt.Sprintf(watchURL, videoID))
	if err != nil {
		return "", err
	}
	htmlText := html.UnescapeString(string(body))
	m := apiKeyRegex.FindStringSubmatch(htmlText)
	if len(m) != 2 {
		if strings.Contains(htmlText, `class="g-recaptcha"`) {
			return "", ErrIPBlocked
		}
		return "", errors.New("could not extract INNERTUBE_API_KEY from watch page")
	}
	return m[1], nil
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

func fetchCaptionsJSON(client *http.Client, videoID, apiKey string) (*playerResponse, error) {
	payload, _ := json.Marshal(map[string]any{
		"context": innertubeContext,
		"videoId": videoID,
	})
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf(innertubeAPIURL, apiKey), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Language", "en-US")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", defaultUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, ErrIPBlocked
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("InnerTube player: status %d", resp.StatusCode)
	}

	var pr playerResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, fmt.Errorf("decode player response: %w", err)
	}

	switch pr.PlayabilityStatus.Status {
	case "", "OK":
		// proceed
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

func buildTranscriptList(client *http.Client, videoID string, pr *playerResponse) *TranscriptList {
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
			client:       client,
			VideoID:      videoID,
			URL:          strings.Replace(track.BaseURL, "&fmt=srv3", "", 1),
			Language:     name,
			LanguageCode: track.LanguageCode,
			IsGenerated:  track.Kind == "asr",
			Translatable: track.IsTranslatable,
		}
		if t.IsGenerated {
			list.Generated[t.LanguageCode] = t
		} else {
			list.Manual[t.LanguageCode] = t
		}
	}
	return list
}

// FindTranscript searches for a transcript matching one of the given language
// codes (in priority order). Manually created transcripts are preferred over
// auto-generated ones.
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

type xmlTranscript struct {
	Texts []struct {
		Start    float64 `xml:"start,attr"`
		Duration float64 `xml:"dur,attr"`
		Text     string  `xml:",chardata"`
	} `xml:"text"`
}

var htmlTagRegex = regexp.MustCompile(`<[^>]*>`)

// Fetch downloads the transcript XML and returns parsed snippets.
func (t *Transcript) Fetch() (*FetchedTranscript, error) {
	if strings.Contains(t.URL, "&exp=xpe") {
		return nil, ErrPoTokenRequired
	}
	body, err := doGet(t.client, t.URL)
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
