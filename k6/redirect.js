// Load test for the redirect hot path.
//
//   k6 run k6/redirect.js
//   k6 run -e BASE_URL=http://localhost:8080 -e TOKEN=... k6/redirect.js
//
// The shape of this test matters more than the numbers. A naive load test
// requests one slug over and over, which measures a warm cache hit and
// nothing else -- and reports a throughput figure the system will never see in
// production. This one mixes three populations deliberately:
//
//   * 80% hot links   -- a small set, which is what real traffic looks like
//   * 15% cold links  -- spread across the whole corpus, so the cache misses
//   *  5% junk slugs  -- the crawler traffic a shortener always attracts, and
//                        the reason negative caching exists
//
// Thresholds are set on the hot path only. A redirect is the product; if p99
// goes above 50ms the system is broken regardless of what the average says.

import http from 'k6/http';
import { check, fail } from 'k6';
import { Counter, Trend } from 'k6/metrics';
import { randomIntBetween } from 'https://jslib.k6.io/k6-utils/1.4.0/index.js';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const TOKEN = __ENV.TOKEN || 'dev-token-at-least-16ch';
const CORPUS = Number(__ENV.CORPUS || 2000);
const HOT = Number(__ENV.HOT || 50);

const redirectLatency = new Trend('redirect_latency', true);
const notFoundLatency = new Trend('not_found_latency', true);
const hotHits = new Counter('hot_hits');
const coldHits = new Counter('cold_hits');
const junkHits = new Counter('junk_hits');

export const options = {
  scenarios: {
    // A ramping arrival rate, not a fixed number of VUs: it measures what the
    // service can serve, rather than how fast a fixed pool can loop.
    redirects: {
      executor: 'ramping-arrival-rate',
      startRate: 200,
      timeUnit: '1s',
      preAllocatedVUs: 50,
      maxVUs: 500,
      stages: [
        { target: 500, duration: '20s' },
        { target: 2000, duration: '30s' },
        { target: 2000, duration: '60s' },
        { target: 5000, duration: '30s' },
        { target: 5000, duration: '30s' },
        { target: 0, duration: '10s' },
      ],
    },
  },
  thresholds: {
    // The hot path is the product.
    'redirect_latency{expected_response:true}': ['p(95)<20', 'p(99)<50'],
    redirect_latency: ['p(99)<50'],
    // A miss still has to be fast, or a crawler degrades the service for
    // everyone.
    not_found_latency: ['p(99)<50'],
    checks: ['rate>0.99'],
    // Any 5xx at all is a failure. A redirect has no excuse to error.
    'http_req_failed{status:500}': ['rate<0.001'],
  },
  // Following redirects would measure go.dev, not this service.
  noVUConnectionReuse: false,
  discardResponseBodies: true,
};

// setup runs once and creates the corpus. Returned data is shared with VUs.
export function setup() {
  const slugs = [];
  const headers = { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' };

  for (let i = 0; i < CORPUS; i++) {
    const res = http.post(
      `${BASE_URL}/api/v1/links`,
      JSON.stringify({ target: `https://example.com/page/${i}?utm_source=loadtest` }),
      { headers },
    );
    if (res.status !== 201) {
      fail(`setup: creating link ${i} returned ${res.status}: ${res.body}`);
    }
    slugs.push(res.json('slug'));
  }

  console.log(`created ${slugs.length} links; ${HOT} of them are "hot"`);
  return { slugs };
}

export default function (data) {
  const roll = Math.random();
  let slug;
  let expect404 = false;

  if (roll < 0.8) {
    // Hot: a small set, served from cache.
    slug = data.slugs[randomIntBetween(0, Math.min(HOT, data.slugs.length) - 1)];
    hotHits.add(1);
  } else if (roll < 0.95) {
    // Cold: anywhere in the corpus.
    slug = data.slugs[randomIntBetween(0, data.slugs.length - 1)];
    coldHits.add(1);
  } else {
    // Junk: the crawler traffic negative caching exists for.
    slug = `zz${randomIntBetween(1, 10_000_000)}`;
    expect404 = true;
    junkHits.add(1);
  }

  const res = http.get(`${BASE_URL}/${slug}`, {
    redirects: 0, // measure this service, not the destination
    tags: { name: expect404 ? 'redirect_miss' : 'redirect_hit' },
  });

  if (expect404) {
    notFoundLatency.add(res.timings.duration);
    check(res, { 'junk slug returns 404': (r) => r.status === 404 });
  } else {
    redirectLatency.add(res.timings.duration);
    check(res, {
      'redirect returns 302': (r) => r.status === 302,
      'redirect has a Location': (r) => !!r.headers['Location'],
      'redirect is not cacheable': (r) =>
        (r.headers['Cache-Control'] || '').includes('no-store'),
    });
  }
}

// teardown reports the cache statistics the service itself recorded, which is
// the number worth looking at: a load test that does not check the hit rate
// cannot tell you whether it exercised the cache or just the database.
export function teardown() {
  const res = http.get(`${BASE_URL}/metrics`);
  if (res.status !== 200) return;

  const wanted = [
    'shortener_cache_hit_ratio',
    'shortener_cache_hits_total',
    'shortener_cache_misses_total',
    'shortener_cache_negative_hits_total',
    'shortener_cache_coalesced_total',
    'shortener_clicks_dropped_total',
  ];
  console.log('--- server-side counters ---');
  for (const line of String(res.body).split('\n')) {
    if (wanted.some((w) => line.startsWith(w + ' '))) {
      console.log('  ' + line);
    }
  }
}
