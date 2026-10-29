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

# The short run CI uses: same executor, same thresholds, a tenth of the rate
k6 run -e PROFILE=smoke k6/redirect.js
```

## Profiles, and why not `--stage`

`redirect.js` has two profiles, picked with `-e PROFILE=full|smoke`. They
share one executor on purpose.

It is tempting to shorten a run from the command line with k6's `--stage`
instead. Do not: `--stage` *replaces* the scenario with a `ramping-vus` one,
so a rate-limited arrival-rate test becomes N concurrent clients looping as
fast as they can. On a two-core CI runner that meant 500 VUs, 13k requests a
second and a p99 well past the 50 ms threshold — a failure that said nothing
about the service and everything about the runner. The profile keeps the
arrival-rate executor and only lowers the rate, so the same thresholds stay
meaningful.

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

1. **`redirect_latency` p99**, and `http_req_duration{name:redirect_hit}`.
   A redirect is the product. The threshold is 50 ms and it should normally
   be far below that, because a hit is a map lookup. Note the tag: a custom
   `Trend` carries only the tags put on it, so a sub-metric like
   `redirect_latency{expected_response:true}` has no samples at all and its
   threshold passes whatever the service does. Thresholds on built-in
   metrics can use the request tags; thresholds on custom ones cannot.
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
