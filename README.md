# elagoht/ratelimit

A collage plugin that limits how fast one client can hit the site: a token bucket
per client, per rule, and a `429 Too Many Requests` with `Retry-After` once it is
empty. Registering it is the whole of it — with no options, every form and action
is limited to a burst of ten, then one request every two seconds.

```go
app, err := collage.New(&collage.Config{
	Plugins: []collage.Plugin{ratelimit.New(ratelimit.Options{
		Rules: []ratelimit.Rule{
			{PathPrefix: "/login", Rate: 0.1, Burst: 5},
			{Rate: 0.5, Burst: 10},
		},
	})},
})
```

Requires collage v0.24.0 or later.

## Rules

A rule matches by method and path prefix, and the **first** rule a request matches
is the one that counts it — so put the narrow rules first. A request no rule
matches is not limited at all.

| Field | | Default |
| --- | --- | --- |
| `Methods` | The methods the rule covers; `["*"]` is every method | every method but `GET`, `HEAD` and `OPTIONS` |
| `PathPrefix` | The paths it covers | `/`, every path |
| `Rate` | Requests a second the bucket refills by: `0.1` is one every ten seconds | required |
| `Burst` | Requests a client can make at once, the size of the bucket | `Rate` rounded up, at least one |

The default methods are the ones that change something — the forms and actions a
bot hammers. Pages are served from collage's cache and cost little; limit them too
with `Methods: []string{"*"}` if you want to.

Each rule has its own buckets, so the five login attempts above do not spend the
ten the rest of the site allows.

## Responses

A request a rule matches is answered with the draft IETF headers, so a
well-behaved client can pace itself:

```
RateLimit-Limit: 10
RateLimit-Remaining: 7
RateLimit-Reset: 6
```

`Limit` is the burst, `Remaining` the whole requests left in the bucket, and
`Reset` the seconds until it is full again. Once it is empty the request is not
passed on: it is answered `429`, with `Retry-After` in seconds, a one-line text
body, and `Cache-Control: no-store`.

## Who a client is

A client is its IP address, and an IPv6 client is its `/64` — the block one
connection is usually handed, and can pick any address from.

Behind a reverse proxy every request comes from the proxy, so set `TrustProxy`.
The address is then read from `X-Forwarded-For` — walking it from the right, past
every trusted proxy, to the first address that is not one — or from `X-Real-IP`
when there is no `X-Forwarded-For`. Each proxy appends the address it was reached
from, so what is to the right of that point was written by proxies you run, and
what is to the left by the client, who can write anything there. The headers are
believed only on a request that arrived from a trusted proxy.

`TrustedProxies` lists the proxies' addresses and CIDR ranges. Empty trusts
loopback and private addresses, which is what a proxy on the same host or network
has; name them when the proxy is a CDN, whose addresses are public.

Without `TrustProxy` the headers are ignored: anybody can send them, and a limit
keyed on them is one every client steps around by changing a header.

`KeyFunc`, from Go, names the client some other way — an API key, a session, a
signed-in user. When it returns `""` the address is used.

```go
ratelimit.New(ratelimit.Options{
	KeyFunc: func(r *http.Request) string { return r.Header.Get("X-API-Key") },
})
```

## Memory

Buckets are kept in memory. Once a minute a goroutine drops every bucket that has
filled up again — a full bucket is what a client who never came would get, so
nothing is forgotten — and `Shutdown` stops it. `MaxKeys` bounds each rule's
buckets however many addresses a flood comes from: past it, an arbitrary bucket is
dropped to make room. The goroutine starts with the first limited request, so a
static build or a command starts none.

## Skipped paths

`Skip` lists path prefixes never limited, by default collage's own development
endpoints under `/_collage/`. collage redirects a path with dot segments or
doubled slashes to its clean spelling before any middleware runs, so
`/_collage/../contact` never reaches the plugin as spelled. The plugin does not
rely on that alone: rules match the path with its dot segments resolved, and a
path is skipped only when it had none.

## Configuration

```json
{
  "elagoht/ratelimit": {
    "rules": [
      { "pathPrefix": "/login", "rate": 0.1, "burst": 5 },
      { "methods": ["POST", "PUT", "PATCH", "DELETE"], "rate": 0.5, "burst": 10 }
    ],
    "trustProxy": true,
    "trustedProxies": ["10.0.0.0/8"],
    "skip": ["/_collage/", "/healthz"],
    "maxKeys": 100000
  }
}
```

Rules in configuration replace the ones given in Go. A rate that is not positive,
a path or skip prefix without a leading `/`, a trusted proxy that is neither an
address nor a range — each stops the application from starting.

## Limitations

- **Limits are per process.** Several instances behind one load balancer each
  count on their own, so a client can make as many requests as the limit allows
  on each of them, and a restart forgets every bucket. A limit shared across
  instances needs a shared store, which this plugin does not have.
- A static build renders without requests, so nothing is limited there — nor on
  a statically exported site, which has no server of its own.
- Clients behind one NAT, or one corporate proxy, share an address and so share
  a bucket. Set rates for the busiest such address you expect, or key by
  something better with `KeyFunc`.
- A 429 is still a request the server received. This keeps a form from being
  submitted a thousand times a minute; it is not a defence against a flood that
  saturates the network before any request reaches Go.

## Changes

### v0.1.2

- `collage.json`: the plugin described to editors — its template functions,
  snippets and configuration schema — for the Collage Snippets & Highlighter
  extension and any tool reading it.

### v0.1.1

- README: collage v0.24.0 cleans paths before middleware; the plugin keeps its own check on the cleaned path as a second line.
- Requires collage v0.24.0.
