package off

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestLookup(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("User-Agent") == "" {
			t.Error("missing User-Agent")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/product/4901777018686":
			w.Write([]byte(`{"status":1,"product":{"product_name":"Onigiri","product_name_ja":"おにぎり","brands":"Seven","serving_size":"","nutriments":{"energy-kcal_100g":180,"proteins_100g":"4.5","fat_100g":1.2,"carbohydrates_100g":38.04}}}`))
		case "/api/v2/product/4901777000001":
			w.Write([]byte(`{"status":1,"product":{"product_name":"Bar","serving_size":"1本 (40g)","nutriments":{"energy-kcal_100g":500,"energy-kcal_serving":200,"salt_serving":0.3}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"status":0,"status_verbose":"product not found"}`))
		}
	}))
	defer ts.Close()
	c := New(ts.URL)
	ctx := context.Background()

	s, err := c.Lookup(ctx, "4901777018686")
	if err != nil || s == nil {
		t.Fatalf("Lookup = %+v, %v", s, err)
	}
	if s.Name != "おにぎり" || s.Brand != "Seven" || s.Unit != "100g" || *s.Kcal != 180 || *s.Protein != 4.5 || *s.Carbs != 38.04 || s.Salt != nil || s.Source != "openfoodfacts" {
		t.Fatalf("suggestion = %+v", *s)
	}
	// Per-serving values win when the serving is named.
	s, err = c.Lookup(ctx, "4901777000001")
	if err != nil || s == nil || s.Unit != "1本 (40g)" || *s.Kcal != 200 || *s.Salt != 0.3 || s.Protein != nil {
		t.Fatalf("serving suggestion = %+v, %v", s, err)
	}
	// Unknown products are (nil, nil) and cached.
	if s, err := c.Lookup(ctx, "4999999999999"); err != nil || s != nil {
		t.Fatalf("unknown = %+v, %v", s, err)
	}
	before := calls.Load()
	c.Lookup(ctx, "4901777018686")
	c.Lookup(ctx, "4999999999999")
	if calls.Load() != before {
		t.Fatal("cache miss on repeated lookups")
	}
}

func TestLocalRateLimit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()
	c := New(ts.URL)
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	for i := 0; i < maxPerMinute; i++ {
		// distinct codes: cache hits must not count against the limit
		if _, err := c.Lookup(context.Background(), fmt.Sprintf("49%011d", i)); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		now = now.Add(time.Second)
	}
	if _, err := c.Lookup(context.Background(), "4900000000099"); err != ErrRateLimited {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
	now = now.Add(time.Minute)
	if _, err := c.Lookup(context.Background(), "4900000000099"); err != nil {
		t.Fatalf("after window: %v", err)
	}
}
