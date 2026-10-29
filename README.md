# url-shortener-at-scale

[![CI](https://github.com/MahyarNemati/url-shortener-at-scale/actions/workflows/ci.yml/badge.svg)](https://github.com/MahyarNemati/url-shortener-at-scale/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/MahyarNemati/url-shortener-at-scale)](https://goreportcard.com/report/github.com/MahyarNemati/url-shortener-at-scale)

The URL shortener system-design question, built for real: coordination-free ID
generation, non-enumerable slugs, a cache designed for the load pattern a
public shortener actually attracts, ClickHouse click analytics, and k6 load
tests whose traffic mix is the interesting part.

**The write-up is the point: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).**

```bash
export SHORTENER_TOKEN=$(openssl rand -hex 16)
export SHORTENER_SLUG_SECRET=$(openssl rand -hex 16)
go run ./cmd/shortener

curl -s -X POST localhost:8080/api/v1/links \
  -H "Authorization: Bearer $SHORTENER_TOKEN" \
  -d '{"target":"https://go.dev/doc"}'
# {"slug":"LjMuXIjwsLQ","short_url":"http://localhost:8080/LjMuXIjwsLQ", ...}

curl -i localhost:8080/LjMuXIjwsLQ
# HTTP/1.1 302 Found
# Location: https://go.dev/doc
```

With ClickHouse:

```bash
docker compose up -d --build --wait
```

## The four decisions that matter

### 1. Slugs are Snowflake IDs, obfuscated

The obvious options are each wrong in a specific way: a random slug needs a
uniqueness check (a database round trip before you can answer), a database
sequence is a single point of coordination that also leaks your volume, and a
hash of the URL makes two slugs for one destination impossible — which breaks
per-campaign analytics.

So: 41 bits of timestamp, 10 bits of instance id, 12 bits of sequence. Two
processes generate IDs with **no communication at all**, at 4096/ms each.

The cost is that Snowflake IDs are monotonic and therefore **enumerable** —
mint one, decode the base62, and walk the whole corpus. The fix is a 4-round
**Feistel network** before encoding: a bijection, so there are still no
collisions and no lookup table, but the next slug is not guessable.

It is deliberately not encryption, and the README says so because the
distinction matters. The requirement is "not enumerable by adding one", not
"secret against an adversary". The bijection is verified over 300,000 values,
because if it ever failed two links would share a slug.

The clock handling is the part a broken Snowflake implementation gets wrong:
sequence exhaustion within a millisecond spins to the next one, a small
backward clock step is absorbed, and a **large one is an error** — silently
waiting out a ten-second NTP step would look like a hang.

### 2. The cache is designed for crawler traffic, not for demos

A redirect that reaches the store has already lost. Three properties, and the
third is the one usually missing:

- **Negative lookups are cached.** The load a public shortener actually
  attracts is a crawler walking random slugs, and without this every one of
  them hits the database — unbounded, because the attacker picks the keyspace.
  The negative TTL is deliberately *shorter* than the positive one, so a
  newly-created vanity link does not appear broken.
- **Concurrent misses are single-flighted.** A viral link expiring from cache
  would otherwise stampede the database precisely when traffic is highest. The
  test fires 200 concurrent requests at a cold slug and asserts the backend is
  queried **once**.
- **Stale is served on a backend error.** A slightly out-of-date redirect beats
  a 500. The accepted cost: a link deleted during a store outage keeps
  redirecting, which is why delete invalidates synchronously.

In-process rather than Redis for the first layer: ~50 ns versus ~200 µs for the
same answer. Redis is the *second* layer in a larger deployment, not the first.

### 3. Analytics never block a click

Click events go into a bounded buffer and flush in batches. When the buffer is
full, events are **dropped** and a counter increments.

That is the right trade-off, stated plainly: the alternative is blocking the
redirect, which turns an analytics outage into a site outage.
`shortener_clicks_dropped_total` is the alert worth having. Failed batches are
not retried, because a growing in-memory retry list is how an analytics hiccup
becomes an OOM kill.

ClickHouse because the query is always "aggregate over a time range" and the
data is append-only. Three schema choices do the work — `ORDER BY (slug, at)`
rather than `(at, slug)`, monthly partitions so retention is a partition drop,
and `LowCardinality` on the columns with tens of distinct values. Getting the
sort order backwards is the most common ClickHouse mistake, and
[ARCHITECTURE.md](docs/ARCHITECTURE.md) explains why.

**Privacy by construction:** client IPs are truncated to /24 or /48 before
storage and referers are reduced to their host. Geography and uniqueness
survive; the result is no longer personal data that has to be deleted on
request. CI asserts no untruncated IP ever reaches the table.

### 4. The small things that bite in production

| Decision | Why |
|---|---|
| **302 by default**, 301 opt-in | A 301 is cached by browsers essentially forever, so the link can never be retargeted or retired |
| `Cache-Control: no-store` on redirects | A cached 302 in a corporate proxy is unfixable from the server |
| **410** for a deleted link, not 404 | A crawler learns "retired" rather than "never existed" |
| A reserved-slug list | Otherwise a link can claim `/favicon.ico` or `/api` and shadow the service |
| A scheme **allow-list** on targets | A shortener accepting `javascript:` hands an XSS vector to every site that renders the link. Allow-list, because the set of dangerous schemes is not closed |
| `robots.txt: Disallow: /` | The destination should be indexed; the short link should not |
| Delete invalidates the cache | A link removed because it points somewhere harmful must stop *now*, not at TTL expiry |

## API

| Method | Path | Auth |
|---|---|---|
| `GET` | `/{slug}` | none — this is the product |
| `POST` | `/api/v1/links` | bearer token |
| `GET` | `/api/v1/links`, `/api/v1/links/{slug}` | bearer token |
| `DELETE` | `/api/v1/links/{slug}` | bearer token |
| `GET` | `/healthz` `/readyz` `/metrics` `/robots.txt` | none |

```bash
# A link with a vanity slug and an expiry
curl -X POST localhost:8080/api/v1/links -H "Authorization: Bearer $TOKEN" \
  -d '{"target":"https://example.com/launch","slug":"launch","ttl":"720h","tags":["q4"]}'
```

Creation requires a token. Without one the service **refuses to start** rather
than running open, because an unauthenticated shortener is a phishing service.

## Load testing

```bash
k6 run k6/redirect.js      # read path
k6 run k6/create.js        # write path
```

The traffic mix is the interesting part. A load test that requests one slug in
a loop measures a warm cache hit and reports a number the system will never
see. `redirect.js` is 80% hot slugs, 15% spread across the corpus, and **5%
random junk** — the crawler traffic negative caching exists for, and the share
people leave out.

The teardown prints the server's own cache counters, because a load test that
does not check the hit rate cannot tell you whether it exercised the cache or
the database. CI asserts the ratio exceeded 0.5, or the run is treated as
meaningless.

**No throughput number is published here**, deliberately. A figure from a
laptop with an in-memory store says nothing about a deployment with Postgres
across a network, and quoting one as if it did is worse than quoting none. See
[k6/README.md](k6/README.md) for what to look at instead.

## Configuration

| Variable | Default | Notes |
|---|---|---|
| `SHORTENER_TOKEN` | *(required)* | ≥16 chars; the service refuses to start without it |
| `SHORTENER_SLUG_SECRET` | — | enables slug obfuscation; **without it slugs are enumerable** |
| `SHORTENER_BASE_URL` | `http://localhost:8080` | used to build the returned short URL |
| `SHORTENER_INSTANCE` | `0` | 0–1023, **must differ per replica** |
| `SHORTENER_CACHE_TTL` | `5m` | |
| `SHORTENER_CACHE_NEGATIVE_TTL` | `10s` | shorter on purpose |
| `SHORTENER_CACHE_SIZE` | `100000` | entries |
| `SHORTENER_CLICKHOUSE` | — | HTTP endpoint; empty discards analytics |
| `SHORTENER_PERMANENT_REDIRECT` | `false` | 301 instead of 302 |
| `SHORTENER_TRUST_PROXY` | `false` | believe `X-Forwarded-For` — only behind a proxy you control |

`SHORTENER_INSTANCE` is the one to get right: two replicas sharing an instance
id can mint the same ID in the same millisecond, which is the one way this
design produces a collision.

## Layout

```
internal/shortid/    Snowflake IDs, base62, Feistel obfuscation
internal/store/      persistence behind an interface
internal/cache/      LRU + negative caching + single-flight + stale-on-error
internal/analytics/  bounded buffer, batching, ClickHouse sink and schema
internal/httpapi/    redirect hot path, management API, metrics
cmd/shortener/       the daemon
k6/                  load tests
docs/ARCHITECTURE.md the write-up
```

## Development

```bash
make test        # unit + integration
make test-race   # with the race detector (needs working cgo)
make bench
make up          # service + ClickHouse
make load        # k6 read-path test
```

CI runs the race detector, repeats the ID-uniqueness and Feistel-bijection
tests (a failure there means two links share a slug), stands the service up
against a real ClickHouse and verifies every behavioural claim in this README,
checks that clicks reach both the raw table and the rollup with **no
untruncated IPs**, and runs the k6 script with a hit-ratio assertion.

## Limitations

- **The store is in-memory.** `store.Store` is the seam, and all the cache
  logic sits above it, so Postgres is a new implementation rather than a
  redesign — but it has not been done, and the load-test latencies reflect
  that.
- **No distributed cache**, so each replica warms independently.
- **No rate limiting** on creation. A public shortener needs it;
  [ratelimit](https://github.com/MahyarNemati/ratelimit) is the companion piece.
- **No abuse scanning** of destinations against reputation feeds.
- **No multi-region story.** Snowflake IDs are region-safe by construction
  (give each region an instance-id range), but replication and failover are
  unaddressed.
- **Click ingestion is best-effort by design.** If counts must be exact, the
  buffer has to become a durable queue and the trade-off reverses.

## License

MIT — see [LICENSE](LICENSE).
