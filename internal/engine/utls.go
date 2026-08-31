package engine

import (
	"context"
	"net"
	"net/http"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/publicsuffix"
)

// newHTTPClient builds the Engine's HTTP client. Its TLS handshake is disguised
// as Chrome's (uTLS) — some CDNs (TikTok / ByteDance especially) fingerprint the
// ClientHello (JA3) and 403 anything that isn't a mainstream browser. It also
// carries the auth-bearing headers forward across a cross-host redirect that the
// stdlib would strip, but only within the same registrable domain.
func newHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}

	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		DialContext:         dialer.DialContext,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			raw, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			uconn := utls.UClient(raw, &utls.Config{ServerName: host}, utls.HelloCustom)
			if err := uconn.ApplyPreset(chromeH1Spec()); err != nil {
				_ = raw.Close()
				return nil, err
			}
			if err := uconn.HandshakeContext(ctx); err != nil {
				_ = raw.Close()
				return nil, err
			}
			return uconn, nil
		},
	}

	return &http.Client{Transport: tr, CheckRedirect: preserveAuthHeaders}
}

// chromeH1Spec is Chrome's ClientHello with ALPN pinned to http/1.1 so the
// stdlib h1 transport can speak to whatever comes back. JA3 hashes extension
// *types* and their order, not the ALPN list contents, so the fingerprint still
// matches Chrome.
func chromeH1Spec() *utls.ClientHelloSpec {
	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
	if err != nil {
		// HelloChrome_Auto always resolves; fall back defensively.
		return &utls.ClientHelloSpec{}
	}
	for _, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			alpn.AlpnProtocols = []string{"http/1.1"}
		}
	}
	return &spec
}

// preserveAuthHeaders re-attaches Cookie / UA / Referer / Sec-Fetch-* to a
// redirect target when it shares the original request's registrable domain
// (e.g. www.tiktok.com -> v16-webapp-prime.tiktok.com). Go drops these on a
// cross-host redirect; TikTok's play URL 302s to a CDN host that still needs the
// tiktok.com cookies.
func preserveAuthHeaders(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return http.ErrUseLastResponse
	}
	orig := via[0]
	if !sameSite(orig.URL.Hostname(), req.URL.Hostname()) {
		return nil
	}
	for _, h := range []string{
		"Cookie", "User-Agent", "Referer",
		"Sec-Fetch-Dest", "Sec-Fetch-Mode", "Sec-Fetch-Site",
		"Accept", "Accept-Language",
	} {
		if v := orig.Header.Get(h); v != "" && req.Header.Get(h) == "" {
			req.Header.Set(h, v)
		}
	}
	return nil
}

// sameSite reports whether a and b share a registrable domain (eTLD+1), using
// the public suffix list so bbc.co.uk / news.co.uk are correctly seen as
// different orgs rather than both "co.uk".
func sameSite(a, b string) bool {
	ra, erra := publicsuffix.EffectiveTLDPlusOne(a)
	rb, errb := publicsuffix.EffectiveTLDPlusOne(b)
	return erra == nil && errb == nil && ra == rb
}
