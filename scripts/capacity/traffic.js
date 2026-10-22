// Light, steady traffic for the #98 capacity test, spread across every site.
//
//   k6 run -e SITES=sites.tsv -e URLS=urls.tsv -e HOST_IP=10.132.0.2 \
//     -e LOGIN_PASS=... scripts/capacity/traffic.js
//
// Run it on the load generator, not the host under test. Environment:
//   SITES       sites.tsv from provision.sh (id, slug, preview domain, group)
//   URLS        urls.tsv from urls.sh (slug, path of a post)
//   HOST_IP     the host under test; every preview domain is mapped to it, so
//               no DNS is needed
//   LOGIN_USER  wp-admin user (default loadtest); LOGIN_PASS is its password
//   RATE        requests per second across all sites (default 5)
//   DURATION    how long to run (default 2h30m)
//
// The mix: about 75% cached front-end pages (the home page and posts), 22%
// uncached ones (search, the REST API, a query string no cache has seen), and
// 3% wp-admin logins. Requests are tagged type=cached|uncached|login so the
// thresholds apply to each class; the handoff's suggestions are error rate
// below 1%, p95 below 500 ms cached and below 2 s uncached.
//
// k6 can't trust a custom CA, so certificates aren't verified; the TLS
// handshakes still happen. Preview domains answer with Pebble certificates.
import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';

const sites = open(__ENV.SITES).trim().split('\n').map((l) => {
  const [, slug, domain] = l.split('\t');
  return { slug, domain };
});
const urls = {};
for (const l of open(__ENV.URLS).trim().split('\n')) {
  const [slug, path] = l.split('\t');
  (urls[slug] = urls[slug] || []).push(path);
}
const hosts = {};
for (const s of sites) hosts[s.domain] = __ENV.HOST_IP;

const user = __ENV.LOGIN_USER || 'loadtest';
const pass = __ENV.LOGIN_PASS;
const failedChecks = new Counter('pf98_failed_checks');

export const options = {
  hosts,
  insecureSkipTLSVerify: true,
  noConnectionReuse: false,
  scenarios: {
    light: {
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.RATE || 5),
      timeUnit: '1s',
      duration: __ENV.DURATION || '2h30m',
      preAllocatedVUs: 20,
      maxVUs: 200,
    },
  },
  thresholds: {
    http_req_failed: [{ threshold: 'rate<0.01', abortOnFail: false }],
    'http_req_duration{type:cached}': [{ threshold: 'p(95)<500', abortOnFail: false }],
    'http_req_duration{type:uncached}': [{ threshold: 'p(95)<2000', abortOnFail: false }],
  },
  summaryTrendStats: ['avg', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

const pick = (a) => a[Math.floor(Math.random() * a.length)];

function cached(base, site) {
  const own = urls[site.slug] || [];
  const path = own.length && Math.random() < 0.7 ? pick(own) : '/';
  return http.get(base + path, { tags: { type: 'cached' } });
}

function uncached(base) {
  const r = Math.random();
  let path;
  if (r < 0.4) path = '/?s=' + pick(['lorem', 'ipsum', 'dolor', 'amet', 'post']);
  else if (r < 0.8) path = '/wp-json/wp/v2/posts?per_page=5&_fields=id,title&x=' + Math.floor(Math.random() * 1e6);
  else path = '/?nocache=' + Math.floor(Math.random() * 1e9);
  return http.get(base + path, { tags: { type: 'uncached' } });
}

function login(base) {
  const jar = http.cookieJar();
  jar.set(base, 'wordpress_test_cookie', 'WP Cookie check');
  const res = http.post(
    base + '/wp-login.php',
    { log: user, pwd: pass, 'wp-submit': 'Log In', redirect_to: base + '/wp-admin/', testcookie: '1' },
    { tags: { type: 'login' }, redirects: 5 },
  );
  return res;
}

export default function () {
  const site = pick(sites);
  const base = 'https://' + site.domain;
  const r = Math.random();
  let res;
  if (r < 0.75) res = cached(base, site);
  else if (r < 0.97) res = uncached(base);
  else res = login(base);
  const ok = check(res, { 'status is 2xx': (x) => x.status >= 200 && x.status < 300 });
  if (!ok) failedChecks.add(1);
}

export function handleSummary(data) {
  const out = __ENV.SUMMARY || 'k6-summary.json';
  return { [out]: JSON.stringify(data, null, 2), stdout: textSummary(data) };
}

// A short summary of the numbers the report needs, so the terminal is readable
// without k6's jslib.
function textSummary(data) {
  const m = data.metrics;
  const line = (name, label) => {
    const v = m[name] && m[name].values;
    return v ? `${label}: avg ${v.avg.toFixed(0)} ms, med ${v.med.toFixed(0)}, p95 ${v['p(95)'].toFixed(0)}, p99 ${v['p(99)'].toFixed(0)}, max ${v.max.toFixed(0)}\n` : '';
  };
  return (
    `requests: ${m.http_reqs.values.count}, failed: ${(m.http_req_failed.values.rate * 100).toFixed(3)}%\n` +
    line('http_req_duration{type:cached}', 'cached  ') +
    line('http_req_duration{type:uncached}', 'uncached') +
    line('http_req_duration{type:login}', 'login   ')
  );
}
