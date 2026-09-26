package ratelimit_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	ratelimit "github.com/Elagoht/collage-ratelimit"
	"github.com/Elagoht/collage/pkg/collage"
)

func site(t *testing.T, p *ratelimit.Plugin, config map[string]json.RawMessage) http.Handler {
	t.Helper()
	app, err := collage.New(&collage.Config{
		Server: collage.ServerConfig{Host: "localhost", Port: 3000},
		Template: collage.TemplateConfig{FS: fstest.MapFS{
			"t/p.html": {Data: []byte(`<main>home</main>`)},
		}, Root: "t"},
		Plugins:      []collage.Plugin{p},
		PluginConfig: config,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.RegisterPage(collage.NewPage("home").WithContent(collage.NewFragment("home", "p.html").Build()).WithPath("en", "/").Build()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/contact", "/login"} {
		action := collage.NewAction(strings.TrimPrefix(path, "/")).WithPath("en", path).
			WithMethods(http.MethodPost).WithoutCSRF().
			WithHandler(func(context.Context, *collage.RenderContext) (*collage.ActionResult, error) {
				return nil, nil
			}).Build()
		if err := app.RegisterAction(action); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return app.Handler()
}

func do(h http.Handler, method, path, remote string, headers ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = remote
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Add(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// allowed counts how many of n requests got through before the first 429.
func allowed(h http.Handler, n int, method, path, remote string, headers ...string) int {
	for i := range n {
		if do(h, method, path, remote, headers...).Code == http.StatusTooManyRequests {
			return i
		}
	}
	return n
}

// By default forms and actions are limited, at a burst of ten; pages are not.
func TestDefaults(t *testing.T) {
	h := site(t, ratelimit.New(ratelimit.Options{}), nil)
	if got := allowed(h, 30, http.MethodGet, "/", "192.0.2.1:1"); got != 30 {
		t.Errorf("GET was limited after %d requests", got)
	}
	if rec := do(h, http.MethodGet, "/", "192.0.2.1:1"); rec.Header().Get("RateLimit-Limit") != "" {
		t.Errorf("an unlimited request carries RateLimit headers: %v", rec.Header())
	}

	first := do(h, http.MethodPost, "/contact", "192.0.2.1:1")
	if first.Code != http.StatusNoContent {
		t.Fatalf("first POST: %d", first.Code)
	}
	if got := first.Header(); got.Get("RateLimit-Limit") != "10" || got.Get("RateLimit-Remaining") != "9" || got.Get("RateLimit-Reset") != "2" {
		t.Errorf("headers after one request: %v", got)
	}
	if got := allowed(h, 20, http.MethodPost, "/contact", "192.0.2.1:1"); got != 9 {
		t.Errorf("%d more POSTs got through, want 9", got)
	}
	refused := do(h, http.MethodPost, "/contact", "192.0.2.1:1")
	if refused.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d", refused.Code)
	}
	retry, _ := strconv.Atoi(refused.Header().Get("Retry-After"))
	if retry < 1 || retry > 2 || refused.Header().Get("RateLimit-Remaining") != "0" {
		t.Errorf("refusal headers: %v", refused.Header())
	}
	if !strings.HasPrefix(refused.Body.String(), "Too many requests") || refused.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("refusal: %q, %v", refused.Body.String(), refused.Header())
	}

	// Another client has a bucket of its own.
	if rec := do(h, http.MethodPost, "/contact", "192.0.2.2:1"); rec.Code != http.StatusNoContent {
		t.Errorf("another client was refused: %d", rec.Code)
	}
}

// The first rule a request matches is the one that counts it.
func TestFirstMatchingRule(t *testing.T) {
	h := site(t, ratelimit.New(ratelimit.Options{Rules: []ratelimit.Rule{
		{PathPrefix: "/login", Rate: 0.01, Burst: 2},
		{Methods: []string{"*"}, Rate: 0.01, Burst: 5},
	}}), nil)
	if got := allowed(h, 10, http.MethodPost, "/login", "192.0.2.1:1"); got != 2 {
		t.Errorf("login allowed %d, want 2", got)
	}
	// A separate bucket: the login attempts did not spend the site's.
	if got := allowed(h, 10, http.MethodGet, "/", "192.0.2.1:1"); got != 5 {
		t.Errorf("pages allowed %d, want 5", got)
	}
	// GET on /login is not the first rule's (no methods: forms only), so it
	// falls to the second, whose bucket is now empty.
	if rec := do(h, http.MethodGet, "/login", "192.0.2.1:1"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("GET /login: %d", rec.Code)
	}
}

// collage's development endpoints are never limited.
func TestSkip(t *testing.T) {
	h := site(t, ratelimit.New(ratelimit.Options{Rules: []ratelimit.Rule{{Methods: []string{"*"}, Rate: 0.01, Burst: 1}}}), nil)
	for range 5 {
		if rec := do(h, http.MethodGet, "/_collage/anything", "192.0.2.1:1"); rec.Code == http.StatusTooManyRequests {
			t.Fatal("a skipped path was limited")
		}
	}
	// Not a development endpoint, however it is spelled: collage redirects a
	// path with dot segments to its clean spelling before any middleware runs,
	// and the plugin's own check on the cleaned path stays behind that.
	if rec := do(h, http.MethodGet, "/_collage/../", "192.0.2.1:1"); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/" {
		t.Errorf("/_collage/../ = %d %q, want collage's redirect to /", rec.Code, rec.Header().Get("Location"))
	}
	do(h, http.MethodGet, "/", "192.0.2.1:1")
	if rec := do(h, http.MethodGet, "/", "192.0.2.1:1"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("/ after the redirect: %d, want the limit", rec.Code)
	}
}

// The bucket refills at the rule's rate.
func TestRefill(t *testing.T) {
	h := site(t, ratelimit.New(ratelimit.Options{Rules: []ratelimit.Rule{{Rate: 10, Burst: 1}}}), nil)
	if do(h, http.MethodPost, "/contact", "192.0.2.1:1").Code != http.StatusNoContent ||
		do(h, http.MethodPost, "/contact", "192.0.2.1:1").Code != http.StatusTooManyRequests {
		t.Fatal("a burst of one allowed more than one")
	}
	time.Sleep(150 * time.Millisecond)
	if rec := do(h, http.MethodPost, "/contact", "192.0.2.1:1"); rec.Code != http.StatusNoContent {
		t.Errorf("not refilled: %d", rec.Code)
	}
}

// X-Forwarded-For is anybody's to send, so it counts only from a trusted proxy,
// and only its right-most untrusted hop.
func TestClientAddress(t *testing.T) {
	rules := []ratelimit.Rule{{Rate: 0.01, Burst: 1}}
	t.Run("ignored without TrustProxy", func(t *testing.T) {
		h := site(t, ratelimit.New(ratelimit.Options{Rules: rules}), nil)
		do(h, http.MethodPost, "/contact", "192.0.2.1:1", "X-Forwarded-For", "198.51.100.1")
		if rec := do(h, http.MethodPost, "/contact", "192.0.2.1:1", "X-Forwarded-For", "198.51.100.2"); rec.Code != http.StatusTooManyRequests {
			t.Error("a changed header bought a fresh bucket")
		}
	})
	t.Run("behind a trusted proxy", func(t *testing.T) {
		h := site(t, ratelimit.New(ratelimit.Options{Rules: rules, TrustProxy: true}), nil)
		proxy := "10.0.0.5:1"
		do(h, http.MethodPost, "/contact", proxy, "X-Forwarded-For", "203.0.113.9, 198.51.100.1, 10.0.0.4")
		// The client forged a left-most hop; the proxy's entry is what counts.
		if rec := do(h, http.MethodPost, "/contact", proxy, "X-Forwarded-For", "1.1.1.1, 198.51.100.1, 10.0.0.4"); rec.Code != http.StatusTooManyRequests {
			t.Error("a forged left-most hop bought a fresh bucket")
		}
		if rec := do(h, http.MethodPost, "/contact", proxy, "X-Forwarded-For", "198.51.100.2"); rec.Code != http.StatusNoContent {
			t.Error("two clients behind one proxy shared a bucket")
		}
		if rec := do(h, http.MethodPost, "/contact", proxy, "X-Real-IP", "198.51.100.3"); rec.Code != http.StatusNoContent {
			t.Error("X-Real-IP was not read")
		}
		if rec := do(h, http.MethodPost, "/contact", proxy, "X-Real-IP", "198.51.100.3"); rec.Code != http.StatusTooManyRequests {
			t.Error("X-Real-IP did not key the bucket")
		}
	})
	t.Run("from an untrusted address", func(t *testing.T) {
		h := site(t, ratelimit.New(ratelimit.Options{Rules: rules, TrustProxy: true, TrustedProxies: []string{"10.0.0.0/8"}}), nil)
		do(h, http.MethodPost, "/contact", "192.0.2.7:1", "X-Forwarded-For", "198.51.100.1")
		if rec := do(h, http.MethodPost, "/contact", "192.0.2.7:1", "X-Forwarded-For", "198.51.100.2"); rec.Code != http.StatusTooManyRequests {
			t.Error("a header from a client that is not a proxy was believed")
		}
	})
	t.Run("an IPv6 /64 is one client", func(t *testing.T) {
		h := site(t, ratelimit.New(ratelimit.Options{Rules: rules}), nil)
		do(h, http.MethodPost, "/contact", "[2001:db8:1:2::1]:1")
		if rec := do(h, http.MethodPost, "/contact", "[2001:db8:1:2::ffff]:1"); rec.Code != http.StatusTooManyRequests {
			t.Error("another address in the same /64 bought a fresh bucket")
		}
		if rec := do(h, http.MethodPost, "/contact", "[2001:db8:1:3::1]:1"); rec.Code != http.StatusNoContent {
			t.Error("another /64 shared the bucket")
		}
	})
}

// KeyFunc names the client, and an empty key falls back to the address.
func TestKeyFunc(t *testing.T) {
	h := site(t, ratelimit.New(ratelimit.Options{
		Rules:   []ratelimit.Rule{{Rate: 0.01, Burst: 1}},
		KeyFunc: func(r *http.Request) string { return r.Header.Get("X-API-Key") },
	}), nil)
	do(h, http.MethodPost, "/contact", "192.0.2.1:1", "X-API-Key", "a")
	if rec := do(h, http.MethodPost, "/contact", "192.0.2.2:1", "X-API-Key", "a"); rec.Code != http.StatusTooManyRequests {
		t.Error("one key from two addresses got two buckets")
	}
	if rec := do(h, http.MethodPost, "/contact", "192.0.2.1:1", "X-API-Key", "b"); rec.Code != http.StatusNoContent {
		t.Error("two keys shared a bucket")
	}
	if rec := do(h, http.MethodPost, "/contact", "192.0.2.1:1"); rec.Code != http.StatusNoContent {
		t.Error("no key did not fall back to the address")
	}
}

// Past MaxKeys a bucket is dropped to make room: memory is bounded.
func TestMaxKeys(t *testing.T) {
	h := site(t, ratelimit.New(ratelimit.Options{Rules: []ratelimit.Rule{{Rate: 0.01, Burst: 1}}, MaxKeys: 1}), nil)
	do(h, http.MethodPost, "/contact", "192.0.2.1:1")
	do(h, http.MethodPost, "/contact", "192.0.2.2:1")
	if rec := do(h, http.MethodPost, "/contact", "192.0.2.1:1"); rec.Code != http.StatusNoContent {
		t.Errorf("the first client's bucket was kept past MaxKeys: %d", rec.Code)
	}
}

// Configuration overlays the options.
func TestConfiguration(t *testing.T) {
	h := site(t, ratelimit.New(ratelimit.Options{}), map[string]json.RawMessage{
		ratelimit.Name: json.RawMessage(`{"rules": [{"methods": ["get"], "pathPrefix": "/", "rate": 0.01, "burst": 2}]}`),
	})
	if got := allowed(h, 5, http.MethodGet, "/", "192.0.2.1:1"); got != 2 {
		t.Errorf("allowed %d, want 2", got)
	}
}

// A misconfigured plugin stops the application from starting.
func TestMisconfiguration(t *testing.T) {
	for name, opts := range map[string]ratelimit.Options{
		"no rate":        {Rules: []ratelimit.Rule{{Burst: 3}}},
		"negative burst": {Rules: []ratelimit.Rule{{Rate: 1, Burst: -1}}},
		"relative path":  {Rules: []ratelimit.Rule{{Rate: 1, PathPrefix: "login"}}},
		"bad method":     {Rules: []ratelimit.Rule{{Rate: 1, Methods: []string{"GET, POST"}}}},
		"bad proxy":      {TrustedProxies: []string{"proxy.internal"}},
		"bad skip":       {Skip: []string{"_collage"}},
		"negative keys":  {MaxKeys: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if rec := do(site(t, ratelimit.New(opts), nil), http.MethodGet, "/", "192.0.2.1:1"); rec.Code != http.StatusServiceUnavailable {
				t.Errorf("status %d, want 503", rec.Code)
			}
		})
	}
}

// Shutdown stops the janitor, and a second one is harmless.
func TestShutdown(t *testing.T) {
	p := ratelimit.New(ratelimit.Options{})
	h := site(t, p, nil)
	do(h, http.MethodPost, "/contact", "192.0.2.1:1")
	done := make(chan struct{})
	go func() {
		_ = p.Shutdown(context.Background())
		_ = p.Shutdown(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not return")
	}
}
