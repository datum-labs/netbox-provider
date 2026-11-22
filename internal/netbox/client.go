// SPDX-License-Identifier: AGPL-3.0-only

// Package netbox is a bounded, GET-only reader for the NetBox REST API.
//
// It reads complete collections or nothing: any error discards every record
// already read for that collection. Errors never include response bodies,
// tokens, or transport details, so they are safe to put in status.
package netbox

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Limits bound one collection traversal.
type Limits struct {
	// PageBytes bounds one response body.
	PageBytes int64
	// CollectionBytes bounds all response bodies of one collection.
	CollectionBytes int64
	// Pages bounds the number of requests for one collection.
	Pages int
	// Records bounds the number of records in one collection.
	Records int
	// CollectionTimeout bounds the whole read of one collection. Each
	// request is also bounded by RequestTimeout.
	CollectionTimeout time.Duration
}

// DefaultLimits are the production bounds.
var DefaultLimits = Limits{PageBytes: 4 << 20, CollectionBytes: 64 << 20, Pages: 1000, Records: 20000, CollectionTimeout: 2 * time.Minute}

// PagesFor returns a page bound that allows records at PageSize per page,
// and never less than the default.
func PagesFor(records int) int {
	return max(DefaultLimits.Pages, records/PageSize+1)
}

// PageSize is the requested page size.
const PageSize = 200

// RequestTimeout bounds a single HTTP request.
const RequestTimeout = 30 * time.Second

// Config configures a Client.
type Config struct {
	// BaseURL is the HTTPS installation root, optionally with a path prefix.
	BaseURL string
	// Token is the API token. It is sent only in the Authorization header.
	Token string
	// Scheme is Bearer or Token.
	Scheme string
	// HTTPClient supplies the transport, for example a custom CA pool. Its
	// redirect policy, cookie jar and timeout are overridden.
	HTTPClient *http.Client
	// Limits overrides DefaultLimits when nonzero.
	Limits Limits
}

// Client reads NetBox collections.
type Client struct {
	root   *url.URL
	auth   string
	http   http.Client
	limits Limits
}

// ErrInvalidConfig marks a configuration no retry can fix.
var ErrInvalidConfig = errors.New("invalid NetBox client configuration")

// ErrCredentialsRejected marks a 401 or 403 from NetBox. Retrying with the
// same token cannot succeed, and NetBox installations commonly ban client
// addresses after repeated rejections.
var ErrCredentialsRejected = errors.New("NetBox rejected the credentials")

// Root validates and normalizes an installation root URL.
func Root(base string) (*url.URL, error) {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawPath != "" ||
		strings.Contains(parsed.Path, "..") || strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/api") {
		return nil, fmt.Errorf("%w: URL must be an HTTPS installation root without credentials, query, fragment, encoded path or /api suffix", ErrInvalidConfig)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed, nil
}

// New returns a Client.
func New(cfg Config) (*Client, error) {
	root, err := Root(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	if cfg.Scheme != "Bearer" && cfg.Scheme != "Token" {
		return nil, fmt.Errorf("%w: auth scheme must be Bearer or Token", ErrInvalidConfig)
	}
	if cfg.Token == "" || strings.ContainsAny(cfg.Token, " \t\r\n") {
		return nil, fmt.Errorf("%w: token must be nonempty and contain no whitespace", ErrInvalidConfig)
	}
	transport := http.Client{}
	if cfg.HTTPClient != nil {
		transport = *cfg.HTTPClient
	}
	transport.Timeout = RequestTimeout
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	transport.Jar = nil
	limits := cfg.Limits
	if limits == (Limits{}) {
		limits = DefaultLimits
	}
	return &Client{root: root, auth: cfg.Scheme + " " + cfg.Token, http: transport, limits: limits}, nil
}

// collectionURL returns the URL of an API collection such as dcim/devices.
func (c *Client) collectionURL(collection string) *url.URL {
	u := *c.root
	u.Path = c.root.Path + "/api/" + collection + "/"
	return &u
}

// List reads every record of a collection. filter is sent with every request
// and must be preserved by every pagination link. It returns the raw records
// in ID order, or an error and no records.
func (c *Client) List(ctx context.Context, collection string, filter url.Values) ([]json.RawMessage, error) {
	if !validCollection(collection) {
		return nil, fmt.Errorf("%w: unsupported collection", ErrInvalidConfig)
	}
	for key := range filter {
		if reservedQueryKey(key) {
			return nil, fmt.Errorf("%w: filter may not set %q", ErrInvalidConfig, key)
		}
	}
	if c.limits.CollectionTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.limits.CollectionTimeout)
		defer cancel()
	}
	endpoint := c.collectionURL(collection)
	current := *endpoint
	query := url.Values{}
	for key, values := range filter {
		query[key] = slices.Clone(values)
	}
	query.Set("limit", strconv.Itoa(PageSize))
	query.Set("ordering", "id")
	current.RawQuery = query.Encode()

	seen := map[string]bool{}
	ids := map[int64]bool{}
	records := make([]json.RawMessage, 0)
	remaining := c.limits.CollectionBytes
	count := int64(-1)
	for range c.limits.Pages {
		if seen[current.String()] {
			return nil, fmt.Errorf("%s: pagination cycle", collection)
		}
		seen[current.String()] = true
		page, err := c.fetch(ctx, &current, &remaining)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", collection, err)
		}
		if count >= 0 && page.Count != count {
			return nil, fmt.Errorf("%s: record count changed during the read", collection)
		}
		count = page.Count
		if len(page.Results) == 0 && page.Next != nil {
			return nil, fmt.Errorf("%s: empty page before the last page", collection)
		}
		for _, raw := range page.Results {
			id, err := recordID(raw)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", collection, err)
			}
			if ids[id] {
				return nil, fmt.Errorf("%s: duplicate record ID", collection)
			}
			ids[id] = true
			records = append(records, raw)
		}
		if len(records) > c.limits.Records {
			return nil, fmt.Errorf("%s: record limit exceeded", collection)
		}
		if int64(len(records)) > count {
			return nil, fmt.Errorf("%s: more records than the advertised count", collection)
		}
		if page.Next == nil {
			if int64(len(records)) != count {
				return nil, fmt.Errorf("%s: record total does not match the advertised count", collection)
			}
			slices.SortFunc(records, func(a, b json.RawMessage) int {
				left, _ := recordID(a)
				right, _ := recordID(b)
				return cmp.Compare(left, right)
			})
			return records, nil
		}
		next, err := c.nextURL(endpoint, &current, *page.Next, filter)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", collection, err)
		}
		current = *next
	}
	return nil, fmt.Errorf("%s: page limit exceeded", collection)
}

func (c *Client) nextURL(endpoint, current *url.URL, link string, filter url.Values) (*url.URL, error) {
	next, err := current.Parse(link)
	if err != nil || next.Scheme != endpoint.Scheme || next.Host != endpoint.Host || next.Path != endpoint.Path ||
		next.RawPath != "" || next.User != nil || next.Fragment != "" {
		return nil, errors.New("unsafe pagination link")
	}
	query, err := url.ParseQuery(next.RawQuery)
	if err != nil {
		return nil, errors.New("invalid pagination query")
	}
	for key, values := range query {
		switch key {
		case "limit", "offset":
			if len(values) != 1 {
				return nil, errors.New("invalid pagination query")
			}
			if _, err := strconv.ParseUint(values[0], 10, 32); err != nil {
				return nil, errors.New("invalid pagination query")
			}
		case "ordering":
			if len(values) != 1 || values[0] != "id" {
				return nil, errors.New("pagination link changed ordering")
			}
		default:
			if _, ok := filter[key]; !ok {
				return nil, errors.New("unsupported pagination query")
			}
		}
	}
	for key, want := range filter {
		got := slices.Clone(query[key])
		want = slices.Clone(want)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			return nil, errors.New("pagination link changed the scope filter")
		}
	}
	return next, nil
}

type page struct {
	Count   int64
	Next    *string
	Results []json.RawMessage
}

// DecodePage strictly decodes one paginated response body.
func DecodePage(reader io.Reader, limit int64) (page, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil || int64(len(data)) > limit {
		return page{}, errors.New("page exceeds the byte limit")
	}
	var envelope struct {
		Count   json.RawMessage `json:"count"`
		Next    json.RawMessage `json:"next"`
		Results json.RawMessage `json:"results"`
	}
	if json.Unmarshal(data, &envelope) != nil || len(envelope.Count) == 0 || len(envelope.Next) == 0 || len(envelope.Results) == 0 {
		return page{}, errors.New("invalid paginated response")
	}
	var result page
	if json.Unmarshal(envelope.Count, &result.Count) != nil || result.Count < 0 ||
		json.Unmarshal(envelope.Next, &result.Next) != nil ||
		json.Unmarshal(envelope.Results, &result.Results) != nil || result.Results == nil {
		return page{}, errors.New("invalid pagination fields")
	}
	if result.Next != nil && *result.Next == "" {
		return page{}, errors.New("empty pagination link")
	}
	return result, nil
}

func recordID(raw json.RawMessage) (int64, error) {
	var record struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(raw, &record) != nil || len(record.ID) == 0 {
		return 0, errors.New("record without an ID")
	}
	id, err := strconv.ParseInt(string(record.ID), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("record ID is not a positive integer")
	}
	return id, nil
}

func (c *Client) fetch(ctx context.Context, target *url.URL, remaining *int64) (page, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return page{}, errors.New("cannot construct request")
	}
	request.Header.Set("Authorization", c.auth)
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return page{}, ctx.Err()
		}
		return page{}, errors.New("request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return page{}, fmt.Errorf("%w: HTTP %d", ErrCredentialsRejected, response.StatusCode)
	}
	if response.StatusCode != http.StatusOK {
		return page{}, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	budgetBound := *remaining < c.limits.PageBytes
	limit := min(c.limits.PageBytes, *remaining)
	counter := &countingReader{reader: response.Body}
	result, err := DecodePage(counter, limit)
	*remaining -= counter.n
	if err != nil && budgetBound && counter.n > limit {
		return page{}, errors.New("collection byte limit exceeded")
	}
	return result, err
}

type countingReader struct {
	reader io.Reader
	n      int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.n += int64(n)
	return n, err
}

func reservedQueryKey(key string) bool {
	return key == "limit" || key == "offset" || key == "ordering" || key == "cursor" || key == "brief" ||
		key == "fields" || key == "omit" || key == "exclude" || key == "format"
}

func validCollection(collection string) bool {
	switch collection {
	case CollectionSites, CollectionLocations, CollectionRacks, CollectionDeviceTypes, CollectionDevices,
		CollectionCables, CollectionPowerPanels, CollectionPowerFeeds, CollectionIPAddresses, CollectionManufacturers:
		return true
	}
	return slices.Contains(ComponentCollections, collection) || slices.Contains(TemplateCollections, collection)
}
