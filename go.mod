// A collage plugin that limits how fast one client can hit the site: a token
// bucket per client address, per rule, answered with 429 and Retry-After once it
// is empty. The buckets are in memory, so each instance counts on its own.
module github.com/Elagoht/collage-ratelimit

go 1.26

require github.com/Elagoht/collage v0.52.0

retract v0.1.4 // tagged by mistake on the previous release's code; use v0.1.5 or later
