package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"image/jpeg"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var httpClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,

		DialContext: (&net.Dialer{
			Timeout:   8 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,

		MaxIdleConns:        6,
		MaxIdleConnsPerHost: 3,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 8 * time.Second,
	},
}

var copyBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 32*1024)
		return &b
	},
}

const pinterestSearchURL = "https://www.pinterest.com/resource/BaseSearchResource/get/"
const cookieName = "pinata_bm"

// ---------- bookmarks types / config ----------
type BookmarkEntry struct {
	Type  string `json:"type"`  // "q" or "img"
	Value string `json:"value"` // query or image URL
}

var bookmarkKey []byte
var bookmarkingEnabled bool
var disableReverse bool
var chunkedMode bool
var imageBackendBase string
var chunkSize = 8
var chunkWorkers = 4

const maxBookmarks = 30
const maxItemLen = 256

// ---------- init: read env ----------
func init() {
	// PINATA_BOOKMARK_KEY: base64 32-byte key
	if kb := os.Getenv("PINATA_BOOKMARK_KEY"); kb != "" {
		if decoded, err := base64.StdEncoding.DecodeString(kb); err == nil && len(decoded) == 32 {
			bookmarkKey = decoded
			bookmarkingEnabled = true
			log.Println("Bookmarking enabled")
		} else {
			bookmarkingEnabled = false
			log.Println("PINATA_BOOKMARK_KEY present but invalid; bookmarking disabled")
		}
	} else {
		bookmarkingEnabled = false
		log.Println("PINATA_BOOKMARK_KEY not set; bookmarking disabled")
	}

	// PINATA_DISABLE_REVERSE: "1"/"true"/"yes" disables reverse search
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PINATA_DISABLE_REVERSE"))) {
	case "1", "true", "yes":
		disableReverse = true
		log.Println("Reverse image search disabled via PINATA_DISABLE_REVERSE")
	default:
		disableReverse = false
	}

	// CHUNK enables chunked/threaded rendering of result cards.
	// Examples:
	//   CHUNK=0/false/no/off -> disabled
	//   CHUNK=1/true/yes/on  -> enabled with default chunk size
	//   CHUNK=12             -> enabled with 12-item batches
	if raw := strings.TrimSpace(os.Getenv("CHUNK")); raw != "" {
		switch strings.ToLower(raw) {
		case "0", "false", "no", "off":
			chunkedMode = false
		default:
			chunkedMode = true
			if n, err := strconv.Atoi(raw); err == nil && n > 0 {
				chunkSize = n
			}
		}
	}
	if chunkSize < 4 {
		chunkSize = 4
	}
	if chunkSize > 16 {
		chunkSize = 16
	}
	cpus := runtime.GOMAXPROCS(0)
	if cpus < 1 {
		cpus = 1
	}
	if cpus > 4 {
		cpus = 4
	}
	chunkWorkers = cpus
	if chunkedMode {
		log.Printf("Chunked mode enabled: chunkSize=%d workers=%d", chunkSize, chunkWorkers)
	}
	imageBackendBase = strings.TrimRight(strings.TrimSpace(os.Getenv("PINATA_IMAGE_BACKEND")), "/")
}

// ---------- encryption helpers (AES-GCM) ----------
func encryptBookmarks(entries []BookmarkEntry) (string, error) {
	if !bookmarkingEnabled {
		return "", nil
	}
	plain, err := json.Marshal(entries)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(bookmarkKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, plain, nil)
	return base64.RawURLEncoding.EncodeToString(ct), nil
}

func decryptBookmarks(encoded string) ([]BookmarkEntry, error) {
	if !bookmarkingEnabled {
		return nil, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(bookmarkKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(data) < ns {
		return nil, io.ErrUnexpectedEOF
	}
	nonce := data[:ns]
	ct := data[ns:]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, err
	}
	// try new format first ([]BookmarkEntry)
	var entries []BookmarkEntry
	if err := json.Unmarshal(plain, &entries); err == nil {
		return entries, nil
	}
	// fallback to legacy []string
	var arr []string
	if err := json.Unmarshal(plain, &arr); err == nil {
		out := make([]BookmarkEntry, 0, len(arr))
		for _, s := range arr {
			out = append(out, BookmarkEntry{Type: "q", Value: s})
		}
		return out, nil
	}
	return nil, io.ErrUnexpectedEOF
}

// ---------- cookie helpers ----------
func readBookmarksFromReq(r *http.Request) []BookmarkEntry {
	if !bookmarkingEnabled {
		return nil
	}
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	entries, err := decryptBookmarks(c.Value)
	if err != nil {
		return nil
	}
	return entries
}

func setBookmarksCookie(w http.ResponseWriter, entries []BookmarkEntry) {
	if !bookmarkingEnabled {
		return
	}
	seen := map[string]bool{}
	out := make([]BookmarkEntry, 0, len(entries))
	for _, e := range entries {
		v := strings.TrimSpace(e.Value)
		if v == "" {
			continue
		}
		if len(v) > maxItemLen {
			v = v[:maxItemLen]
		}
		if e.Type != "q" && e.Type != "img" {
			e.Type = "q"
		}
		key := e.Type + "|" + v
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, BookmarkEntry{Type: e.Type, Value: v})
		if len(out) >= maxBookmarks {
			break
		}
	}
	enc, err := encryptBookmarks(out)
	if err != nil {
		return
	}
	c := &http.Cookie{
		Name:     cookieName,
		Value:    enc,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// Secure: true, // enable in production with HTTPS
		MaxAge: 60 * 60 * 24 * 365 * 10,
	}
	http.SetCookie(w, c)
}

func clearBookmarksCookie(w http.ResponseWriter) {
	c := &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	}
	http.SetCookie(w, c)
}

// ---------- redirect helper ----------

// safeNext returns next if it is a path on this site, or "/" otherwise, so
// form "next" values can't redirect users to another site.
func safeNext(next string) string {
	// Backslashes are rejected anywhere: browsers treat them as "/", and
	// http.Redirect's path cleaning can turn "/./\\x" into "/\\x" ("//x").
	if next == "" || next[0] != '/' || strings.HasPrefix(next, "//") || strings.ContainsRune(next, '\\') {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return next
}

// validate and normalize a hex color; returns "#rrggbb" or empty string if invalid
func normalizeHexColor(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// allow with or without leading '#'
	if strings.HasPrefix(s, "#") {
		s = s[1:]
	}
	if len(s) != 6 {
		return ""
	}
	for _, r := range s {
		if !(('0' <= r && r <= '9') || ('a' <= r && r <= 'f') || ('A' <= r && r <= 'F')) {
			return ""
		}
	}
	return "#" + strings.ToLower(s)
}

// hex to rgba string with alpha
func hexToRGBA(hex string, alpha float64) string {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) != 6 {
		return "rgba(124,58,237,0.12)" // fallback purple-ish
	}
	rv, _ := strconv.ParseUint(hex[0:2], 16, 8)
	gv, _ := strconv.ParseUint(hex[2:4], 16, 8)
	bv, _ := strconv.ParseUint(hex[4:6], 16, 8)
	return fmt.Sprintf("rgba(%d,%d,%d,%.2f)", rv, gv, bv, alpha)
}

// get theme variables from cookies; returns accent (hex) and imgScale (float like "1.00")
func getThemeVars(r *http.Request) (string, string) {
	// Default accent
	accent := "#7c3aed"
	imgScale := "1.00" // default 100%
	if c, err := r.Cookie("pinata_accent"); err == nil {
		if val := normalizeHexColor(c.Value); val != "" {
			accent = val
		}
	}
	if c2, err := r.Cookie("pinata_img_scale"); err == nil {
		// expect integer percent
		if p, err := strconv.Atoi(c2.Value); err == nil {
			if p < 50 {
				p = 50
			}
			if p > 200 {
				p = 200
			}
			// convert to scale
			scale := float64(p) / 100.0
			imgScale = fmt.Sprintf("%.2f", scale)
		}
	}
	return accent, imgScale
}

// ---------- CSS (uses CSS vars; defaults are present but overridden per-request via inline style) ----------
const cssContent = `:root{--bg:#0b0f17;--muted:#94a3b8;--text:#e6e6ff;--accent:#7c3aed;--accent-rgba:rgba(124,58,237,0.12);--img-scale:1}*{box-sizing:border-box}html,body{height:100%}body{margin:0;padding:20px;background:linear-gradient(180deg,#071020 0%,var(--bg) 100%);color:var(--text);font-family:ui-monospace,Menlo,Monaco,monospace}a{color:inherit}.header{display:flex;gap:12px;align-items:center;margin-bottom:18px;flex-wrap:wrap}.brand{font-size:20px;font-weight:700;color:var(--accent);text-decoration:none}.search-box{margin-left:auto;display:flex;gap:8px;align-items:center;flex:0 1 auto}.search-block{width:100%;display:flex;gap:8px;margin-top:14px}.search-inline{display:flex;gap:8px;align-items:center;min-width:0}input[type="text"]{background:transparent;border:1px solid rgba(255,255,255,0.06);padding:8px 12px;color:var(--text);min-width:120px;border-radius:8px;outline:none}button[type="submit"],.btn-save{background:linear-gradient(90deg,var(--accent),#5b21b6);color:white;border:none;padding:8px 12px;border-radius:8px;cursor:pointer}.btn-save{font-weight:600}.img-container{column-width:calc(260px * var(--img-scale));column-gap:16px;width:100%;max-width:1400px;margin-top:18px}.card{display:inline-block;width:100%;margin:0 0 16px;border-radius:10px;overflow:hidden;background:linear-gradient(180deg,rgba(255,255,255,0.01),rgba(255,255,255,0.02));box-shadow:0 6px 18px rgba(3,7,18,0.6);border:1px solid rgba(124,58,237,0.06);break-inside:avoid;-webkit-column-break-inside:avoid;-moz-column-break-inside:avoid;min-height:0;position:relative}.card img{display:block;width:100%;height:auto;object-fit:cover;background:#08101a}.card-controls{position:absolute;top:8px;right:8px;display:flex;gap:8px;align-items:center}.btn-save-mini{background:rgba(0,0,0,0.45);border:1px solid rgba(255,255,255,0.06);color:var(--text);padding:6px;border-radius:999px;cursor:pointer;font-weight:700;display:inline-flex;align-items:center;justify-content:center;width:34px;height:34px;text-decoration:none}.magnifier{background:rgba(0,0,0,0.45);border:1px solid rgba(255,255,255,0.06);color:var(--text);padding:6px;border-radius:999px;font-size:14px;width:34px;height:34px;display:inline-flex;align-items:center;justify-content:center;text-decoration:none}.bookmarks{margin-left:12px;color:var(--muted);font-size:14px}.bookmark-list{margin-top:10px;display:flex;gap:8px;flex-wrap:wrap}.bookmark-pill{background:rgba(255,255,255,0.03);padding:6px 8px;border-radius:999px;border:1px solid rgba(255,255,255,0.04);font-size:13px;display:flex;gap:6px;align-items:center}.bookmark-pill form{display:inline}.bookmark-remove-btn{background:transparent;border:none;color:#ff7b7b;font-weight:700;cursor:pointer;padding:0 6px}.export-form{margin-top:12px;display:flex;gap:8px;align-items:center}.pagination{text-align:center;margin:26px 0}.pagination a{color:var(--accent);text-decoration:none;padding:8px 12px;border-radius:8px;border:1px solid rgba(124,58,237,0.12);background:rgba(124,58,237,0.02)}.footer-note{color:var(--muted);font-size:12px;margin-top:22px}@media (max-width:640px){body{padding:12px;font-size:18px}.brand{font-size:22px}input[type="text"]{min-width:120px;padding:12px 14px;font-size:16px}button[type="submit"],.btn-save{padding:10px 14px;font-size:16px;border-radius:10px}.img-container{column-width:calc(180px * var(--img-scale));column-gap:12px}.search-block{gap:10px;flex-direction:column}.search-inline{width:100%}.search-box{margin-left:0;width:100%}.bookmarks{order:3;width:100%;margin-top:8px}}`

// ---------- handlers ----------

func styleHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf8")
	_, _ = io.WriteString(w, cssContent)
}

// settings POST handler: sets accent color and image scale cookies
func settingsPostHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	accent := normalizeHexColor(r.FormValue("accent"))
	scaleStr := r.FormValue("scale") // expected as integer percent like "100"
	if accent == "" {
		accent = "#7c3aed"
	}
	percent := 100
	if ss := strings.TrimSpace(scaleStr); ss != "" {
		if p, err := strconv.Atoi(ss); err == nil {
			if p < 50 {
				p = 50
			}
			if p > 200 {
				p = 200
			}
			percent = p
		}
	}
	// set cookies (non-encrypted, not sensitive)
	http.SetCookie(w, &http.Cookie{
		Name:   "pinata_accent",
		Value:  accent,
		Path:   "/",
		MaxAge: 60 * 60 * 24 * 365 * 5,
	})
	http.SetCookie(w, &http.Cookie{
		Name:   "pinata_img_scale",
		Value:  strconv.Itoa(percent),
		Path:   "/",
		MaxAge: 60 * 60 * 24 * 365 * 5,
	})
	next := safeNext(r.FormValue("next"))
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func renderCardHTML(q, next, u string, thumbMobile, thumbDesktop, thumbHigh int) string {
	full := "/image_proxy?url=" + url.QueryEscape(u)
	tm := thumbURL(u, thumbMobile)
	td := thumbURL(u, thumbDesktop)
	th := thumbURL(u, thumbHigh)

	srcset := fmt.Sprintf("%s %dw, %s %dw, %s %dw", tm, thumbMobile, td, thumbDesktop, th, thumbHigh)
	sizes := fmt.Sprintf("(max-width:640px) %dpx, %dpx", thumbMobile, thumbDesktop)

	var b strings.Builder
	b.Grow(len(u)*2 + 768)
	b.WriteString(`<div class="card">`)
	b.WriteString(`<a href="`)
	b.WriteString(html.EscapeString(full))
	b.WriteString(`" style="display:block;" target="_blank" rel="noreferrer"><img loading="lazy" decoding="async" src="`)
	b.WriteString(html.EscapeString(td))
	b.WriteString(`" srcset="`)
	b.WriteString(html.EscapeString(srcset))
	b.WriteString(`" sizes="`)
	b.WriteString(html.EscapeString(sizes))
	b.WriteString(`" alt="image"></a>`)
	b.WriteString(`<div class="card-controls">`)
	if !disableReverse {
		b.WriteString(`<a class="magnifier" href="/revsearch?b64=`)
		b.WriteString(base64.StdEncoding.EncodeToString([]byte(u)))
		b.WriteString(`" title="Search Tineye" target="_blank">🔍</a>`)
	}
	if bookmarkingEnabled {
		b.WriteString(`<form method="post" action="/bookmark_image" style="display:inline;margin:0;">`)
		b.WriteString(`<input type="hidden" name="url" value="`)
		b.WriteString(html.EscapeString(u))
		b.WriteString(`"><input type="hidden" name="next" value="`)
		b.WriteString(html.EscapeString(next))
		b.WriteString(`"><button class="btn-save-mini" type="submit" title="Save image">❤</button></form>`)
	}
	b.WriteString(`</div></div>`)
	return b.String()
}

func writeChunkedCards(w http.ResponseWriter, q, next string, urls []string, thumbMobile, thumbDesktop, thumbHigh int) {
	if len(urls) == 0 {
		return
	}
	if !chunkedMode || len(urls) == 1 {
		for _, u := range urls {
			_, _ = io.WriteString(w, renderCardHTML(q, next, u, thumbMobile, thumbDesktop, thumbHigh))
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}

	type job struct {
		idx int
		u   string
	}
	type result struct {
		idx  int
		html string
	}

	jobs := make(chan job, len(urls))
	results := make(chan result, len(urls))

	workers := chunkWorkers
	if workers > len(urls) {
		workers = len(urls)
	}
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := range jobs {
				results <- result{idx: j.idx, html: renderCardHTML(q, next, j.u, thumbMobile, thumbDesktop, thumbHigh)}
			}
		}()
	}

	for i, u := range urls {
		jobs <- job{idx: i, u: u}
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	out := make([]string, len(urls))
	for r := range results {
		out[r.idx] = r.html
	}
	for _, s := range out {
		_, _ = io.WriteString(w, s)
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}


func useImageBackend() bool {
	return imageBackendBase != ""
}

// Index (front) - server-rendered bookmarks and settings form (no JS)
func indexHandler(w http.ResponseWriter, r *http.Request) {
	accent, imgScale := getThemeVars(r)
	// produce small inline style that overrides css vars
	accentRgba := hexToRGBA(accent, 0.12)
	inlineStyle := fmt.Sprintf(`<style>:root{--accent:%s;--accent-rgba:%s;--img-scale:%s;}</style>`, html.EscapeString(accent), html.EscapeString(accentRgba), html.EscapeString(imgScale))

	w.Header().Set("Content-Type", "text/html; charset=utf8")
	_, _ = io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Pinata - Search</title><link rel="stylesheet" href="/static/style.css">`+inlineStyle+`</head><body>`)
	_, _ = io.WriteString(w, `<div class="header"><a class="brand" href="/">Pinata</a><div class="search-box"></div></div>`)
	_, _ = io.WriteString(w, `<div style="color:var(--muted); margin-bottom:12px;">Pinata is an alternate frontend to Pinterest with support for reverse image search, encrypted bookmarks, and image proxying! None of your data ever reaches Pinterest or their servers while using this frontend, and the instance owner can not ever see what you view or bookmarks.</div>`)
	_, _ = io.WriteString(w, `<form class="search-block" method="get" action="/search"><input type="text" name="q" placeholder="Search Image" required maxlength="64"><button type="submit">Search</button></form>`)

	// Settings form (color + scale)
	_, _ = io.WriteString(w, `<div style="margin-top:12px;"><form method="post" action="/settings" style="display:flex;gap:10px;align-items:center;flex-wrap:wrap;">`)
	_, _ = io.WriteString(w, `<label style="font-size:14px;color:var(--muted);">Accent: <input type="color" name="accent" value="`+html.EscapeString(accent)+`" style="margin-left:6px;"></label>`)
	_, _ = io.WriteString(w, `<label style="font-size:14px;color:var(--muted);">Image scale: <select name="scale" style="margin-left:6px;">`)
	// options: 75,100,125,150
	opts := []int{75, 100, 125, 150}
	for _, v := range opts {
		sel := ""
		if fmt.Sprintf("%.2f", float64(v)/100.0) == imgScale {
			sel = ` selected`
		}
		_, _ = io.WriteString(w, `<option value="`+strconv.Itoa(v)+`"`+sel+`>`+strconv.Itoa(v)+`%</option>`)
	}
	_, _ = io.WriteString(w, `</select></label>`)
	_, _ = io.WriteString(w, `<input type="hidden" name="next" value="/"><button type="submit" class="btn-save">Apply</button></form></div>`)

	// bookmarks shown only on index
	if bookmarkingEnabled {
		items := readBookmarksFromReq(r)
		_, _ = io.WriteString(w, `<div class="bookmarks"><div style="font-size:14px;color:var(--muted);margin-top:8px">Saved bookmarks</div><div class="bookmark-list">`)
		for _, e := range items {
			escaped := html.EscapeString(e.Value)
			if e.Type == "q" {
				_, _ = io.WriteString(w, `<span class="bookmark-pill"><a href="/search?q=`+url.QueryEscape(e.Value)+`">`+escaped+`</a>`)
			} else {
				_, _ = io.WriteString(w, `<span class="bookmark-pill"><a href="/image_proxy?url=`+url.QueryEscape(e.Value)+`">`+escaped+`</a>`)
			}
			_, _ = io.WriteString(w, `<form method="post" action="/bookmark_remove" style="display:inline;margin:0 0 0 6px;"><input type="hidden" name="type" value="`+html.EscapeString(e.Type)+`"><input type="hidden" name="value" value="`+html.EscapeString(e.Value)+`"><button class="bookmark-remove-btn" type="submit" title="Remove">✕</button></form></span>`)
		}
		_, _ = io.WriteString(w, `</div>`)
		_, _ = io.WriteString(w, `<div class="export-form"><form method="get" action="/bookmarks/export"><button type="submit" class="btn-save">Export JSON</button></form>`)
		_, _ = io.WriteString(w, `<form method="post" action="/bookmarks/import" enctype="multipart/form-data" style="margin-left:8px;"><input type="file" name="file" accept="application/json" required><button type="submit" class="btn-save" style="margin-left:8px">Import JSON</button></form></div>`)
		_, _ = io.WriteString(w, `</div>`)
	}

	_, _ = io.WriteString(w, `<div class="footer-note">Powered by Pinata • Reverse image search uses Tineye • <a href="https://codeberg.org/gigirassy/pinata/">Contribute to this code or host your own instance!</a></div></body></html>`)
}

// searchHandler: streaming results, include inline style variables from cookies
func searchHandler(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 1 || len(q) > 64 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	bookmark := r.URL.Query().Get("bookmark")
	csrftoken := r.URL.Query().Get("csrftoken")

	dataObj := map[string]any{"options": map[string]any{"query": q}}
	if bookmark != "" {
		dataObj["options"].(map[string]any)["bookmarks"] = []string{bookmark}
	}
	jb, err := json.Marshal(dataObj)
	if err != nil {
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	dataParam := url.QueryEscape(string(jb))

	var req *http.Request
	if bookmark == "" {
		u := pinterestSearchURL + "?data=" + dataParam
		req, err = http.NewRequestWithContext(r.Context(), "GET", u, nil)
	} else {
		body := "data=" + dataParam
		req, err = http.NewRequestWithContext(r.Context(), "POST", pinterestSearchURL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if err != nil {
		http.Error(w, "failed to build request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("x-pinterest-pws-handler", "www/search/[scope].js")
	if csrftoken != "" {
		req.Header.Set("x-csrftoken", csrftoken)
		req.Header.Set("Cookie", "csrftoken="+csrftoken)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		http.Error(w, "failed to fetch", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	var newCsrf string
	for _, c := range resp.Cookies() {
		if strings.EqualFold(c.Name, "csrftoken") {
			newCsrf = c.Value
			break
		}
	}

	accent, imgScale := getThemeVars(r)
	thumbMobile, thumbDesktop, thumbHigh := thumbWidths(imgScale)
	accentRgba := hexToRGBA(accent, 0.12)
	inlineStyle := fmt.Sprintf(`<style>:root{--accent:%s;--accent-rgba:%s;--img-scale:%s;}</style>`, html.EscapeString(accent), html.EscapeString(accentRgba), html.EscapeString(imgScale))

	// Start streaming HTML
	w.Header().Set("Content-Type", "text/html; charset=utf8")
	_, _ = io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>`+html.EscapeString(q)+` - Pinata</title><link rel="stylesheet" href="/static/style.css">`+inlineStyle+`</head><body>`)
	// header: inline search and Save-search form
	_, _ = io.WriteString(w, `<div class="header" style="margin-bottom:8px;"><a class="brand" href="/">Pinata</a><div class="search-box">`)
	_, _ = io.WriteString(w, `<form class="search-inline" method="get" action="/search"><input type="text" name="q" value="`+html.EscapeString(q)+`" maxlength="64"><button type="submit">Search</button></form>`)
	if bookmarkingEnabled {
		next := "/search?q=" + url.QueryEscape(q)
		_, _ = io.WriteString(w, `<form method="post" action="/bookmark" style="margin-left:8px;"><input type="hidden" name="q" value="`+html.EscapeString(q)+`"><input type="hidden" name="next" value="`+html.EscapeString(next)+`"><button class="btn-save" type="submit">Save</button></form>`)
	}
	_, _ = io.WriteString(w, `</div></div>`)
	_, _ = io.WriteString(w, `<h2 style="margin:4px 0 0 0;">Results for "`+html.EscapeString(q)+`"</h2>`)
	_, _ = io.WriteString(w, `<div class="img-container">`)

	dec := json.NewDecoder(resp.Body)
	var nextBookmark string
	nextSearch := "/search?q=" + url.QueryEscape(q)
	chunk := make([]string, 0, chunkSize)

	for {
		tk, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Printf("json token error: %v", err)
			break
		}
		key, ok := tk.(string)
		if !ok {
			continue
		}
		switch key {
		case "results":
			tk2, err := dec.Token()
			if err != nil {
				log.Printf("unexpected json after results: %v", err)
				continue
			}
			if delim, ok := tk2.(json.Delim); !ok || delim != '[' {
				continue
			}
			var rObj struct {
				Images struct {
					Orig struct {
						URL string `json:"url"`
					} `json:"orig"`
				} `json:"images"`
			}
			for dec.More() {
				if err := dec.Decode(&rObj); err != nil {
					log.Printf("error decoding result item: %v", err)
					break
				}
				u := strings.TrimSpace(rObj.Images.Orig.URL)
				if u == "" {
					continue
				}
				if chunkedMode {
					chunk = append(chunk, u)
					if len(chunk) >= chunkSize {
						writeChunkedCards(w, q, nextSearch, chunk, thumbMobile, thumbDesktop, thumbHigh)
						chunk = chunk[:0]
					}
				} else {
					_, _ = io.WriteString(w, renderCardHTML(q, nextSearch, u, thumbMobile, thumbDesktop, thumbHigh))
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
				}
			}
			_, _ = dec.Token()
		case "bookmark":
			tk2, err := dec.Token()
			if err == nil {
				if s, ok := tk2.(string); ok {
					nextBookmark = s
				}
			}
		default:
			continue
		}
	}

	if chunkedMode && len(chunk) > 0 {
		writeChunkedCards(w, q, nextSearch, chunk, thumbMobile, thumbDesktop, thumbHigh)
	}

	_, _ = io.WriteString(w, `</div>`)
	if nextBookmark != "" {
		qenc := url.QueryEscape(q)
		benc := url.QueryEscape(nextBookmark)
		cenc := ""
		if newCsrf != "" {
			cenc = "&csrftoken=" + url.QueryEscape(newCsrf)
		} else if csrftoken != "" {
			cenc = "&csrftoken=" + url.QueryEscape(csrftoken)
		}
		next := "/search?q=" + qenc + "&bookmark=" + benc + cenc
		_, _ = io.WriteString(w, `<div class="pagination"><a href="`+html.EscapeString(next)+`">Next page</a></div>`)
	}
	_, _ = io.WriteString(w, `<div class="footer-note">Powered by Pinata • Reverse image search uses Tineye • <a href="https://codeberg.org/gigirassy/pinata/">Contribute to this code or host your own instance!</a></div></body></html>`)
}

// ---------- secure image proxy (only https i.pinimg.com) ----------
func imageProxyHandler(w http.ResponseWriter, r *http.Request) {
	uq := r.URL.Query().Get("url")
	if uq == "" {
		http.Error(w, "url required", http.StatusBadRequest)
		return
	}

	orig, err := url.QueryUnescape(uq)
	if err != nil {
		http.Error(w, "invalid url", http.StatusBadRequest)
		return
	}

	parsed, err := url.Parse(orig)
	if err != nil {
		http.Error(w, "invalid url", http.StatusBadRequest)
		return
	}

	if parsed.Scheme != "https" {
		http.Error(w, "proxy allowed for https only", http.StatusForbidden)
		return
	}
	if !strings.EqualFold(parsed.Hostname(), "i.pinimg.com") {
		http.Error(w, "proxy allowed only for i.pinimg.com", http.StatusForbidden)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	var req *http.Request
	if useImageBackend() {
		backendURL := imageBackendBase + "/fetch?url=" + url.QueryEscape(parsed.String())
		req, err = http.NewRequestWithContext(ctx, "GET", backendURL, nil)
	} else {
		req, err = http.NewRequestWithContext(ctx, "GET", parsed.String(), nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:145.0) Gecko/20100101 Firefox/145.0")
	}
	if err != nil {
		http.Error(w, "failed", http.StatusBadGateway)
		return
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		http.Error(w, "failed to fetch", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for _, h := range []string{"Content-Type", "Cache-Control", "ETag", "Last-Modified"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}

	w.WriteHeader(resp.StatusCode)
	bufPtr := copyBufPool.Get().(*[]byte)
	buf := *bufPtr
	_, _ = io.CopyBuffer(w, resp.Body, buf)
	copyBufPool.Put(bufPtr)
}

func thumbWidths(scaleStr string) (int, int, int) {
	scale := 1.0
	if v, err := strconv.ParseFloat(scaleStr, 64); err == nil && v > 0 {
		scale = v
	}
	mobile := int(math.Round(180 * scale))
	desktop := int(math.Round(260 * scale))
	high := int(math.Round(520 * scale))
	if mobile < 120 {
		mobile = 120
	}
	if desktop < mobile {
		desktop = mobile
	}
	if high < desktop {
		high = desktop
	}
	return mobile, desktop, high
}

func thumbURL(u string, w int) string {
	return "/thumb_proxy?url=" + url.QueryEscape(u) + "&w=" + strconv.Itoa(w)
}

func resizeNearest(src image.Image, dstW int) image.Image {
	b := src.Bounds()
	sw := b.Dx()
	sh := b.Dy()
	if dstW <= 0 || sw <= 0 || sh <= 0 || dstW >= sw {
		return src
	}
	dstH := int(math.Round(float64(sh) * float64(dstW) / float64(sw)))
	if dstH < 1 {
		dstH = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	for y := 0; y < dstH; y++ {
		sy := b.Min.Y + int(float64(y)*float64(sh)/float64(dstH))
		if sy >= b.Max.Y {
			sy = b.Max.Y - 1
		}
		for x := 0; x < dstW; x++ {
			sx := b.Min.X + int(float64(x)*float64(sw)/float64(dstW))
			if sx >= b.Max.X {
				sx = b.Max.X - 1
			}
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}

func thumbImageProxyHandler(w http.ResponseWriter, r *http.Request) {
	uq := r.URL.Query().Get("url")
	if uq == "" {
		http.Error(w, "url required", http.StatusBadRequest)
		return
	}

	orig, err := url.QueryUnescape(uq)
	if err != nil {
		http.Error(w, "invalid url", http.StatusBadRequest)
		return
	}

	parsed, err := url.Parse(orig)
	if err != nil {
		http.Error(w, "invalid url", http.StatusBadRequest)
		return
	}

	if parsed.Scheme != "https" {
		http.Error(w, "proxy allowed for https only", http.StatusForbidden)
		return
	}
	if !strings.EqualFold(parsed.Hostname(), "i.pinimg.com") {
		http.Error(w, "proxy allowed only for i.pinimg.com", http.StatusForbidden)
		return
	}

	targetW, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("w")))
	if err != nil || targetW < 1 {
		targetW = 260
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	var req *http.Request
	if useImageBackend() {
		backendURL := fmt.Sprintf(
			"%s/thumb?url=%s&w=%d",
			imageBackendBase,
			url.QueryEscape(parsed.String()),
			targetW,
		)
		req, err = http.NewRequestWithContext(ctx, "GET", backendURL, nil)
	} else {
		req, err = http.NewRequestWithContext(ctx, "GET", parsed.String(), nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:145.0) Gecko/20100101 Firefox/145.0")
	}
	if err != nil {
		http.Error(w, "failed", http.StatusBadGateway)
		return
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		http.Error(w, "failed to fetch", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// If backend is enabled, trust its output.
	if useImageBackend() {
		for _, h := range []string{"Content-Type", "Cache-Control", "ETag", "Last-Modified"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		bufPtr := copyBufPool.Get().(*[]byte)
		buf := *bufPtr
		_, _ = io.CopyBuffer(w, resp.Body, buf)
		copyBufPool.Put(bufPtr)
		return
	}

	// Original direct-fetch thumbnail logic
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "failed to read", http.StatusBadGateway)
		return
	}

	if resp.StatusCode != http.StatusOK {
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "" {
			w.Header().Set("Cache-Control", cc)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
		return
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		} else {
			w.Header().Set("Content-Type", http.DetectContentType(data))
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "" {
			w.Header().Set("Cache-Control", cc)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}

	if targetW >= img.Bounds().Dx() {
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		} else {
			w.Header().Set("Content-Type", http.DetectContentType(data))
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "" {
			w.Header().Set("Cache-Control", cc)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	if cc := resp.Header.Get("Cache-Control"); cc != "" {
		w.Header().Set("Cache-Control", cc)
	}
	w.WriteHeader(http.StatusOK)
	_ = jpeg.Encode(w, resizeNearest(img, targetW), &jpeg.Options{Quality: 82})
}

func revsearchHandler(w http.ResponseWriter, r *http.Request) {
	if disableReverse {
		http.Error(w, "reverse disabled", http.StatusNotFound)
		return
	}
	b64 := r.URL.Query().Get("b64")
	if b64 == "" {
		http.Error(w, "b64 required", http.StatusBadRequest)
		return
	}
	bs, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		http.Error(w, "invalid b64", http.StatusBadRequest)
		return
	}
	orig := string(bs)
	if !(strings.HasPrefix(orig, "http://") || strings.HasPrefix(orig, "https://")) {
		http.Error(w, "invalid url", http.StatusBadRequest)
		return
	}
	tineye := "https://tineye.com/search?url=" + url.QueryEscape(orig)
	http.Redirect(w, r, tineye, http.StatusSeeOther)
}

// ---------- bookmark handlers ----------

func bookmarkPostHandler(w http.ResponseWriter, r *http.Request) {
	if !bookmarkingEnabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	q := strings.TrimSpace(r.FormValue("q"))
	if q == "" || len(q) > 64 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	next := safeNext(r.FormValue("next"))
	entries := readBookmarksFromReq(r)
	new := []BookmarkEntry{{Type: "q", Value: q}}
	for _, e := range entries {
		if e.Type == "q" && e.Value == q {
			continue
		}
		new = append(new, e)
		if len(new) >= maxBookmarks {
			break
		}
	}
	setBookmarksCookie(w, new)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func bookmarkImagePostHandler(w http.ResponseWriter, r *http.Request) {
	if !bookmarkingEnabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	u := strings.TrimSpace(r.FormValue("url"))
	if u == "" || !(strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	next := safeNext(r.FormValue("next"))
	entries := readBookmarksFromReq(r)
	new := []BookmarkEntry{{Type: "img", Value: u}}
	for _, e := range entries {
		if e.Type == "img" && e.Value == u {
			continue
		}
		new = append(new, e)
		if len(new) >= maxBookmarks {
			break
		}
	}
	setBookmarksCookie(w, new)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func bookmarkRemoveHandler(w http.ResponseWriter, r *http.Request) {
	if !bookmarkingEnabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	typ := r.FormValue("type")
	val := r.FormValue("value")
	if typ == "" || val == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	entries := readBookmarksFromReq(r)
	out := make([]BookmarkEntry, 0, len(entries))
	for _, e := range entries {
		if e.Type == typ && e.Value == val {
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		clearBookmarksCookie(w)
	} else {
		setBookmarksCookie(w, out)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func bookmarksExportHandler(w http.ResponseWriter, r *http.Request) {
	if !bookmarkingEnabled {
		http.Error(w, "bookmarks disabled", http.StatusNotFound)
		return
	}
	entries := readBookmarksFromReq(r)
	if entries == nil {
		entries = []BookmarkEntry{}
	}
	js, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		http.Error(w, "failed to export", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf8")
	w.Header().Set("Content-Disposition", "attachment; filename=\"pinata_bookmarks.json\"")
	_, _ = w.Write(js)
}

func bookmarksImportHandler(w http.ResponseWriter, r *http.Request) {
	if !bookmarkingEnabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20) // 2MB
	if err := r.ParseMultipartForm(2 << 20); err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	defer file.Close()
	dec := json.NewDecoder(file)
	var entries []BookmarkEntry
	if err := dec.Decode(&entries); err == nil {
		// ok
	} else {
		if _, err := file.Seek(0, io.SeekStart); err == nil {
			var arr []string
			dec2 := json.NewDecoder(file)
			if err2 := dec2.Decode(&arr); err2 == nil {
				entries = make([]BookmarkEntry, 0, len(arr))
				for _, s := range arr {
					entries = append(entries, BookmarkEntry{Type: "q", Value: s})
				}
			} else {
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
		} else {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	existing := readBookmarksFromReq(r)
	merged := make([]BookmarkEntry, 0, maxBookmarks)
	seen := map[string]bool{}
	add := func(e BookmarkEntry) {
		key := e.Type + "|" + e.Value
		if seen[key] {
			return
		}
		seen[key] = true
		merged = append(merged, e)
	}
	for _, e := range entries {
		e.Value = strings.TrimSpace(e.Value)
		if e.Value == "" {
			continue
		}
		if len(e.Value) > maxItemLen {
			e.Value = e.Value[:maxItemLen]
		}
		if e.Type != "q" && e.Type != "img" {
			e.Type = "q"
		}
		add(e)
		if len(merged) >= maxBookmarks {
			break
		}
	}
	for _, e := range existing {
		add(e)
		if len(merged) >= maxBookmarks {
			break
		}
	}
	setBookmarksCookie(w, merged)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---------- main ----------
func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /static/style.css", styleHandler)
	mux.HandleFunc("POST /settings", settingsPostHandler)
	mux.HandleFunc("GET /{$}", indexHandler)
	mux.HandleFunc("GET /search", searchHandler)
	mux.HandleFunc("GET /image_proxy", imageProxyHandler)
	mux.HandleFunc("GET /revsearch", revsearchHandler)
	mux.HandleFunc("GET /thumb_proxy", thumbImageProxyHandler)

	// bookmark endpoints: anything that changes state is POST-only
	mux.HandleFunc("POST /bookmark", bookmarkPostHandler)
	mux.HandleFunc("POST /bookmark_image", bookmarkImagePostHandler)
	mux.HandleFunc("POST /bookmark_remove", bookmarkRemoveHandler)
	mux.HandleFunc("GET /bookmarks/export", bookmarksExportHandler)
	mux.HandleFunc("POST /bookmarks/import", bookmarksImportHandler)

	// Reject cross-origin browser requests with unsafe methods (CSRF),
	// based on Sec-Fetch-Site with an Origin/Host fallback. Stateless: no tokens.
	csrf := http.NewCrossOriginProtection()

	server := &http.Server{
		Addr:         ":8080",
		Handler:      csrf.Handler(mux),
		ReadTimeout:  12 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
		BaseContext: func(net.Listener) context.Context { return context.Background() },
	}

	log.Println("Pinata listening on :8080 (no-JS mode). Bookmarking enabled:", bookmarkingEnabled, " Reverse disabled:", disableReverse)
	log.Fatal(server.ListenAndServe())
}