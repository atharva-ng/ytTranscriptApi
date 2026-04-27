package transcriptapi

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ProxyConfig configures the HTTP proxy used for outgoing requests. Pass one
// to API.Proxy to route the watch-page scrape, the InnerTube call, and the
// transcript download through a proxy.
//
// YouTube blocks most cloud-provider IP ranges. The reliable workaround is a
// rotating residential proxy pool: a fresh egress IP per request keeps you
// below the per-IP rate budget and recovers automatically when an IP is burnt.
type ProxyConfig interface {
	// ProxyURL is invoked per request and returned to net/http.Transport.Proxy.
	// Return (nil, nil) to bypass the proxy for a given request.
	ProxyURL(req *http.Request) (*url.URL, error)

	// RetriesWhenBlocked is the number of additional attempts after an IP block
	// (HTTP 429 or YouTube reCAPTCHA challenge). Useful with rotating pools, where
	// each retry triggers a new IP. Zero disables block-retries.
	RetriesWhenBlocked() int

	// PreventKeepAlives reports whether the underlying HTTP transport should
	// disable connection keep-alives. Rotating proxies typically rotate IPs only
	// when a fresh TCP connection is opened, so keep-alives defeat rotation.
	PreventKeepAlives() bool
}

// GenericProxyConfig is a static HTTP/HTTPS proxy. Set HTTPURL, HTTPSURL, or
// both. If only one is set, it is used for both schemes.
type GenericProxyConfig struct {
	HTTPURL  string
	HTTPSURL string

	// Retries is the number of additional attempts after an IP block. Zero means
	// no block-retries; static proxies usually can't recover by retrying.
	Retries int

	// DisableKeepAlives disables HTTP keep-alives. Set if the proxy rotates IPs
	// per-connection.
	DisableKeepAlives bool
}

func (g *GenericProxyConfig) pick(scheme string) string {
	if scheme == "https" {
		if g.HTTPSURL != "" {
			return g.HTTPSURL
		}
		return g.HTTPURL
	}
	if g.HTTPURL != "" {
		return g.HTTPURL
	}
	return g.HTTPSURL
}

// ProxyURL implements ProxyConfig.
func (g *GenericProxyConfig) ProxyURL(req *http.Request) (*url.URL, error) {
	raw := g.pick(req.URL.Scheme)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse proxy URL: %w", err)
	}
	return u, nil
}

// RetriesWhenBlocked implements ProxyConfig.
func (g *GenericProxyConfig) RetriesWhenBlocked() int { return g.Retries }

// PreventKeepAlives implements ProxyConfig.
func (g *GenericProxyConfig) PreventKeepAlives() bool { return g.DisableKeepAlives }

// WebshareProxyConfig configures Webshare rotating residential proxies, which
// is the recommended way to bypass YouTube IP blocks. Sign up for a
// "Residential" plan (not "Proxy Server" or "Static Residential") at
// webshare.io and use the Proxy Username and Proxy Password from the dashboard.
//
// The zero value is not usable: Username and Password are required.
type WebshareProxyConfig struct {
	Username string
	Password string

	// Locations is an optional list of country codes (e.g. "US", "GB") that
	// restrict the rotating pool to those regions. Reduces latency and bypasses
	// some location-based gates.
	Locations []string

	// Domain overrides the proxy host. Defaults to "p.webshare.io".
	Domain string
	// Port overrides the proxy port. Defaults to 80.
	Port int

	// Retries is the number of additional attempts after an IP block. Zero
	// applies the recommended default (10); use a negative value to disable.
	Retries int

	// AllowKeepAlives keeps TCP connections open across requests. Default
	// (false) disables keep-alives so each request gets a freshly rotated IP.
	AllowKeepAlives bool
}

const (
	webshareDefaultDomain = "p.webshare.io"
	webshareDefaultPort   = 80
	webshareDefaultRetry  = 10
)

func (w *WebshareProxyConfig) buildURL() (*url.URL, error) {
	if w.Username == "" || w.Password == "" {
		return nil, fmt.Errorf("WebshareProxyConfig: Username and Password are required")
	}
	domain := w.Domain
	if domain == "" {
		domain = webshareDefaultDomain
	}
	port := w.Port
	if port == 0 {
		port = webshareDefaultPort
	}

	var locs strings.Builder
	for _, loc := range w.Locations {
		locs.WriteByte('-')
		locs.WriteString(strings.ToUpper(loc))
	}
	user := strings.TrimSuffix(w.Username, "-rotate") + locs.String() + "-rotate"

	return &url.URL{
		Scheme: "http",
		User:   url.UserPassword(user, w.Password),
		Host:   fmt.Sprintf("%s:%d", domain, port),
		Path:   "/",
	}, nil
}

// ProxyURL implements ProxyConfig.
func (w *WebshareProxyConfig) ProxyURL(req *http.Request) (*url.URL, error) {
	return w.buildURL()
}

// RetriesWhenBlocked implements ProxyConfig.
func (w *WebshareProxyConfig) RetriesWhenBlocked() int {
	if w.Retries == 0 {
		return webshareDefaultRetry
	}
	if w.Retries < 0 {
		return 0
	}
	return w.Retries
}

// PreventKeepAlives implements ProxyConfig.
func (w *WebshareProxyConfig) PreventKeepAlives() bool { return !w.AllowKeepAlives }
