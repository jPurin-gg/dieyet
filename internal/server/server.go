// Package server is the HTTP layer: JSON API under /api, the embedded
// frontend everywhere else, sessions, CSRF and the security headers.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jPurin-gg/dieyet/internal/auth"
	"github.com/jPurin-gg/dieyet/internal/off"
	"github.com/jPurin-gg/dieyet/internal/store"
	"github.com/jPurin-gg/dieyet/web"
)

type Config struct {
	// SecureCookie must be true in production (the browser talks HTTPS to
	// Cloudflare). Set false only for plain-http local development.
	SecureCookie bool
	SessionTTL   time.Duration // default 180 days
	OFF          *off.Client   // nil disables Open Food Facts suggestions
	Now          func() time.Time
	Logger       *slog.Logger
}

type Server struct {
	store   *store.Store
	cfg     Config
	byPair  *auth.Limiter // login attempts per (ip, nickname)
	byIP    *auth.Limiter // login attempts per ip
	handler http.Handler
}

func New(st *store.Store, cfg Config) *Server {
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 180 * 24 * time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Server{
		store:  st,
		cfg:    cfg,
		byPair: auth.NewLimiter(10, 10*time.Minute),
		byIP:   auth.NewLimiter(60, 10*time.Minute),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	static := http.FileServerFS(web.Files)
	mux.HandleFunc("GET /manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		// Go's mime table does not know .webmanifest.
		w.Header().Set("Content-Type", "application/manifest+json")
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, web.Files, "manifest.webmanifest")
	})
	mux.Handle("GET /static/", cacheControl("no-cache", static))
	mux.Handle("GET /vendor/", cacheControl("public, max-age=86400", static))

	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/me", s.requireUser(s.handleMe))
	mux.HandleFunc("GET /api/products/{barcode}", s.requireUser(s.handleGetProduct))
	mux.HandleFunc("PUT /api/products/{barcode}", s.requireUser(s.handlePutProduct))
	mux.HandleFunc("GET /api/logs", s.requireUser(s.handleListLogs))
	mux.HandleFunc("POST /api/logs", s.requireUser(s.handleAddLog))
	mux.HandleFunc("DELETE /api/logs/{id}", s.requireUser(s.handleDeleteLog))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "そのAPIはありません")
	})

	// Cross-origin POST/PUT/DELETE are rejected from Sec-Fetch-Site / Origin
	// (Go 1.25). Together with SameSite=Lax cookies this is the CSRF defence.
	var h http.Handler = http.NewCrossOriginProtection().Handler(mux)
	h = http.MaxBytesHandler(h, 1<<20)
	h = securityHeaders(h)
	h = s.logRequests(h)
	s.handler = h
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// ---------------------------------------------------------------- middleware

func securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; " +
		"img-src 'self' data: blob:; media-src 'self' blob:; connect-src 'self'; worker-src 'self' blob:; " +
		"manifest-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(self), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

func cacheControl(value string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", value)
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.cfg.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.cfg.Logger.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", s.cfg.Now().Sub(start).Milliseconds())
	})
}

// ---------------------------------------------------------------- sessions

func (s *Server) cookieName() string {
	if s.cfg.SecureCookie {
		return "__Host-dieyet_session"
	}
	return "dieyet_session"
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(s.cfg.SessionTTL / time.Second),
		HttpOnly: true,
		Secure:   s.cfg.SecureCookie,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cfg.SecureCookie,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) requireUser(h func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(s.cookieName())
		if err != nil || c.Value == "" {
			writeErr(w, http.StatusUnauthorized, "ログインしてください")
			return
		}
		u, err := s.store.UserBySession(r.Context(), c.Value, s.cfg.Now())
		if errors.Is(err, store.ErrNotFound) {
			s.clearSessionCookie(w)
			writeErr(w, http.StatusUnauthorized, "ログインしてください")
			return
		}
		if err != nil {
			s.internalError(w, err)
			return
		}
		h(w, r, u)
	}
}

// ---------------------------------------------------------------- handlers: auth

type loginRequest struct {
	Nickname   string `json:"nickname"`
	Passphrase string `json:"passphrase"`
}

// handleLogin signs in when the (nickname, passphrase) pair matches an account
// and otherwise creates a new account for that pair, as decided in docs/技術選定.md.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	nickname := strings.TrimSpace(req.Nickname)
	if n := utf8.RuneCountInString(nickname); n == 0 || n > 30 || hasControl(nickname) {
		writeErr(w, http.StatusBadRequest, "ニックネームは1〜30文字で入力してください")
		return
	}
	if n := utf8.RuneCountInString(req.Passphrase); n < 4 || n > 200 || hasControl(req.Passphrase) {
		writeErr(w, http.StatusBadRequest, "合言葉は4〜200文字で入力してください")
		return
	}
	ip := clientIP(r)
	if !s.byIP.Allow(ip) || !s.byPair.Allow(ip+"\x00"+nickname) {
		writeErr(w, http.StatusTooManyRequests, "試行回数が多すぎます。しばらく待ってから試してください")
		return
	}

	ctx := r.Context()
	now := s.cfg.Now()
	creds, err := s.store.UsersByNickname(ctx, nickname)
	if err != nil {
		s.internalError(w, err)
		return
	}
	var user store.User
	created := true
	for _, c := range creds {
		if auth.Verify(req.Passphrase, c.Hash) {
			user, created = c.User, false
			break
		}
	}
	if created {
		if user, err = s.store.CreateUser(ctx, nickname, auth.Hash(req.Passphrase), now); err != nil {
			s.internalError(w, err)
			return
		}
	} else if err := s.store.TouchLogin(ctx, user.ID, now); err != nil {
		s.internalError(w, err)
		return
	}

	token := auth.NewToken()
	expires := now.Add(s.cfg.SessionTTL)
	if err := s.store.CreateSession(ctx, token, user.ID, now, expires); err != nil {
		s.internalError(w, err)
		return
	}
	s.setSessionCookie(w, token, expires)
	writeJSON(w, http.StatusOK, map[string]any{"user": user, "created": created})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.cookieName()); err == nil && c.Value != "" {
		if err := s.store.DeleteSession(r.Context(), c.Value); err != nil {
			s.internalError(w, err)
			return
		}
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, u store.User) {
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

// ---------------------------------------------------------------- handlers: products

type productRequest struct {
	Name   string   `json:"name"`
	Brand  string   `json:"brand"`
	Unit   string   `json:"unit"`
	Kcal   *float64 `json:"kcal"`
	Prot   *float64 `json:"protein_g"`
	Fat    *float64 `json:"fat_g"`
	Carbs  *float64 `json:"carbs_g"`
	Salt   *float64 `json:"salt_g"`
	Source string   `json:"source"`
}

func (s *Server) handleGetProduct(w http.ResponseWriter, r *http.Request, _ store.User) {
	code, ok := normalizeBarcode(r.PathValue("barcode"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "バーコードは8桁か12〜14桁の数字です")
		return
	}
	p, err := s.store.Product(r.Context(), code)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"found": true, "product": p, "suggestion": nil})
		return
	}
	if !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, err)
		return
	}
	// Unknown here: offer Open Food Facts data as a starting point for manual
	// entry. Nothing is stored until the user saves the product.
	var suggestion *off.Suggestion
	if s.cfg.OFF != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 7*time.Second)
		defer cancel()
		sg, err := s.cfg.OFF.Lookup(ctx, code)
		if err != nil {
			s.cfg.Logger.Warn("open food facts lookup failed", "barcode", code, "err", err)
		}
		suggestion = sg
	}
	writeJSON(w, http.StatusOK, map[string]any{"found": false, "product": nil, "suggestion": suggestion})
}

func (s *Server) handlePutProduct(w http.ResponseWriter, r *http.Request, u store.User) {
	code, ok := normalizeBarcode(r.PathValue("barcode"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "バーコードは8桁か12〜14桁の数字です")
		return
	}
	var req productRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p := store.Product{
		Barcode:   code,
		Name:      strings.TrimSpace(req.Name),
		Brand:     strings.TrimSpace(req.Brand),
		Unit:      strings.TrimSpace(req.Unit),
		Nutrients: store.Nutrients{Kcal: req.Kcal, Protein: req.Prot, Fat: req.Fat, Carbs: req.Carbs, Salt: req.Salt},
		Source:    req.Source,
	}
	if n := utf8.RuneCountInString(p.Name); n == 0 || n > 200 {
		writeErr(w, http.StatusBadRequest, "商品名は1〜200文字で入力してください")
		return
	}
	if utf8.RuneCountInString(p.Brand) > 200 {
		writeErr(w, http.StatusBadRequest, "メーカー名が長すぎます")
		return
	}
	if n := utf8.RuneCountInString(p.Unit); n == 0 || n > 50 {
		writeErr(w, http.StatusBadRequest, "栄養成分の単位（例: 1個、100g）を入力してください")
		return
	}
	if msg := checkNutrients(p.Nutrients); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	switch p.Source {
	case "", "manual":
		p.Source = "manual"
	case "openfoodfacts":
	default:
		writeErr(w, http.StatusBadRequest, "source が不正です")
		return
	}
	saved, err := s.store.PutProduct(r.Context(), p, u.ID, s.cfg.Now())
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// ---------------------------------------------------------------- handlers: logs

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

type logRequest struct {
	Barcode  *string  `json:"barcode"`
	Name     string   `json:"name"`
	Unit     string   `json:"unit"`
	Servings *float64 `json:"servings"`
	Date     string   `json:"date"`     // YYYY-MM-DD in the phone's local time
	EatenAt  string   `json:"eaten_at"` // optional RFC 3339; defaults to now
	Kcal     *float64 `json:"kcal"`     // per unit; only for entries without a barcode
	Prot     *float64 `json:"protein_g"`
	Fat      *float64 `json:"fat_g"`
	Carbs    *float64 `json:"carbs_g"`
	Salt     *float64 `json:"salt_g"`
}

func (s *Server) handleAddLog(w http.ResponseWriter, r *http.Request, u store.User) {
	var req logRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !validDate(req.Date) {
		writeErr(w, http.StatusBadRequest, "date は YYYY-MM-DD で指定してください")
		return
	}
	servings := 1.0
	if req.Servings != nil {
		servings = *req.Servings
	}
	if !(servings > 0 && servings <= 100) {
		writeErr(w, http.StatusBadRequest, "量は 0 より大きく 100 以下で入力してください")
		return
	}
	now := s.cfg.Now()
	eatenAt := now
	if req.EatenAt != "" {
		t, err := time.Parse(time.RFC3339, req.EatenAt)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "eaten_at は RFC 3339 形式で指定してください")
			return
		}
		eatenAt = t
	}

	n := store.NewLog{UserID: u.ID, Servings: servings, EatenOn: req.Date, EatenAt: eatenAt}
	if req.Barcode != nil {
		code, ok := normalizeBarcode(*req.Barcode)
		if !ok {
			writeErr(w, http.StatusBadRequest, "バーコードは8桁か12〜14桁の数字です")
			return
		}
		p, err := s.store.Product(r.Context(), code)
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "先に商品を登録してください")
			return
		}
		if err != nil {
			s.internalError(w, err)
			return
		}
		n.Barcode = &code
		n.Name, n.Unit = p.Name, p.Unit
		n.Nutrients = p.Nutrients.Scale(servings)
	} else {
		n.Name = strings.TrimSpace(req.Name)
		n.Unit = strings.TrimSpace(req.Unit)
		if c := utf8.RuneCountInString(n.Name); c == 0 || c > 200 {
			writeErr(w, http.StatusBadRequest, "食べたものの名前を1〜200文字で入力してください")
			return
		}
		if n.Unit == "" {
			n.Unit = "1食"
		}
		if utf8.RuneCountInString(n.Unit) > 50 {
			writeErr(w, http.StatusBadRequest, "単位が長すぎます")
			return
		}
		per := store.Nutrients{Kcal: req.Kcal, Protein: req.Prot, Fat: req.Fat, Carbs: req.Carbs, Salt: req.Salt}
		if msg := checkNutrients(per); msg != "" {
			writeErr(w, http.StatusBadRequest, msg)
			return
		}
		n.Nutrients = per.Scale(servings)
	}
	l, err := s.store.AddLog(r.Context(), n, now)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, l)
}

func (s *Server) handleListLogs(w http.ResponseWriter, r *http.Request, u store.User) {
	date := r.URL.Query().Get("date")
	if !validDate(date) {
		writeErr(w, http.StatusBadRequest, "date は YYYY-MM-DD で指定してください")
		return
	}
	logs, err := s.store.LogsForDay(r.Context(), u.ID, date)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"date": date, "entries": logs, "totals": store.Summarize(logs)})
}

func (s *Server) handleDeleteLog(w http.ResponseWriter, r *http.Request, u store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "id が不正です")
		return
	}
	err = s.store.DeleteLog(r.Context(), u.ID, id, s.cfg.Now())
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "その記録はありません")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- handlers: static

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFileFS(w, r, web.Files, "index.html")
}

// ---------------------------------------------------------------- helpers

func (s *Server) internalError(w http.ResponseWriter, err error) {
	s.cfg.Logger.Error("internal error", "err", err)
	writeErr(w, http.StatusInternalServerError, "サーバー内部でエラーが起きました")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// readJSON decodes exactly one JSON object and rejects unknown fields, so a
// misspelled "Salt" or "sodium" is an error instead of a silently dropped value.
func readJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("リクエストの形式が不正です: %s", err.Error())
	}
	if dec.More() {
		return errors.New("リクエストの形式が不正です: 余分なデータがあります")
	}
	return nil
}

func checkNutrients(n store.Nutrients) string {
	for _, v := range []*float64{n.Kcal, n.Protein, n.Fat, n.Carbs, n.Salt} {
		if v != nil && !(*v >= 0 && *v <= 100000) {
			return "栄養成分の値は 0 以上で入力してください（不明なら空欄）"
		}
	}
	return ""
}

// normalizeBarcode accepts EAN-8/EAN-13/UPC-A/GTIN-14 digits and folds UPC-A
// (12 digits) into the equivalent EAN-13 so one product has one key.
func normalizeBarcode(code string) (string, bool) {
	code = strings.TrimSpace(code)
	for _, r := range code {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	switch len(code) {
	case 8, 13, 14:
		return code, true
	case 12:
		return "0" + code, true
	}
	return "", false
}

func validDate(s string) bool {
	if !dateRe.MatchString(s) {
		return false
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// clientIP trusts CF-Connecting-IP only when the request arrived from loopback,
// i.e. through cloudflared on the same host.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" {
			return cf
		}
	}
	return host
}
