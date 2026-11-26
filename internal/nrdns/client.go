// Package nrdns is a small client for the NodeRings DNS API: zones and record
// sets. It is shared by the NodeRings external-dns webhook and cert-manager
// webhook; keep the copies in both repositories the same.
package nrdns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultEndpoint is the public NodeRings API.
const DefaultEndpoint = "https://api.noderings.com"

// Config configures a Client.
type Config struct {
	// Endpoint is the API base URL. Empty means DefaultEndpoint.
	Endpoint string
	// Token is a NodeRings service account token.
	Token string
	// OrganizationID, when set, is sent as X-Organization-ID with every request.
	OrganizationID string
	// UserAgent identifies the caller.
	UserAgent string
	// MaxRetries is how many times a retryable request is retried. Zero means 3.
	MaxRetries int
	// HTTPClient overrides the HTTP client (tests).
	HTTPClient *http.Client
	// Logger receives one debug line per request. Nil discards them.
	Logger *slog.Logger
	// MinBackoff is the first retry delay. Zero means 500 ms.
	MinBackoff time.Duration
}

// Client calls the NodeRings DNS API. It is safe for concurrent use.
type Client struct {
	base       *url.URL
	token      string
	org        string
	userAgent  string
	maxRetries int
	minBackoff time.Duration
	http       *http.Client
	log        *slog.Logger
}

// New returns a Client.
func New(cfg Config) (*Client, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	base, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("invalid NodeRings API URL %q: must be an absolute URL such as %s", cfg.Endpoint, DefaultEndpoint)
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("a NodeRings API token is required")
	}
	c := &Client{
		base:       base,
		token:      strings.TrimSpace(cfg.Token),
		org:        strings.TrimSpace(cfg.OrganizationID),
		userAgent:  cfg.UserAgent,
		maxRetries: cfg.MaxRetries,
		minBackoff: cfg.MinBackoff,
		http:       cfg.HTTPClient,
		log:        cfg.Logger,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 60 * time.Second}
	}
	if c.maxRetries <= 0 {
		c.maxRetries = 3
	}
	if c.minBackoff <= 0 {
		c.minBackoff = 500 * time.Millisecond
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	return c, nil
}

// Zone is a hosted DNS zone.
type Zone struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	State               string   `json:"state"`
	AssignedNameservers []string `json:"assignedNameservers"`
	DelegationOK        bool     `json:"delegationOk"`
}

// Record is one record set: every value for a name and type.
// Name is relative to the zone, "@" for the apex.
type Record struct {
	Name   string   `json:"name"`
	Type   string   `json:"type"`
	TTL    Int64    `json:"ttl"`
	RData  []string `json:"rdata"`
	System bool     `json:"system,omitempty"`
}

// Int64 decodes an integer sent as a JSON number or, as the API does for
// 64-bit fields, as a string. It encodes as a number.
type Int64 int64

// UnmarshalJSON implements json.Unmarshaler.
func (i *Int64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*i = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*i = Int64(v)
	return nil
}

// ListZones returns every zone in the organization.
func (c *Client) ListZones(ctx context.Context) ([]Zone, error) {
	var out struct {
		Zones []Zone `json:"zones"`
	}
	err := c.do(ctx, request{
		method: http.MethodGet, path: "/v1/dns/zones",
		query: url.Values{"page": {"0"}, "pageSize": {"-1"}}, retryUnavailable: true,
	}, &out)
	return out.Zones, err
}

// CreateZone hosts a new zone.
func (c *Client) CreateZone(ctx context.Context, name string) (*Zone, error) {
	var out struct {
		Zone *Zone `json:"zone"`
	}
	if err := c.do(ctx, request{method: http.MethodPost, path: "/v1/dns/zones", body: map[string]string{"name": name}}, &out); err != nil {
		return nil, err
	}
	return out.Zone, nil
}

// DeleteZone deletes a zone and every record in it.
func (c *Client) DeleteZone(ctx context.Context, id string) error {
	return c.do(ctx, request{
		method: http.MethodDelete, path: "/v1/dns/zones/" + url.PathEscape(id),
		query: url.Values{"force": {"true"}},
	}, nil)
}

// ListRecords lists a zone's record sets. name is a case-insensitive substring
// filter and rrtype an exact type filter; both are optional.
func (c *Client) ListRecords(ctx context.Context, zoneID, name, rrtype string) ([]Record, error) {
	const pageSize = 100
	var all []Record
	for page := 0; ; page++ {
		q := url.Values{"page": {strconv.Itoa(page)}, "pageSize": {strconv.Itoa(pageSize)}}
		if name != "" {
			q.Set("name", name)
		}
		if rrtype != "" {
			q.Set("type", rrtype)
		}
		var out struct {
			Records    []Record `json:"records"`
			TotalCount int      `json:"totalCount"`
		}
		if err := c.do(ctx, request{
			method: http.MethodGet, path: "/v1/dns/zones/" + url.PathEscape(zoneID) + "/records",
			query: q, retryUnavailable: true,
		}, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Records...)
		if len(out.Records) < pageSize || len(all) >= out.TotalCount {
			return all, nil
		}
	}
}

// UpsertRecord creates or replaces a record set.
func (c *Client) UpsertRecord(ctx context.Context, zoneID string, r Record) error {
	return c.do(ctx, request{
		method: http.MethodPut, path: "/v1/dns/zones/" + url.PathEscape(zoneID) + "/records",
		body: map[string]any{"record": r},
	}, nil)
}

// DeleteRecord deletes a record set. A missing record set is not an error.
func (c *Client) DeleteRecord(ctx context.Context, zoneID, name, rrtype string) error {
	err := c.do(ctx, request{
		method: http.MethodDelete, path: "/v1/dns/zones/" + url.PathEscape(zoneID) + "/records",
		query: url.Values{"name": {name}, "type": {rrtype}},
	}, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

type request struct {
	method string
	path   string
	query  url.Values
	body   any
	// retryUnavailable allows retrying HTTP 503. Record writes turn validation
	// errors into 503, so writes are not retried on it.
	retryUnavailable bool
}

func (c *Client) do(ctx context.Context, r request, out any) error {
	var payload []byte
	if r.body != nil {
		b, err := json.Marshal(r.body)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		payload = b
	}
	u := *c.base
	u.Path = strings.TrimRight(c.base.Path, "/") + r.path
	u.RawQuery = r.query.Encode()

	var lastErr error
	for attempt := 0; ; attempt++ {
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, r.method, u.String(), body)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.org != "" {
			req.Header.Set("X-Organization-ID", c.org)
		}
		if c.userAgent != "" {
			req.Header.Set("User-Agent", c.userAgent)
		}
		started := time.Now()
		resp, err := c.http.Do(req)
		if err != nil {
			c.log.DebugContext(ctx, "NodeRings API request failed", "method", r.method, "path", r.path, "error", err)
			lastErr = err
			if ctx.Err() != nil || attempt >= c.maxRetries {
				return lastErr
			}
		} else {
			respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
			_ = resp.Body.Close()
			if readErr != nil {
				return fmt.Errorf("reading response: %w", readErr)
			}
			c.log.DebugContext(ctx, "NodeRings API request", "method", r.method, "path", r.path,
				"status", resp.StatusCode, "ms", time.Since(started).Milliseconds())
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				if out == nil || len(bytes.TrimSpace(respBody)) == 0 {
					return nil
				}
				if err := json.Unmarshal(respBody, out); err != nil {
					return fmt.Errorf("decoding %s %s response: %w", r.method, r.path, err)
				}
				return nil
			}
			apiErr := decodeError(resp.StatusCode, respBody)
			lastErr = apiErr
			if !retryable(apiErr, r) || attempt >= c.maxRetries {
				return apiErr
			}
		}
		if err := sleep(ctx, c.backoff(attempt)); err != nil {
			return lastErr
		}
	}
}

func retryable(e *Error, r request) bool {
	switch e.HTTPStatus {
	case http.StatusTooManyRequests:
		return !e.IsQuota()
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		return true
	case http.StatusServiceUnavailable:
		return r.retryUnavailable
	}
	return false
}

func (c *Client) backoff(attempt int) time.Duration {
	const maxBackoff = 10 * time.Second
	d := c.minBackoff << min(attempt, 10)
	if d > maxBackoff || d <= 0 {
		d = maxBackoff
	}
	return d/2 + time.Duration(rand.Int64N(int64(d)/2+1))
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
