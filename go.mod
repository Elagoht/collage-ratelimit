// A collage plugin that limits how fast one client can hit the site: a token
// bucket per client address, per rule, answered with 429 and Retry-After once it
// is empty. The buckets are in memory, so each instance counts on its own.
module github.com/Elagoht/collage-ratelimit

go 1.26

require github.com/Elagoht/collage v0.23.0
