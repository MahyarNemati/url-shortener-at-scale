// Load test for link creation.
//
//   k6 run k6/create.js
//
// Writes are far less frequent than reads in a shortener -- the usual figure
// quoted in system-design interviews is 100:1 -- so this is a separate, much
// smaller test. It exists to check two things:
//
//   1. ID generation does not become a bottleneck or produce duplicates under
//      concurrency. A sequence in a database would serialise here; a
//      Snowflake generator should not.
//   2. Creation latency stays bounded while the read test is running, which is
//      the interesting case: a write path that degrades under read load means
//      they are sharing something they should not.

import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const TOKEN = __ENV.TOKEN || 'dev-token-at-least-16ch';

const duplicateSlugs = new Counter('duplicate_slugs');
const seen = new Set();

export const options = {
  scenarios: {
    creates: {
      executor: 'constant-arrival-rate',
      rate: 200,
      timeUnit: '1s',
      duration: '60s',
      preAllocatedVUs: 20,
      maxVUs: 100,
    },
  },
  thresholds: {
    http_req_duration: ['p(95)<50', 'p(99)<100'],
    checks: ['rate>0.99'],
    // Any duplicate slug is a correctness failure, not a performance one.
    duplicate_slugs: ['count==0'],
  },
};

export default function () {
  const res = http.post(
    `${BASE_URL}/api/v1/links`,
    JSON.stringify({
      target: `https://example.com/p/${__VU}/${__ITER}`,
      tags: ['loadtest'],
    }),
    {
      headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' },
      tags: { name: 'create_link' },
    },
  );

  const ok = check(res, {
    'created': (r) => r.status === 201,
    'has a slug': (r) => !!r.json('slug'),
    'has a short url': (r) => !!r.json('short_url'),
  });

  if (ok) {
    // Per-VU only: k6 VUs do not share memory, so this catches duplicates
    // within one VU's stream. Cross-VU uniqueness is covered by the Go
    // concurrency test, which can actually share a map.
    const slug = res.json('slug');
    if (seen.has(slug)) {
      duplicateSlugs.add(1);
    }
    seen.add(slug);
  }
}
