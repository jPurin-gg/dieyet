// Package off looks products up on Open Food Facts as a starting point for
// manual entry. Japanese products are often missing or incomplete there, so
// a nil Suggestion is the normal case, not an error.
package off

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultBase = "https://world.openfoodfacts.org"
	userAgent   = "dieyet/0.1 (https://github.com/jPurin-gg/dieyet)"
	// Open Food Facts asks for at most 15 product reads per minute.
	maxPerMinute = 15
	positiveTTL  = 24 * time.Hour
	negativeTTL  = time.Hour
)

var ErrRateLimited = errors.New("open food facts: local rate limit reached")

type Suggestion struct {
	Name    string   `json:"name"`
	Brand   string   `json:"brand"`
	Unit    string   `json:"unit"`
	Kcal    *float64 `json:"kcal"`
	Protein *float64 `json:"protein_g"`
	Fat     *float64 `json:"fat_g"`
	Carbs   *float64 `json:"carbs_g"`
	Salt    *float64 `json:"salt_g"`
	Source  string   `json:"source"`
}

type Client struct {
	base string
	http *http.Client
	now  func() time.Time

	mu     sync.Mutex
	cache  map[string]cacheEntry
	recent []time.Time
}

type cacheEntry struct {
	s     *Suggestion
	until time.Time
}

func New(base string) *Client {
	if base == "" {
		base = DefaultBase
	}
	return &Client{
		base:  strings.TrimRight(base, "/"),
		http:  &http.Client{Timeout: 6 * time.Second},
		now:   time.Now,
		cache: map[string]cacheEntry{},
	}
}

// Lookup returns the product as a suggestion, or (nil, nil) when unknown.
func (c *Client) Lookup(ctx context.Context, barcode string) (*Suggestion, error) {
	now := c.now()
	c.mu.Lock()
	if e, ok := c.cache[barcode]; ok && now.Before(e.until) {
		c.mu.Unlock()
		return e.s, nil
	}
	cutoff := now.Add(-time.Minute)
	kept := c.recent[:0]
	for _, t := range c.recent {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	c.recent = kept
	if len(c.recent) >= maxPerMinute {
		c.mu.Unlock()
		return nil, ErrRateLimited
	}
	c.recent = append(c.recent, now)
	c.mu.Unlock()

	s, err := c.fetch(ctx, barcode)
	if err != nil {
		return nil, err
	}
	ttl := positiveTTL
	if s == nil {
		ttl = negativeTTL
	}
	c.mu.Lock()
	if len(c.cache) > 10000 {
		c.cache = map[string]cacheEntry{}
	}
	c.cache[barcode] = cacheEntry{s: s, until: now.Add(ttl)}
	c.mu.Unlock()
	return s, nil
}

type offProduct struct {
	Status  int `json:"status"`
	Product struct {
		ProductName   string         `json:"product_name"`
		ProductNameJa string         `json:"product_name_ja"`
		Brands        string         `json:"brands"`
		ServingSize   string         `json:"serving_size"`
		Nutriments    map[string]any `json:"nutriments"`
	} `json:"product"`
}

func (c *Client) fetch(ctx context.Context, barcode string) (*Suggestion, error) {
	u := c.base + "/api/v2/product/" + url.PathEscape(barcode) + "?fields=product_name,product_name_ja,brands,serving_size,nutriments"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("open food facts: HTTP %d", resp.StatusCode)
	}
	var body offProduct
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("open food facts: decode: %w", err)
	}
	if body.Status != 1 {
		return nil, nil
	}
	p := body.Product
	s := &Suggestion{Source: "openfoodfacts"}
	s.Name = strings.TrimSpace(p.ProductNameJa)
	if s.Name == "" {
		s.Name = strings.TrimSpace(p.ProductName)
	}
	s.Brand = strings.TrimSpace(p.Brands)

	// Prefer per-serving values when OFF has them and names the serving.
	keys := [5]string{"energy-kcal", "proteins", "fat", "carbohydrates", "salt"}
	suffix, unit := "_100g", "100g"
	if serving := strings.TrimSpace(p.ServingSize); serving != "" {
		for _, k := range keys {
			if _, ok := p.Nutriments[k+"_serving"]; ok {
				suffix, unit = "_serving", serving
				break
			}
		}
	}
	s.Unit = unit
	vals := [5]**float64{&s.Kcal, &s.Protein, &s.Fat, &s.Carbs, &s.Salt}
	found := false
	for i, k := range keys {
		if v := num(p.Nutriments[k+suffix]); v != nil {
			*vals[i] = v
			found = true
		}
	}
	if s.Name == "" && !found {
		return nil, nil
	}
	return s, nil
}

func num(v any) *float64 {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return nil
		}
		f = parsed
	default:
		return nil
	}
	if f < 0 {
		return nil
	}
	f = float64(int64(f*100+0.5)) / 100
	return &f
}
