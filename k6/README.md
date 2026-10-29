# Load tests

```bash
# Start the service
SHORTENER_TOKEN=dev-token-at-least-16ch go run ./cmd/shortener

# Read path (the one that matters)
k6 run k6/redirect.js

# Write path
k6 run k6/create.js

# Against another environment
k6 run -e BASE_URL=https://staging.example -e TOKEN=$TOKEN k6/redirect.js
```

## Why the traffic mix is what it is

A load test that requests one slug in a loop measures a warm cache hit and
reports a throughput figure the system will never see. `redirect.js` mixes:

| Share | Population | What it exercises |
|---|---|---|
| 80% | 50 hot slugs | the cache hit path, which is most real traffic |
| 15% | the whole corpus | cache misses and the single-flight path |
| 5% | random junk slugs | negative caching, i.e. crawler traffic |

That last 5% is the one people leave out, and it is the load pattern a public
shortener genuinely attracts. Without negative caching it goes straight to the
database on every request.

## Reading the results

The numbers to look at, in order:

1. **`redirect_latency` p99.** A redirect is the product. The threshold is
   50 ms and it should normally be far below that, because a hit is a map
   lookup.
2. **`shortener_cache_hit_ratio`** from the teardown output. If this is low,
   the test did not exercise the cache and the latency figures are measuring
   the database instead. Raise `HOT` or lower `CORPUS`.
3. **`shortener_cache_coalesced_total`.** Non-zero means concurrent misses for
   the same slug were collapsed into one query, which is the stampede
   protection working.
4. **`shortener_clicks_dropped_total`.** Non-zero means the analytics buffer
   filled. The redirect was still served — that is the design — but you are
   losing click data and should raise the buffer or batch size.

**Do not publish a throughput number without the hardware and the mix.** A
figure from a laptop with an in-memory store says nothing about a deployment
with Postgres across a network, and quoting one as if it did is worse than
quoting none.

## Tuning knobs

```bash
k6 run -e CORPUS=10000 -e HOT=20 k6/redirect.js   # colder, more misses
k6 run -e CORPUS=200   -e HOT=200 k6/redirect.js  # everything hot
```
