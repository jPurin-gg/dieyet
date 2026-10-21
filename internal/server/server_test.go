package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jPurin-gg/dieyet/internal/server"
	"github.com/jPurin-gg/dieyet/internal/store"
	"github.com/jPurin-gg/dieyet/internal/testdb"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := store.Open(context.Background(), testdb.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := server.New(st, server.Config{SecureCookie: false, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

func newClient(t *testing.T) *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

func do(t *testing.T, c *http.Client, method, url string, body any, headers ...string) (int, map[string]any) {
	t.Helper()
	var buf io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	data, _ := io.ReadAll(res.Body)
	if len(data) > 0 {
		json.Unmarshal(data, &out)
	}
	return res.StatusCode, out
}

func login(t *testing.T, c *http.Client, base, nickname, passphrase string) map[string]any {
	t.Helper()
	status, out := do(t, c, "POST", base+"/api/login", map[string]string{"nickname": nickname, "passphrase": passphrase})
	if status != 200 {
		t.Fatalf("login %s: %d %v", nickname, status, out)
	}
	return out
}

func TestLoginCreatesThenMatches(t *testing.T) {
	ts := newServer(t)
	c := newClient(t)

	first := login(t, c, ts.URL, "ぷりん", "ひみつ1234")
	if first["created"] != true {
		t.Fatalf("first login should create: %v", first)
	}
	id1 := first["user"].(map[string]any)["id"]

	again := login(t, c, ts.URL, "ぷりん", "ひみつ1234")
	if again["created"] != false || again["user"].(map[string]any)["id"] != id1 {
		t.Fatalf("same pair should match the same account: %v", again)
	}

	other := login(t, c, ts.URL, "ぷりん", "べつのひと")
	if other["created"] != true || other["user"].(map[string]any)["id"] == id1 {
		t.Fatalf("different passphrase must be a different account: %v", other)
	}

	if status, out := do(t, c, "POST", ts.URL+"/api/login", map[string]string{"nickname": " ", "passphrase": "ひみつ1234"}); status != 400 {
		t.Fatalf("blank nickname: %d %v", status, out)
	}
	if status, _ := do(t, c, "POST", ts.URL+"/api/login", map[string]string{"nickname": "x", "passphrase": "abc"}); status != 400 {
		t.Fatalf("short passphrase accepted: %d", status)
	}
}

func TestUnauthenticated(t *testing.T) {
	ts := newServer(t)
	c := newClient(t)
	if status, _ := do(t, c, "GET", ts.URL+"/api/me", nil); status != 401 {
		t.Fatalf("GET /api/me without session: %d", status)
	}
	if status, _ := do(t, c, "GET", ts.URL+"/api/logs?date=2026-10-20", nil); status != 401 {
		t.Fatalf("GET /api/logs without session: %d", status)
	}
}

func TestScanRecordFlow(t *testing.T) {
	ts := newServer(t)
	c := newClient(t)
	login(t, c, ts.URL, "alice", "ひみつ1234")

	// UPC-A 12 digits folds into EAN-13.
	status, out := do(t, c, "GET", ts.URL+"/api/products/012345678905", nil)
	if status != 200 || out["found"] != false {
		t.Fatalf("unknown product: %d %v", status, out)
	}
	// Register it: salt deliberately omitted => unknown, not 0.
	status, out = do(t, c, "PUT", ts.URL+"/api/products/012345678905", map[string]any{
		"name": "テスト飲料", "brand": "", "unit": "1本", "kcal": 45, "protein_g": 0, "fat_g": 0, "carbs_g": 11.2,
	})
	if status != 200 || out["barcode"] != "0012345678905" || out["salt_g"] != nil || out["protein_g"] != 0.0 {
		t.Fatalf("put product: %d %v", status, out)
	}
	status, out = do(t, c, "GET", ts.URL+"/api/products/0012345678905", nil)
	if status != 200 || out["found"] != true {
		t.Fatalf("get product: %d %v", status, out)
	}

	status, out = do(t, c, "POST", ts.URL+"/api/logs", map[string]any{"barcode": "012345678905", "servings": 2, "date": "2026-10-20"})
	if status != 201 || out["kcal"] != 90.0 || out["salt_g"] != nil || out["name"] != "テスト飲料" {
		t.Fatalf("add log: %d %v", status, out)
	}
	logID := out["id"]

	status, out = do(t, c, "POST", ts.URL+"/api/logs", map[string]any{"name": "学食のカレー", "servings": 1, "date": "2026-10-20", "kcal": 700, "salt_g": 3.5})
	if status != 201 || out["unit"] != "1食" {
		t.Fatalf("add manual log: %d %v", status, out)
	}

	status, out = do(t, c, "GET", ts.URL+"/api/logs?date=2026-10-20", nil)
	if status != 200 {
		t.Fatalf("list logs: %d %v", status, out)
	}
	totals := out["totals"].(map[string]any)
	kcal := totals["kcal"].(map[string]any)
	salt := totals["salt_g"].(map[string]any)
	if kcal["sum"] != 790.0 || kcal["unknown"] != 0.0 || salt["sum"] != 3.5 || salt["unknown"] != 1.0 || totals["complete"] != false {
		t.Fatalf("totals = %v", totals)
	}
	if n := len(out["entries"].([]any)); n != 2 {
		t.Fatalf("entries = %d", n)
	}

	status, _ = do(t, c, "DELETE", ts.URL+"/api/logs/"+jsonNum(logID), nil)
	if status != 204 {
		t.Fatalf("delete: %d", status)
	}
	_, out = do(t, c, "GET", ts.URL+"/api/logs?date=2026-10-20", nil)
	if n := len(out["entries"].([]any)); n != 1 {
		t.Fatalf("entries after delete = %d", n)
	}

	if status, out := do(t, c, "POST", ts.URL+"/api/logs", map[string]any{"barcode": "4999999999999", "date": "2026-10-20"}); status != 404 {
		t.Fatalf("log for unregistered product: %d %v", status, out)
	}
	if status, _ := do(t, c, "GET", ts.URL+"/api/logs?date=20261020", nil); status != 400 {
		t.Fatalf("bad date accepted: %d", status)
	}
}

func TestOwnership(t *testing.T) {
	ts := newServer(t)
	alice, bob := newClient(t), newClient(t)
	login(t, alice, ts.URL, "alice", "ひみつ1234")
	login(t, bob, ts.URL, "bob", "ひみつ1234")

	_, out := do(t, alice, "POST", ts.URL+"/api/logs", map[string]any{"name": "aliceの昼", "date": "2026-10-20", "kcal": 500})
	id := jsonNum(out["id"])

	if status, _ := do(t, bob, "DELETE", ts.URL+"/api/logs/"+id, nil); status != 404 {
		t.Fatalf("bob deleted alice's log: %d", status)
	}
	_, out = do(t, bob, "GET", ts.URL+"/api/logs?date=2026-10-20", nil)
	if n := len(out["entries"].([]any)); n != 0 {
		t.Fatalf("bob sees %d of alice's entries", n)
	}
	_, out = do(t, alice, "GET", ts.URL+"/api/logs?date=2026-10-20", nil)
	if n := len(out["entries"].([]any)); n != 1 {
		t.Fatalf("alice's entry missing: %d", n)
	}
}

func TestUnknownFieldsRejected(t *testing.T) {
	ts := newServer(t)
	c := newClient(t)
	login(t, c, ts.URL, "alice", "ひみつ1234")
	status, out := do(t, c, "PUT", ts.URL+"/api/products/4901777018686", map[string]any{"name": "x", "unit": "1個", "sodium": 1})
	if status != 400 || !strings.Contains(out["error"].(string), "sodium") {
		t.Fatalf("unknown field accepted: %d %v", status, out)
	}
	status, _ = do(t, c, "PUT", ts.URL+"/api/products/4901777018686", map[string]any{"name": "x", "unit": "1個", "kcal": -5})
	if status != 400 {
		t.Fatalf("negative kcal accepted: %d", status)
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	ts := newServer(t)
	c := newClient(t)
	login(t, c, ts.URL, "alice", "ひみつ1234")
	if status, _ := do(t, c, "POST", ts.URL+"/api/logout", nil); status != 204 {
		t.Fatalf("logout: %d", status)
	}
	if status, _ := do(t, c, "GET", ts.URL+"/api/me", nil); status != 401 {
		t.Fatalf("session still valid after logout: %d", status)
	}
}

func TestCrossOriginRequestsBlocked(t *testing.T) {
	ts := newServer(t)
	c := newClient(t)
	login(t, c, ts.URL, "alice", "ひみつ1234")
	status, _ := do(t, c, "POST", ts.URL+"/api/logs", map[string]any{"name": "x", "date": "2026-10-20"},
		"Sec-Fetch-Site", "cross-site", "Origin", "https://evil.example")
	if status != 403 {
		t.Fatalf("cross-site POST not blocked: %d", status)
	}
}

func TestStaticAndHeaders(t *testing.T) {
	ts := newServer(t)
	c := newClient(t)
	res, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(body), "dieyet") {
		t.Fatalf("index: %d", res.StatusCode)
	}
	if !strings.Contains(res.Header.Get("Content-Security-Policy"), "wasm-unsafe-eval") || res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("security headers missing: %v", res.Header)
	}
	res, err = c.Get(ts.URL + "/vendor/zxing-wasm/zxing_reader.wasm")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/wasm" {
		t.Fatalf("wasm: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	res, _ = c.Get(ts.URL + "/static/app.js")
	res.Body.Close()
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("app.js: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	res, _ = c.Get(ts.URL + "/manifest.webmanifest")
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/manifest+json" {
		t.Fatalf("manifest: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
}

func jsonNum(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
