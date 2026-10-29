package analytics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ClickHouseSchema is the DDL for the click table.
//
// Three choices in here are the interesting part of using ClickHouse well:
//
//   - **ORDER BY (slug, at)**, not (at, slug). ClickHouse's primary key is
//     its sort order, and the overwhelmingly common query is "clicks for this
//     slug over time". Sorting by slug first means that query reads a
//     contiguous range instead of scanning every part.
//   - **PARTITION BY toYYYYMM(at)**, so dropping old data is a partition drop
//     (instant, metadata only) rather than a DELETE (a mutation that rewrites
//     parts).
//   - **LowCardinality** on the columns with few distinct values. It is a
//     dictionary encoding: country and device have tens of values across
//     billions of rows, and storing them as plain strings wastes most of the
//     disk and all of the cache.
const ClickHouseSchema = `
CREATE TABLE IF NOT EXISTS clicks (
    at            DateTime64(3, 'UTC'),
    slug          String,
    target        String,
    referer_host  LowCardinality(String),
    user_agent    String,
    country       LowCardinality(FixedString(2)),
    device        LowCardinality(String),
    ip_prefix     String,
    status        UInt16
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(at)
ORDER BY (slug, at)
TTL toDateTime(at) + INTERVAL 13 MONTH
SETTINGS index_granularity = 8192;

-- A rollup, so the common dashboard query does not scan raw rows. The
-- aggregate state columns let partial results merge, which is what makes an
-- incremental materialized view correct rather than merely fast.
CREATE TABLE IF NOT EXISTS clicks_daily (
    day       Date,
    slug      String,
    country   LowCardinality(FixedString(2)),
    device    LowCardinality(String),
    clicks    AggregateFunction(count, UInt8),
    uniq_ips  AggregateFunction(uniq, String)
)
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(day)
ORDER BY (slug, day, country, device);

CREATE MATERIALIZED VIEW IF NOT EXISTS clicks_daily_mv TO clicks_daily AS
SELECT
    toDate(at)            AS day,
    slug,
    country,
    device,
    countState(toUInt8(1)) AS clicks,
    uniqState(ip_prefix)   AS uniq_ips
FROM clicks
WHERE status < 400
GROUP BY day, slug, country, device;
`

// ClickHouse writes events over the HTTP interface.
//
// The HTTP interface rather than the native protocol: it needs no driver, is
// trivial to debug with curl, and for batched JSONEachRow inserts the
// throughput difference does not matter. The native protocol is worth it when
// you are inserting millions of rows per second, which this is not.
type ClickHouse struct {
	endpoint string
	database string
	user     string
	password string
	client   *http.Client
}

// ClickHouseOptions configure the sink.
type ClickHouseOptions struct {
	// Endpoint is the HTTP interface, e.g. "http://localhost:8123".
	Endpoint string
	Database string
	User     string
	Password string
	Timeout  time.Duration
}

// NewClickHouse returns a sink.
func NewClickHouse(opts ClickHouseOptions) (*ClickHouse, error) {
	if opts.Endpoint == "" {
		return nil, fmt.Errorf("analytics: clickhouse endpoint is required")
	}
	if _, err := url.Parse(opts.Endpoint); err != nil {
		return nil, fmt.Errorf("analytics: bad clickhouse endpoint: %w", err)
	}
	if opts.Database == "" {
		opts.Database = "default"
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	return &ClickHouse{
		endpoint: strings.TrimRight(opts.Endpoint, "/"),
		database: opts.Database,
		user:     opts.User,
		password: opts.Password,
		client:   &http.Client{Timeout: opts.Timeout},
	}, nil
}

// Migrate applies the schema. Running DDL from the application is acceptable
// here because the schema is append-only and the statements are idempotent;
// anything that rewrites data should be a reviewed migration instead.
func (c *ClickHouse) Migrate(ctx context.Context) error {
	for _, stmt := range splitStatements(ClickHouseSchema) {
		if err := c.exec(ctx, stmt); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
	}
	return nil
}

// WriteBatch inserts events as JSONEachRow.
//
// One newline-delimited body per batch, which is the format ClickHouse ingests
// fastest over HTTP and the only one that does not require escaping a SQL
// VALUES list -- and therefore the only one with no injection surface.
func (c *ClickHouse) WriteBatch(ctx context.Context, events []Event) error {
	if len(events) == 0 {
		return nil
	}

	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, e := range events {
		row := map[string]any{
			// ClickHouse parses this into DateTime64(3).
			"at":           e.At.UTC().Format("2006-01-02 15:04:05.000"),
			"slug":         e.Slug,
			"target":       e.Target,
			"referer_host": e.RefererHost,
			"user_agent":   e.UserAgent,
			// FixedString(2) rejects anything else, so normalise here rather
			// than failing the whole batch for one odd header.
			"country":   normalizeCountry(e.Country),
			"device":    e.Device,
			"ip_prefix": e.IPPrefix,
			"status":    e.Status,
		}
		if err := enc.Encode(row); err != nil {
			return fmt.Errorf("encode event: %w", err)
		}
	}

	return c.insert(ctx, "INSERT INTO clicks FORMAT JSONEachRow", &body)
}

// Close releases idle connections.
func (c *ClickHouse) Close() error {
	c.client.CloseIdleConnections()
	return nil
}

func (c *ClickHouse) exec(ctx context.Context, query string) error {
	return c.insert(ctx, query, nil)
}

func (c *ClickHouse) insert(ctx context.Context, query string, body io.Reader) error {
	u, err := url.Parse(c.endpoint + "/")
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("database", c.database)
	q.Set("query", query)
	// Fail the insert rather than silently coercing a bad value, so a schema
	// mismatch is visible instead of becoming wrong data.
	q.Set("input_format_skip_unknown_fields", "0")
	u.RawQuery = q.Encode()

	reader := body
	if reader == nil {
		reader = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), reader)
	if err != nil {
		return err
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.password)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// ClickHouse puts a genuinely useful error in the body; losing it
		// would leave only "500" to debug with.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("clickhouse returned %s: %s", resp.Status, strings.TrimSpace(string(detail)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}

// Ping checks connectivity.
func (c *ClickHouse) Ping(ctx context.Context) error {
	return c.exec(ctx, "SELECT 1")
}

// normalizeCountry coerces a value into the two uppercase letters
// FixedString(2) requires, or two spaces when unknown.
func normalizeCountry(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) != 2 {
		return "  "
	}
	for i := 0; i < 2; i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return "  "
		}
	}
	return s
}

// splitStatements splits DDL on semicolons, skipping comments and blanks.
func splitStatements(ddl string) []string {
	var out []string
	for _, raw := range strings.Split(ddl, ";") {
		var kept []string
		for _, line := range strings.Split(raw, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "--") {
				continue
			}
			kept = append(kept, line)
		}
		stmt := strings.TrimSpace(strings.Join(kept, "\n"))
		if stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}

// Queries documents the queries the rollup is designed for, so the ORDER BY
// above can be checked against them rather than taken on trust.
const Queries = `
-- Clicks per day for one slug. Reads a contiguous range thanks to
-- ORDER BY (slug, at).
SELECT toDate(at) AS day, count() AS clicks
FROM clicks
WHERE slug = {slug:String} AND at >= now() - INTERVAL 30 DAY
GROUP BY day ORDER BY day;

-- Same question from the rollup: orders of magnitude less data read.
SELECT day, countMerge(clicks) AS clicks, uniqMerge(uniq_ips) AS visitors
FROM clicks_daily
WHERE slug = {slug:String} AND day >= today() - 30
GROUP BY day ORDER BY day;

-- Top links in the last 24 hours.
SELECT slug, count() AS clicks, uniq(ip_prefix) AS visitors
FROM clicks
WHERE at >= now() - INTERVAL 1 DAY AND status < 400
GROUP BY slug ORDER BY clicks DESC LIMIT 20;

-- Geography and device mix for one slug.
SELECT country, device, countMerge(clicks) AS clicks
FROM clicks_daily
WHERE slug = {slug:String} AND day >= today() - 7
GROUP BY country, device ORDER BY clicks DESC;

-- Referrers.
SELECT referer_host, count() AS clicks
FROM clicks
WHERE slug = {slug:String} AND at >= now() - INTERVAL 7 DAY AND referer_host != ''
GROUP BY referer_host ORDER BY clicks DESC LIMIT 20;

-- How much traffic is bots, which is usually more than people expect.
SELECT device, count() AS clicks, round(100 * count() / sum(count()) OVER (), 1) AS pct
FROM clicks
WHERE at >= now() - INTERVAL 1 DAY
GROUP BY device ORDER BY clicks DESC;
`
