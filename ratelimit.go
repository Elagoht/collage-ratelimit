// Package ratelimit is a collage plugin that limits how fast one client can hit
// the site: a token bucket per client, per rule, answered with 429 once it is
// empty.
//
//	app, err := collage.New(&collage.Config{
//		Plugins: []collage.Plugin{ratelimit.New(ratelimit.Options{
//			Rules: []ratelimit.Rule{
//				{PathPrefix: "/login", Rate: 0.1, Burst: 5},
//				{Rate: 0.5, Burst: 10},
//			},
//		})},
//	})
//
// A rule with no methods covers every method but GET, HEAD and OPTIONS — the
// forms and actions a bot hammers — and the first rule a request matches is the
// one that counts it. A client is its IP address, or whatever Options.KeyFunc
// says it is.
//
// The buckets live in the process's memory. Several instances behind one load
// balancer each count on their own, so a client can make as many requests as
// the limit allows on each of them.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/netip"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Elagoht/collage/pkg/collage"
)

// Name is the plugin's name, and the key its configuration is found under.
const Name = "elagoht/ratelimit"

// Options configures the plugin.
type Options struct {
	// Rules are tried in order and the first that matches a request counts it; a
	// request no rule matches is not limited. Empty is one rule: every method but
	// GET, HEAD and OPTIONS, everywhere, at half a request a second with a burst
	// of ten.
	Rules []Rule `json:"rules"`
	// KeyFunc names the client a request comes from: an API key, a session, a
	// user. Unset, or returning "", the client is its IP address. It can only be
	// set from Go.
	KeyFunc func(r *http.Request) string `json:"-"`
	// TrustProxy reads the client's address from X-Forwarded-For, or X-Real-IP,
	// when the request came from a trusted proxy. Without it those headers are
	// ignored: anybody can send them, and a limit keyed on them is a limit every
	// client can step around by changing a header.
	TrustProxy bool `json:"trustProxy"`
	// TrustedProxies are the addresses and CIDR ranges of the proxies in front of
	// the site. Empty trusts loopback and private addresses, which is what a
	// reverse proxy on the same host or network has.
	TrustedProxies []string `json:"trustedProxies"`
	// Skip are path prefixes never limited. Default ["/_collage/"], collage's own
	// development endpoints.
	Skip []string `json:"skip"`
	// MaxKeys is the most clients each rule keeps a bucket for. Past it, an
	// arbitrary bucket is dropped to make room, which bounds memory however many
	// addresses a flood comes from. Default 100000.
	MaxKeys int `json:"maxKeys"`
}

// Rule is a limit on the requests it matches.
type Rule struct {
	// Methods the rule matches. Empty is every method but GET, HEAD and OPTIONS;
	// ["*"] is every method.
	Methods []string `json:"methods"`
	// PathPrefix the rule matches. Empty is "/", every path.
	PathPrefix string `json:"pathPrefix"`
	// Rate is how many requests a second the bucket refills by, and so the rate a
	// client can keep up: 0.1 is one request every ten seconds.
	Rate float64 `json:"rate"`
	// Burst is how many requests a client can make at once before Rate applies,
	// and the size of the bucket. Default Rate rounded up, at least one.
	Burst int `json:"burst"`
}

var _ collage.Plugin = (*Plugin)(nil)

// Plugin limits requests.
type Plugin struct {
	opts     Options
	trusted  []netip.Prefix
	limiters []*limiter

	janitorOnce sync.Once
	stopOnce    sync.Once
	stop        chan struct{}
	done        chan struct{}
}

// New returns a plugin with opts as its starting point, which the application's
// own configuration is then decoded over.
func New(opts Options) *Plugin { return &Plugin{opts: opts} }

func (p *Plugin) Name() string    { return Name }
func (p *Plugin) Version() string { return "0.1.4" }

// Init reads and checks the configuration and wraps every request.
func (p *Plugin) Init(_ context.Context, host collage.Host) error {
	cfg, err := collage.PluginConfig(host, p.opts)
	if err != nil {
		return err
	}
	p.opts = cfg
	if err := p.prepare(); err != nil {
		return err
	}
	return host.Use(p.middleware)
}

// Shutdown stops the goroutine that drops idle buckets.
func (p *Plugin) Shutdown(context.Context) error {
	// Taking the Once here means no request can start the janitor afterwards,
	// and makes what a request's Once wrote visible to this goroutine.
	p.janitorOnce.Do(func() {})
	p.stopOnce.Do(func() {
		if p.stop != nil {
			close(p.stop)
			<-p.done
		}
	})
	return nil
}

func (p *Plugin) prepare() error {
	o := &p.opts
	if len(o.Rules) == 0 {
		o.Rules = []Rule{{Rate: 0.5, Burst: 10}}
	}
	if o.Skip == nil {
		o.Skip = []string{"/_collage/"}
	}
	for _, prefix := range o.Skip {
		if !strings.HasPrefix(prefix, "/") {
			return fmt.Errorf("ratelimit: skip prefix %q must begin with /", prefix)
		}
	}
	if o.MaxKeys < 0 {
		return errors.New("ratelimit: maxKeys cannot be negative")
	}
	if o.MaxKeys == 0 {
		o.MaxKeys = 100_000
	}
	for i := range o.Rules {
		rule := &o.Rules[i]
		if rule.Rate <= 0 || math.IsInf(rule.Rate, 0) || math.IsNaN(rule.Rate) {
			return fmt.Errorf("ratelimit: rule %d: rate must be a positive number of requests a second", i)
		}
		if rule.Burst < 0 {
			return fmt.Errorf("ratelimit: rule %d: burst cannot be negative", i)
		}
		if rule.Burst == 0 {
			rule.Burst = max(1, int(math.Ceil(rule.Rate)))
		}
		if rule.PathPrefix == "" {
			rule.PathPrefix = "/"
		}
		if !strings.HasPrefix(rule.PathPrefix, "/") {
			return fmt.Errorf("ratelimit: rule %d: path prefix %q must begin with /", i, rule.PathPrefix)
		}
		for j, m := range rule.Methods {
			if m == "" || strings.ContainsAny(m, " \t,") {
				return fmt.Errorf("ratelimit: rule %d: method %q is not a method", i, m)
			}
			rule.Methods[j] = strings.ToUpper(m)
		}
		p.limiters = append(p.limiters, &limiter{rule: *rule, max: o.MaxKeys, buckets: map[string]*bucket{}})
	}
	for _, s := range o.TrustedProxies {
		prefix, err := netip.ParsePrefix(s)
		if err != nil {
			addr, addrErr := netip.ParseAddr(s)
			if addrErr != nil {
				return fmt.Errorf("ratelimit: trusted proxy %q is neither an address nor a CIDR range", s)
			}
			prefix = netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen())
		}
		p.trusted = append(p.trusted, prefix.Masked())
	}
	return nil
}

func (p *Plugin) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Matched on the cleaned path, and skipped only when the path was clean
		// already, so /_collage/../contact never passes as a development
		// endpoint. collage v0.24.0 redirects such a path before any middleware
		// runs; this stays as a second line, for a handler that does not.
		clean := cleanPath(r.URL.Path)
		if clean == r.URL.Path {
			for _, prefix := range p.opts.Skip {
				if strings.HasPrefix(clean, prefix) {
					next.ServeHTTP(w, r)
					return
				}
			}
		}
		l := p.match(r.Method, clean)
		if l == nil {
			next.ServeHTTP(w, r)
			return
		}
		p.janitorOnce.Do(p.startJanitor)
		key := ""
		if p.opts.KeyFunc != nil {
			key = p.opts.KeyFunc(r)
		}
		if key == "" {
			key = p.clientIP(r)
		}
		d := l.take(key, time.Now())
		h := w.Header()
		h.Set("RateLimit-Limit", strconv.Itoa(l.rule.Burst))
		h.Set("RateLimit-Remaining", strconv.Itoa(d.remaining))
		h.Set("RateLimit-Reset", strconv.Itoa(seconds(d.reset)))
		if !d.allowed {
			retry := seconds(d.retry)
			h.Set("Retry-After", strconv.Itoa(retry))
			h.Set("Content-Type", "text/plain; charset=utf-8")
			h.Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintf(w, "Too many requests. Try again in %d %s.\n", retry, plural(retry, "second", "seconds"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// match returns the limiter of the first rule a request matches, or nil.
func (p *Plugin) match(method, urlPath string) *limiter {
	for _, l := range p.limiters {
		if strings.HasPrefix(urlPath, l.rule.PathPrefix) && methodMatches(l.rule.Methods, method) {
			return l
		}
	}
	return nil
}

// cleanPath resolves dot segments and doubled slashes, keeping a trailing slash.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	c := path.Clean(p)
	if strings.HasSuffix(p, "/") && c != "/" {
		c += "/"
	}
	return c
}

func methodMatches(methods []string, method string) bool {
	if len(methods) == 0 {
		switch method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			return false
		}
		return true
	}
	for _, m := range methods {
		if m == "*" || m == method {
			return true
		}
	}
	return false
}

// clientIP returns the address r came from, as a bucket key.
//
// Behind trusted proxies it walks X-Forwarded-For from the right, past every
// proxy the site trusts, and takes the first address that is not one: each
// proxy appends the address it was reached from, so everything to the right of
// that point was written by a proxy the site runs, and everything to the left by
// the client, who can write anything there.
func (p *Plugin) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	addr = addr.Unmap()
	if !p.opts.TrustProxy || !p.isTrusted(addr) {
		return keyOf(addr)
	}
	if values := r.Header.Values("X-Forwarded-For"); len(values) > 0 {
		hops := strings.Split(strings.Join(values, ","), ",")
		for i := len(hops) - 1; i >= 0; i-- {
			hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
			if err != nil {
				// A trusted proxy writes an address; this was written by the
				// client, so the nearest trusted hop is as far as can be told.
				break
			}
			addr = hop.Unmap()
			if !p.isTrusted(addr) {
				break
			}
		}
		return keyOf(addr)
	}
	if realIP, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); err == nil {
		return keyOf(realIP.Unmap())
	}
	return keyOf(addr)
}

func (p *Plugin) isTrusted(addr netip.Addr) bool {
	if p.trusted == nil {
		return addr.IsLoopback() || addr.IsPrivate()
	}
	for _, prefix := range p.trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// keyOf is the bucket an address counts against. An IPv6 client is usually handed
// a whole /64 and can pick any address in it, so the /64 is the client.
func keyOf(addr netip.Addr) string {
	if addr.Is6() {
		prefix, _ := addr.Prefix(64)
		return prefix.String()
	}
	return addr.String()
}

// seconds rounds up, so a client told to wait n seconds is not refused again for
// arriving on the dot.
func seconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int(math.Ceil(d.Seconds()))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// startJanitor drops, once a minute, the buckets that have filled up again. A full
// bucket is what a client that never came would get, so dropping one forgets
// nothing.
func (p *Plugin) startJanitor() {
	p.stop = make(chan struct{})
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case now := <-ticker.C:
				for _, l := range p.limiters {
					l.evict(now)
				}
			}
		}
	}()
}

type bucket struct {
	tokens float64
	last   time.Time
}

type limiter struct {
	rule    Rule
	max     int
	mu      sync.Mutex
	buckets map[string]*bucket
}

type decision struct {
	allowed      bool
	remaining    int
	reset, retry time.Duration
}

// take spends a token from key's bucket, if there is one to spend.
func (l *limiter) take(key string, now time.Time) decision {
	l.mu.Lock()
	defer l.mu.Unlock()
	burst := float64(l.rule.Burst)
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.max {
			// Map iteration starts anywhere, which makes this a random eviction:
			// cheap, and no worse than any other choice under a flood.
			for k := range l.buckets {
				delete(l.buckets, k)
				break
			}
		}
		b = &bucket{tokens: burst, last: now}
		l.buckets[key] = b
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(burst, b.tokens+elapsed*l.rule.Rate)
		b.last = now
	}
	d := decision{}
	if b.tokens >= 1 {
		b.tokens--
		d.allowed = true
	} else {
		d.retry = l.until(1 - b.tokens)
	}
	d.remaining = int(math.Floor(b.tokens))
	d.reset = l.until(burst - b.tokens)
	return d
}

// until is how long the bucket takes to gain tokens.
func (l *limiter) until(tokens float64) time.Duration {
	return time.Duration(tokens / l.rule.Rate * float64(time.Second))
}

func (l *limiter) evict(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	burst := float64(l.rule.Burst)
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rule.Rate >= burst {
			delete(l.buckets, k)
		}
	}
}
