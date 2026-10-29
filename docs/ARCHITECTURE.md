# Architecture

The URL shortener is the standard system-design interview question, which means
it has a standard answer that sounds good and several details that only show up
when you build it. This document is about the details.

## The shape of the problem

Two numbers determine every decision:

- **Reads vastly outnumber writes.** A link is created once and clicked many
  times; 100:1 is the figure usually quoted, and for a marketing campaign it is
  far higher. So the read path gets the engineering and the write path can be
  comparatively slow.
- **The read path is trivial work.** Resolving a slug is one key lookup
  returning a few hundred bytes. There is no computation to optimise, which
  means **all** the latency is in whatever storage you reach, and the entire
  design problem is "how often do we avoid reaching it".

Everything below follows from those two sentences.

```
                      ┌──────────────┐
   GET /AbC123 ──────▶│ in-process   │──hit──▶ 302 Location   (~50 ns)
                      │ LRU + neg.   │
                      │ cache        │
                      └──────┬───────┘
                             │ miss (single-flighted)
                             ▼
                      ┌──────────────┐
                      │   store      │  Postgres / DynamoDB
                      └──────────────┘
                             │
   click ─────────────▶ bounded buffer ──batch──▶ ClickHouse
                        (drop when full)
```

## Decision 1: how slugs are generated

This is the first real fork in the road, and the obvious options are each wrong
in a specific way.

| Approach | Problem |
|---|---|
| Random string | Needs a uniqueness check on every insert: a database round trip before you can answer, plus a retry loop under contention |
| Database sequence | A single point of coordination — every write waits on one row — and the slugs leak your total volume to anyone who can count in base 62 |
| Hash of the URL | Free deduplication, but no way to have two slugs for one URL, which breaks per-campaign analytics |
| **Snowflake ID** | **Chosen.** No coordination at all, but monotonic and therefore enumerable |

The implementation is 41 bits of millisecond timestamp, 10 bits of instance id,
12 bits of sequence:

```
 63                  22        12         0
  ┌───────────────────┬─────────┬─────────┐
  │ timestamp (41)    │ inst(10)│ seq(12) │
  └───────────────────┴─────────┴─────────┘
```

That gives 4096 IDs per millisecond per instance (≈4M/s), 1024 instances, and
~69 years of range from a 2024 epoch. Critically, **two processes can generate
IDs with no communication whatsoever** — there is no sequence to contend on and
no uniqueness check to perform.

Two clock problems have to be handled explicitly, and a Snowflake
implementation that ignores either one is broken:

- **Sequence exhausted within a millisecond**: spin until the next millisecond
  rather than emit a duplicate.
- **Clock moved backwards** (NTP step, VM migration): a small step is absorbed
  by holding the previous timestamp; a large one is an **error**. Silently
  waiting out a ten-second backward step would look like a hang, and emitting
  an ID anyway would collide with one already issued.

### The cost of Snowflake, and the fix

Monotonic IDs mean **enumerable slugs**. Mint one link, decode the base62, and
you can walk every link in the system. For a public shortener that is a real
problem: the corpus includes links people assumed were unlisted.

The fix is to obfuscate the ID before encoding, with a **4-round Feistel
network** over 64 bits. A Feistel network is a *bijection*, which is the
property that matters here: every ID maps to exactly one output and back, so
there are still no collisions and no lookup table, and generation stays
coordination-free.

It is deliberately **not** encryption. Four rounds of a multiply-xor round
function is not a cipher and does not claim to be. The requirement is "the next
slug is not guessable by adding one", not "secret against an adversary with the
corpus". Saying which of those you have achieved is the point.

The bijection is verified over 300,000 values in the tests, because if it ever
failed, two different links would share a slug.

### Why base62

`[0-9A-Za-z]`, not base64. A slug ends up in URLs, gets double-clicked to
select, and gets read aloud over the phone. `+` and `/` need escaping, and
`-`/`_` break word selection in most terminals. 11 base62 characters cover the
full uint64 range; a slug minted today is 10.

## Decision 2: the cache is the system

A redirect that reaches the database has already lost. Three properties matter,
and the third is the one usually missing.

### Negative caching

The load pattern a public shortener actually attracts is **a crawler walking
random slugs**. Without caching misses, every one of those goes straight to the
database — and they are unbounded, because the attacker picks the keyspace.

So misses are cached too, with a **shorter TTL** than hits (10s vs 5min). The
asymmetry is deliberate: a slug that does not exist *yet* may be created
imminently, and a long negative TTL would make a newly-created vanity link
appear broken. Creating a link explicitly invalidates its slug for the same
reason.

### Single-flight

A viral link expiring from cache sends every concurrent request to the database
at once — a stampede precisely when traffic is highest, and precisely when the
database can least absorb it. Concurrent misses for one slug are collapsed into
a single load; the rest wait on it. The test fires 200 concurrent requests at a
cold slug and asserts the backend is queried **once**.

### Serve stale on error

If the store is unreachable, an expired-but-present cache entry is served
rather than a 500. A slightly out-of-date redirect is almost always still
correct, and a working redirect beats an error page.

This has a failure mode worth naming: a link deleted during a store outage
keeps redirecting. That is the accepted trade-off, and it is why delete
invalidates the cache synchronously rather than relying on the TTL.

### Why in-process rather than Redis

A local map lookup is ~50 ns; a Redis round trip is ~200 µs — four thousand
times slower, for the same answer. For 10 million links at a few hundred bytes
each, the working set fits comfortably in memory.

Redis is the *second* layer in a larger deployment, shared between instances so
a cold start is not a thundering herd on the database. It is not the first.

## Decision 3: analytics must never block a click

The redirect is the product; recording the click is a side effect. So click
events go into a **bounded buffer** and are flushed in batches by a background
worker. When the buffer is full, **events are dropped** and a counter
increments.

Dropping is the right call and worth being explicit about. The alternative is
blocking the redirect, which turns an analytics outage into a site outage. The
drop counter is how you find out it happened, and
`shortener_clicks_dropped_total` being non-zero is the alert worth having.

Failed batches are **not retried**. Retrying needs a durable queue, and
silently growing an in-memory retry list is how an analytics hiccup becomes an
OOM kill.

### Why ClickHouse

The query is always "aggregate over a time range", the data is append-only and
never updated, and the volume is orders of magnitude larger than the link
table. That is the exact workload a columnar store exists for; running it in
the same Postgres as the links means the analytics load competes with the
redirect path.

Three schema choices do the real work:

- **`ORDER BY (slug, at)`, not `(at, slug)`.** ClickHouse's primary key *is*
  its sort order. The dominant query is "clicks for this slug over time", and
  sorting by slug first makes that a contiguous read instead of a scan across
  every part. Getting this backwards is the most common ClickHouse mistake.
- **`PARTITION BY toYYYYMM(at)`**, so dropping old data is a partition drop —
  instant, metadata only — rather than a `DELETE`, which is a mutation that
  rewrites parts.
- **`LowCardinality`** on country, device and referer host. It is dictionary
  encoding: those columns have tens of distinct values across billions of rows,
  and storing them as plain strings wastes most of the disk and all of the
  cache.

A materialized view maintains a daily rollup with `AggregateFunction` state
columns, so the dashboard query reads thousands of rows instead of billions.
The aggregate *state* (rather than a plain count) is what lets partial results
merge correctly as parts are combined.

### Privacy

Client IPs are truncated to /24 (IPv4) or /48 (IPv6) before storage. Geography
and rough uniqueness survive truncation; the result is no longer an identifier
for a person. Referer is reduced to its **host** — the path of a referring page
is frequently sensitive and is not needed to answer "where did traffic come
from".

This is not box-ticking: a click log with full IPs is personal data that has to
be deleted on request, and designing that obligation out of existence is
cheaper than building the deletion pipeline.

## Decision 4: small correctness details

These are the ones that come up in production rather than in interviews.

**302, not 301, by default.** A 301 is cached by browsers essentially forever,
which means the link can never be retargeted or retired. That is a surprising
thing to discover after a campaign ends. 301 is available for links you are
certain are permanent.

**`Cache-Control: no-store` on the redirect.** A cached 302 inside a corporate
proxy is unfixable from the server side.

**410 for a deleted link, not 404.** The caller learns the difference between
"never existed" and "retired", which is exactly what a crawler needs to decide
whether to come back.

**A reserved-slug list.** Without it, a link could claim `/favicon.ico` or
`/api` and start shadowing the service's own routes.

**A scheme allow-list on targets.** A shortener that accepts `javascript:` or
`data:` targets hands an XSS vector to every site that renders the link, and
`file:` reads from whoever clicks it. Allow-list `http` and `https`; do not
block-list the dangerous ones, because the list of dangerous ones is not
closed.

**`robots.txt` disallows everything.** The destination should be indexed; the
short link should not.

## What this design does not solve

- **The store is in-memory.** The `store.Store` interface is the seam, and the
  cache, single-flight and negative-caching logic all sit above it — so adding
  Postgres is a new implementation, not a redesign. But it has not been done,
  and the latency numbers from the load test reflect that.
- **No distributed cache.** Each instance has its own, so a cold start is a
  miss storm proportional to instance count.
- **No rate limiting.** A public shortener needs it on creation, or it becomes
  a phishing-link service. ([ratelimit](https://github.com/MahyarNemati/ratelimit)
  is the companion piece.)
- **No abuse scanning.** Real shorteners check destinations against
  reputation feeds before serving them.
- **No multi-region story.** Snowflake IDs are region-safe by construction
  (give each region its own instance-id range), but replication, failover and
  read-local-write-global are unaddressed.
- **Click ingestion is best-effort by design.** If click counts must be exact,
  the buffer has to become a durable queue, and the trade-off reverses.
