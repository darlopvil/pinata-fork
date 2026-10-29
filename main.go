package main

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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
const pinterestPinURL = "https://www.pinterest.com/resource/PinResource/get/"
const cookieName = "pinata_bm"

// ---------- bookmarks types / config ----------
type BookmarkEntry struct {
	Type  string `json:"type"`            // "q", "img" or "pin"
	Value string `json:"value"`           // query, image URL or pin page link
	Image string `json:"image,omitempty"` // pin image, for numeric pin links
}

var bookmarkKey []byte
var bookmarkingEnabled bool
var disableReverse bool
var imageBackendBase string
var pinLinkKey []byte

const maxBookmarks = 30
const maxItemLen = 256
const maxQueryRunes = 64

// maxPinLink caps the pin page links a bookmark can hold. Measured signed
// links are 435 bytes, 524 with a 500-character description; a pin whose link
// is longer is bookmarked as an image instead.
const maxPinLink = 1024

// maxCookieValue keeps the bookmarks cookie (name=value) within the 4096
// bytes browsers accept per cookie; a larger Set-Cookie is silently dropped.
const maxCookieValue = 4096 - len(cookieName) - 1

// Image bookmarks are almost always pinimg URLs. Inside the cookie their
// prefix is stored as "~": repeated in every entry, it alone made 30 image
// bookmarks overflow the cookie size limit.
const pinimgPrefix = "https://i.pinimg.com/"

func compactBookmarks(entries []BookmarkEntry) []BookmarkEntry {
	out := make([]BookmarkEntry, len(entries))
	for i, e := range entries {
		if e.Type == "img" && strings.HasPrefix(e.Value, pinimgPrefix) {
			e.Value = "~" + strings.TrimPrefix(e.Value, pinimgPrefix)
		}
		if strings.HasPrefix(e.Image, pinimgPrefix) {
			e.Image = "~" + strings.TrimPrefix(e.Image, pinimgPrefix)
		}
		out[i] = e
	}
	return out
}

func expandBookmarks(entries []BookmarkEntry) {
	for i, e := range entries {
		if e.Type == "img" && strings.HasPrefix(e.Value, "~") {
			entries[i].Value = pinimgPrefix + strings.TrimPrefix(e.Value, "~")
		}
		if strings.HasPrefix(e.Image, "~") {
			entries[i].Image = pinimgPrefix + strings.TrimPrefix(e.Image, "~")
		}
	}
}

// normalizeEntry cleans a bookmark before it is stored, and reports whether
// it is worth keeping.
func normalizeEntry(e BookmarkEntry) (BookmarkEntry, bool) {
	e.Value = strings.TrimSpace(e.Value)
	switch e.Type {
	case "pin":
		// Only genuine pin links are stored: an invalid one would take cookie
		// space without ever showing up on the homepage to be removed.
		if e.Value == "" || len(e.Value) > maxPinLink {
			return e, false
		}
		if _, _, ok := pinLinkInfo(e.Value, e.Image); !ok {
			return e, false
		}
		if !isPinID(strings.TrimPrefix(e.Value, "/pin/")) {
			e.Image = "" // signed links carry their image
		}
	case "img":
		e.Value, e.Image = truncateUTF8(e.Value, maxItemLen), ""
	default:
		e.Type, e.Value, e.Image = "q", truncateUTF8(e.Value, maxItemLen), ""
	}
	return e, e.Value != ""
}

// pinLinkInfo checks a bookmarked pin link and returns the image to show for
// it and its title, if known. Signed links carry both and are verified;
// numeric links rely on the stored image.
func pinLinkInfo(link, image string) (string, string, bool) {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "" || u.Host != "" || !strings.HasPrefix(u.Path, "/pin/") {
		return "", "", false
	}
	id := strings.TrimPrefix(u.Path, "/pin/")
	if isPinID(id) {
		return image, "", isPinimgURL(image)
	}
	if !isOpaquePinID(id) {
		return "", "", false
	}
	c, ok := openPinCard(id, u.Query().Get("d"), u.Query().Get("s"))
	if !ok {
		return "", "", false
	}
	return c.O, firstNonEmpty(c.T, truncateRunes(c.D, 150)), true
}

// variantURL maps a pinimg URL (an original or any size) to the JPEG of
// another size: Pinterest serves every size under the same path and hash,
// always as JPEG (checked for JPG, PNG, GIF and HEIC originals).
func variantURL(u, size string) string {
	if !isPinimgURL(u) {
		return ""
	}
	rest := strings.TrimPrefix(u, pinimgPrefix)
	i := strings.Index(rest, "/")
	if i < 0 {
		return ""
	}
	p := rest[i+1:]
	if dot := strings.LastIndex(p, "."); dot > strings.LastIndex(p, "/") {
		p = p[:dot]
	}
	return pinimgPrefix + size + "/" + p + ".jpg"
}

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// bookmarksFull reports whether one more bookmark the size of a typical pin
// would push the oldest ones out.
func bookmarksFull(entries []BookmarkEntry) bool {
	// Most bookmarks are pins: probe with the longest measured pin link.
	probe := append([]BookmarkEntry{{Type: "pin", Value: strings.Repeat("x", 524)}}, entries...)
	if len(probe) > maxBookmarks {
		return true
	}
	enc, err := encryptBookmarks(probe)
	return err == nil && len(enc) > maxCookieValue
}

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

	imageBackendBase = strings.TrimRight(strings.TrimSpace(os.Getenv("PINATA_IMAGE_BACKEND")), "/")

	// Pin links from search results are signed (see signPinCard). With a
	// bookmark key, derive a separate key from it so links survive restarts;
	// without one, use a random key and links last until the next restart.
	if bookmarkingEnabled {
		m := hmac.New(sha256.New, bookmarkKey)
		m.Write([]byte("pinata pin links v1"))
		pinLinkKey = m.Sum(nil)
	} else {
		pinLinkKey = make([]byte, 32)
		if _, err := rand.Read(pinLinkKey); err != nil {
			log.Fatalf("can't generate the pin link key: %v", err)
		}
		log.Println("Pin links use a random key and stop working after a restart; set PINATA_BOOKMARK_KEY to keep them valid")
	}
}

// ---------- encryption helpers (AES-GCM) ----------
func encryptBookmarks(entries []BookmarkEntry) (string, error) {
	if !bookmarkingEnabled {
		return "", nil
	}
	plain, err := json.Marshal(compactBookmarks(entries))
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
		expandBookmarks(entries)
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

// isHTTPS reports whether the client reached Pinata over HTTPS, directly or
// through a reverse proxy that sets X-Forwarded-Proto. Cookies get the Secure
// flag then, so plain-HTTP local testing keeps working.
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func setBookmarksCookie(w http.ResponseWriter, r *http.Request, entries []BookmarkEntry) {
	if !bookmarkingEnabled {
		return
	}
	seen := map[string]bool{}
	out := make([]BookmarkEntry, 0, len(entries))
	for _, e := range entries {
		e, ok := normalizeEntry(e)
		if !ok {
			continue
		}
		key := e.Type + "|" + e.Value
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
		if len(out) >= maxBookmarks {
			break
		}
	}
	// Drop the oldest entries until the cookie fits: browsers reject a larger
	// one, which used to lose the newest bookmark without a word.
	enc, err := encryptBookmarks(out)
	for err == nil && len(enc) > maxCookieValue && len(out) > 0 {
		out = out[:len(out)-1]
		enc, err = encryptBookmarks(out)
	}
	if err != nil {
		return
	}
	c := &http.Cookie{
		Name:     cookieName,
		Value:    enc,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
		MaxAge:   60 * 60 * 24 * 365 * 10,
	}
	http.SetCookie(w, c)
}

func clearBookmarksCookie(w http.ResponseWriter, r *http.Request) {
	c := &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
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

// placeholderStyle paints an image's box in its dominant color (sent by
// Pinterest) while it loads, with the alt text in whichever of a dark or a
// light color contrasts more with it (WCAG relative luminance: above 0.179,
// black text has the higher contrast). A slow or failed image then still
// shows its color and a readable title.
func placeholderStyle(color string) string {
	c := normalizeHexColor(color)
	if c == "" {
		return ""
	}
	lin := func(hex string) float64 {
		v, _ := strconv.ParseUint(hex, 16, 8)
		x := float64(v) / 255
		if x <= 0.04045 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	l := 0.2126*lin(c[1:3]) + 0.7152*lin(c[3:5]) + 0.0722*lin(c[5:7])
	text := "#f1f5f9"
	if l > 0.179 {
		text = "#0b0f17"
	}
	return ` style="background-color:` + c + `;color:` + text + `"`
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
const cssContent = `:root{--bg:#0b0f17;--muted:#94a3b8;--text:#e6e6ff;--accent:#7c3aed;--accent-rgba:rgba(124,58,237,0.12);--img-scale:1}*{box-sizing:border-box}html,body{height:100%}body{margin:0;padding:20px;background:linear-gradient(180deg,#071020 0%,var(--bg) 100%);color:var(--text);font-family:ui-monospace,Menlo,Monaco,monospace}a{color:inherit}.header{display:flex;gap:12px;align-items:center;margin-bottom:18px;flex-wrap:wrap}.brand{font-size:20px;font-weight:700;color:var(--accent);text-decoration:none}.search-box{margin-left:auto;display:flex;gap:8px;align-items:center;flex:0 1 auto}.search-block{width:100%;display:flex;gap:8px;margin-top:14px}.search-inline{display:flex;gap:8px;align-items:center;min-width:0}input[type="text"]{background:transparent;border:1px solid rgba(255,255,255,0.06);padding:8px 12px;color:var(--text);min-width:120px;border-radius:8px;outline:none}button[type="submit"],.btn-save{background:linear-gradient(90deg,var(--accent),#5b21b6);color:white;border:none;padding:8px 12px;border-radius:8px;cursor:pointer}.btn-save{font-weight:600}.img-container{column-width:calc(260px * var(--img-scale));column-gap:16px;width:100%;max-width:1400px;margin-top:18px}.card{display:inline-block;width:100%;margin:0 0 16px;border-radius:10px;overflow:hidden;background:linear-gradient(180deg,rgba(255,255,255,0.01),rgba(255,255,255,0.02));box-shadow:0 6px 18px rgba(3,7,18,0.6);border:1px solid rgba(124,58,237,0.06);break-inside:avoid;-webkit-column-break-inside:avoid;-moz-column-break-inside:avoid;min-height:0;position:relative}.card a{text-decoration:none}.card img{display:block;width:100%;height:auto;object-fit:cover;background:#08101a;font-size:12px;line-height:1.4;overflow:hidden}.card-controls{position:absolute;top:8px;right:8px;display:flex;gap:8px;align-items:center}.btn-save-mini{background:rgba(0,0,0,0.45);border:1px solid rgba(255,255,255,0.06);color:var(--text);padding:6px;border-radius:999px;cursor:pointer;font-weight:700;display:inline-flex;align-items:center;justify-content:center;width:34px;height:34px;text-decoration:none}.magnifier{background:rgba(0,0,0,0.45);border:1px solid rgba(255,255,255,0.06);color:var(--text);padding:6px;border-radius:999px;font-size:14px;width:34px;height:34px;display:inline-flex;align-items:center;justify-content:center;text-decoration:none}.bookmarks{margin-left:12px;color:var(--muted);font-size:14px}.bookmark-list{margin-top:10px;display:flex;gap:8px;flex-wrap:wrap}.bookmark-pill{background:rgba(255,255,255,0.03);padding:6px 8px;border-radius:999px;border:1px solid rgba(255,255,255,0.04);font-size:13px;display:flex;gap:6px;align-items:center}.bookmark-pill form{display:inline}.bookmark-remove-btn{background:transparent;border:none;color:#ff7b7b;font-weight:700;cursor:pointer;padding:0 6px}.bm-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(130px,1fr));gap:10px;margin-top:12px;max-width:1100px}.bm-tile{position:relative;aspect-ratio:3/4;border-radius:10px;overflow:hidden;background:#08101a;border:1px solid rgba(255,255,255,0.04)}.bm-tile img{display:block;width:100%;height:100%;object-fit:cover;font-size:12px;color:var(--muted)}.bm-tile form{position:absolute;top:6px;right:6px;margin:0}.bm-tile .bookmark-remove-btn{width:28px;height:28px;padding:0;border-radius:999px;background:rgba(0,0,0,0.55)}.export-form{margin-top:12px;display:flex;gap:8px;align-items:center}.pagination{text-align:center;margin:26px 0}.pagination a{color:var(--accent);text-decoration:none;padding:8px 12px;border-radius:8px;border:1px solid rgba(124,58,237,0.12);background:rgba(124,58,237,0.02)}.footer-note{color:var(--muted);font-size:12px;margin-top:22px}.pin{display:flex;gap:24px;align-items:flex-start;flex-wrap:wrap;margin-top:18px}.pin-media{flex:0 1 auto;max-width:100%}.pin-media img{display:block;max-width:100%;width:auto;height:auto;border-radius:10px;background:#08101a;font-size:14px;overflow:hidden}@media (min-width:641px){.pin-media img{max-height:85vh}}.pin-info{flex:1 1 280px;max-width:560px}.pin-info h2{margin:0 0 10px}.pin-desc{white-space:pre-line;margin:0 0 12px}.pin-meta{color:var(--muted);font-size:14px;line-height:1.7;margin:0 0 12px}.pin-actions{display:flex;gap:8px;align-items:center;flex-wrap:wrap}.save-frame{display:block;width:34px;height:34px;border:0;background:transparent;overflow:hidden}.save-frame-q{width:96px;height:36px;margin-left:8px}@media (max-width:640px){body{padding:12px;font-size:18px}.brand{font-size:22px}input[type="text"]{min-width:120px;padding:12px 14px;font-size:16px}button[type="submit"],.btn-save{padding:10px 14px;font-size:16px;border-radius:10px}.img-container{column-width:calc(180px * var(--img-scale));column-gap:12px}.search-block{gap:10px;flex-direction:column}.search-inline{width:100%}.search-box{margin-left:0;width:100%}.bookmarks{order:3;width:100%;margin-top:8px}}`

// ---------- handlers ----------

// pageHead returns the document head with the stylesheet inlined, plus the
// per-user theme variables. Styles arrive with the first bytes of the page,
// so nothing can be laid out unstyled while a separate stylesheet loads.
func pageHead(r *http.Request, title string) string {
	accent, imgScale := getThemeVars(r)
	vars := fmt.Sprintf(`:root{--accent:%s;--accent-rgba:%s;--img-scale:%s;}`, html.EscapeString(accent), html.EscapeString(hexToRGBA(accent, 0.12)), html.EscapeString(imgScale))
	return `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>` + html.EscapeString(title) + `</title><style>` + cssContent + vars + `</style></head><body>`
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
		Name:     "pinata_accent",
		Value:    accent,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
		MaxAge:   60 * 60 * 24 * 365 * 5,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "pinata_img_scale",
		Value:    strconv.Itoa(percent),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
		MaxAge:   60 * 60 * 24 * 365 * 5,
	})
	next := safeNext(r.FormValue("next"))
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func renderCardHTML(r *searchResult, href string, thumbMobile, thumbDesktop, thumbHigh int) string {
	full := fullImage(r.Images.Orig, r.Images.X736)
	u := full.URL
	if href == "" {
		href = "/image_proxy?url=" + url.QueryEscape(u)
	}
	// Thumbnails are resized from Pinterest's JPEG variants rather than the
	// original: GIF and HEIC originals weigh megabytes, and the image proxy
	// passes GIFs through unresized and can't decode HEIC.
	thumbSrc := func(w int) string {
		if w <= 474 && isPinimgURL(r.Images.X474.URL) {
			return r.Images.X474.URL
		}
		if isPinimgURL(r.Images.X736.URL) {
			return r.Images.X736.URL
		}
		return u
	}
	tm := thumbURL(thumbSrc(thumbMobile), thumbMobile)
	td := thumbURL(thumbSrc(thumbDesktop), thumbDesktop)
	th := thumbURL(thumbSrc(thumbHigh), thumbHigh)
	// Width and height reserve the card's space before the thumbnail loads.
	dims := r.Images.X474
	if dims.Width <= 0 || dims.Height <= 0 {
		dims = full
	}

	srcset := fmt.Sprintf("%s %dw, %s %dw, %s %dw", tm, thumbMobile, td, thumbDesktop, th, thumbHigh)
	sizes := fmt.Sprintf("(max-width:640px) %dpx, %dpx", thumbMobile, thumbDesktop)

	var b strings.Builder
	b.Grow(len(u)*2 + 768)
	b.WriteString(`<div class="card">`)
	b.WriteString(`<a href="`)
	b.WriteString(html.EscapeString(href))
	b.WriteString(`" style="display:block;" target="_blank" rel="noreferrer"><img loading="lazy" decoding="async" src="`)
	if strings.HasSuffix(strings.ToLower(u), ".gif") {
		// GIF cards show the animated original instead of a resized JPEG.
		b.WriteString(html.EscapeString("/image_proxy?url=" + url.QueryEscape(u)))
	} else {
		b.WriteString(html.EscapeString(td))
		b.WriteString(`" srcset="`)
		b.WriteString(html.EscapeString(srcset))
		b.WriteString(`" sizes="`)
		b.WriteString(html.EscapeString(sizes))
	}
	b.WriteString(`"`)
	if dims.Width > 0 && dims.Height > 0 {
		b.WriteString(fmt.Sprintf(` width="%d" height="%d"`, dims.Width, dims.Height))
	}
	alt := firstNonEmpty(r.Title, r.GridTitle, r.AutoAltText, r.SeoAltText, truncateRunes(r.Description, 150), "Pinterest image")
	b.WriteString(placeholderStyle(r.DominantColor))
	b.WriteString(` alt="` + html.EscapeString(alt) + `"></a>`)
	b.WriteString(`<div class="card-controls">`)
	if !disableReverse {
		b.WriteString(`<a class="magnifier" href="/revsearch?b64=`)
		b.WriteString(base64.StdEncoding.EncodeToString([]byte(u)))
		b.WriteString(`" title="Search Tineye" target="_blank">🔍</a>`)
	}
	if bookmarkingEnabled {
		b.WriteString(pinSaveFrame(href, u))
	}
	b.WriteString(`</div></div>`)
	return b.String()
}

func useImageBackend() bool {
	return imageBackendBase != ""
}

// ---------- error pages ----------

// renderErrorPage writes a styled HTML error page with the given status code.
func renderErrorPage(w http.ResponseWriter, r *http.Request, code int, msg string) {
	title := fmt.Sprintf("%d %s", code, http.StatusText(code))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, pageHead(r, title+" - Pinata"))
	_, _ = io.WriteString(w, `<div class="header"><a class="brand" href="/">Pinata</a></div>`)
	_, _ = io.WriteString(w, `<h2 style="margin:4px 0 8px 0;">`+html.EscapeString(title)+`</h2><p style="color:var(--muted);">`+html.EscapeString(msg)+`</p><p><a href="/">Back to search</a></p></body></html>`)
}

// renderFetchError logs a failed Pinterest request and renders a 502. Only the
// inner error is logged: url.Error carries the request URL, which holds the
// user's query.
func renderFetchError(w http.ResponseWriter, r *http.Request, what string, err error) {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	log.Printf("pinterest %s: request failed: %v", what, err)
	renderErrorPage(w, r, http.StatusBadGateway, "Couldn't reach Pinterest. Try again in a moment.")
}

// renderUpstreamError renders the error page for a non-2xx Pinterest response.
func renderUpstreamError(w http.ResponseWriter, r *http.Request, what string, resp *http.Response) {
	log.Printf("pinterest %s: upstream status %d", what, resp.StatusCode)
	if resp.StatusCode == http.StatusTooManyRequests {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		renderErrorPage(w, r, http.StatusServiceUnavailable, "Pinterest is rate-limiting this instance (HTTP 429). Try again later.")
		return
	}
	renderErrorPage(w, r, http.StatusBadGateway, fmt.Sprintf("Pinterest returned an error (HTTP %d). Try again later.", resp.StatusCode))
}

// errorPageWriter replaces the mux's plain-text 404/405 bodies with the
// styled error page. Any other status passes through untouched.
type errorPageWriter struct {
	http.ResponseWriter
	r        *http.Request
	replaced bool
}

func (e *errorPageWriter) WriteHeader(code int) {
	switch code {
	case http.StatusNotFound:
		e.replaced = true
		renderErrorPage(e.ResponseWriter, e.r, code, "This page doesn't exist.")
	case http.StatusMethodNotAllowed:
		e.replaced = true
		renderErrorPage(e.ResponseWriter, e.r, code, "This address doesn't accept that kind of request.")
	default:
		e.ResponseWriter.WriteHeader(code)
	}
}

func (e *errorPageWriter) Write(b []byte) (int, error) {
	if e.replaced {
		return len(b), nil
	}
	return e.ResponseWriter.Write(b)
}

// securityHeaders locks every response down. Pinata ships no JavaScript,
// embeds nothing from other sites and serves all its images itself, so the
// policy can be strict: inline styles are the only exception. Frames are
// allowed from the instance itself only, for the save button frames.
func securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; form-action 'self'; frame-src 'self'; frame-ancestors 'self'; base-uri 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// withErrorPages serves mux, using the styled error page for requests that
// match no route or use the wrong method.
func withErrorPages(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern == "" {
			mux.ServeHTTP(&errorPageWriter{ResponseWriter: w, r: r}, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// Index (front) - server-rendered bookmarks and settings form (no JS)
func indexHandler(w http.ResponseWriter, r *http.Request) {
	accent, imgScale := getThemeVars(r)

	w.Header().Set("Content-Type", "text/html; charset=utf8")
	_, _ = io.WriteString(w, pageHead(r, "Pinata - Search"))
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
		_, _ = io.WriteString(w, `<div class="bookmarks"><div style="font-size:14px;color:var(--muted);margin-top:8px">Saved bookmarks (`+strconv.Itoa(len(items))+`)</div>`)
		if bookmarksFull(items) {
			_, _ = io.WriteString(w, `<div style="font-size:13px;color:var(--muted);margin-top:4px">Bookmark storage is nearly full: new saves may remove the oldest ones.</div>`)
		}
		var pills, tiles strings.Builder
		for _, e := range items {
			remove := `<form method="post" action="/bookmark_remove"><input type="hidden" name="type" value="` + html.EscapeString(e.Type) + `"><input type="hidden" name="value" value="` + html.EscapeString(e.Value) + `"><button class="bookmark-remove-btn" type="submit" title="Remove">✕</button></form>`
			var link, img, alt string
			switch e.Type {
			case "q":
				pills.WriteString(`<span class="bookmark-pill"><a href="/search?q=` + url.QueryEscape(e.Value) + `">` + html.EscapeString(e.Value) + `</a>` + remove + `</span>`)
				continue
			case "pin":
				var ok bool
				if img, alt, ok = pinLinkInfo(e.Value, e.Image); !ok {
					continue
				}
				link = e.Value
			case "img":
				if !isPinimgURL(e.Value) {
					continue
				}
				img, link = e.Value, "/image_proxy?url="+url.QueryEscape(e.Value)
			}
			thumb := img
			if v := variantURL(img, "474x"); v != "" {
				thumb = v
			}
			tiles.WriteString(`<div class="bm-tile"><a href="` + html.EscapeString(link) + `" target="_blank" rel="noreferrer"><img loading="lazy" decoding="async" src="` + html.EscapeString(thumbURL(thumb, 260)) + `" alt="` + html.EscapeString(firstNonEmpty(alt, "Bookmarked pin")) + `"></a>` + remove + `</div>`)
		}
		if pills.Len() > 0 {
			_, _ = io.WriteString(w, `<div class="bookmark-list">`+pills.String()+`</div>`)
		}
		if tiles.Len() > 0 {
			_, _ = io.WriteString(w, `<div class="bm-grid">`+tiles.String()+`</div>`)
		}
		_, _ = io.WriteString(w, `<div class="export-form"><form method="get" action="/bookmarks/export"><button type="submit" class="btn-save">Export JSON</button></form>`)
		_, _ = io.WriteString(w, `<form method="post" action="/bookmarks/import" enctype="multipart/form-data" style="margin-left:8px;"><input type="file" name="file" accept="application/json" required><button type="submit" class="btn-save" style="margin-left:8px">Import JSON</button></form></div>`)
		_, _ = io.WriteString(w, `</div>`)
	}

	_, _ = io.WriteString(w, `<div class="footer-note">Powered by Pinata • Reverse image search uses Tineye • <a href="https://codeberg.org/gigirassy/pinata/">Contribute to this code or host your own instance!</a></div></body></html>`)
}

// searchHandler: streaming results, include inline style variables from cookies
func searchHandler(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if utf8.RuneCountInString(q) > maxQueryRunes {
		renderErrorPage(w, r, http.StatusBadRequest, fmt.Sprintf("Searches can be up to %d characters long.", maxQueryRunes))
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
		renderFetchError(w, r, "search", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		renderUpstreamError(w, r, "search", resp)
		return
	}

	var newCsrf string
	for _, c := range resp.Cookies() {
		if strings.EqualFold(c.Name, "csrftoken") {
			newCsrf = c.Value
			break
		}
	}

	_, imgScale := getThemeVars(r)
	thumbMobile, thumbDesktop, thumbHigh := thumbWidths(imgScale)

	// Start streaming HTML
	w.Header().Set("Content-Type", "text/html; charset=utf8")
	_, _ = io.WriteString(w, pageHead(r, q+" - Pinata"))
	// header: inline search and Save-search form
	_, _ = io.WriteString(w, `<div class="header" style="margin-bottom:8px;"><a class="brand" href="/">Pinata</a><div class="search-box">`)
	_, _ = io.WriteString(w, `<form class="search-inline" method="get" action="/search"><input type="text" name="q" value="`+html.EscapeString(q)+`" maxlength="64"><button type="submit">Search</button></form>`)
	if bookmarkingEnabled {
		_, _ = io.WriteString(w, saveFrameTag("q", q, ""))
	}
	_, _ = io.WriteString(w, `</div></div>`)
	_, _ = io.WriteString(w, `<h2 style="margin:4px 0 0 0;">Results for "`+html.EscapeString(q)+`"</h2>`)
	_, _ = io.WriteString(w, `<div class="img-container">`)

	dec := json.NewDecoder(resp.Body)
	var nextBookmark string
	shown := 0

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
			for dec.More() {
				// Fresh value per item: Decode leaves fields untouched when a
				// key is missing or null, so a reused struct would carry the
				// previous result's image over.
				var rObj searchResult
				if err := dec.Decode(&rObj); err != nil {
					log.Printf("error decoding result item: %v", err)
					break
				}
				u := strings.TrimSpace(rObj.Images.Orig.URL)
				if u == "" {
					continue
				}
				shown++
				_, _ = io.WriteString(w, renderCardHTML(&rObj, cardHref(&rObj), thumbMobile, thumbDesktop, thumbHigh))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
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

	_, _ = io.WriteString(w, `</div>`)
	if shown == 0 {
		_, _ = io.WriteString(w, `<p style="color:var(--muted);">No results for "`+html.EscapeString(q)+`".</p>`)
	}
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

// ---------- pin page ----------

// pinImage is one size of a pin's image as Pinterest describes it.
type pinImage struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// fullImage returns the image to show at full size: the original, or the
// 736x JPEG variant when the original is HEIC/HEIF, which most browsers
// can't display.
func fullImage(orig, x736 pinImage) pinImage {
	lower := strings.ToLower(orig.URL)
	if (strings.HasSuffix(lower, ".heic") || strings.HasSuffix(lower, ".heif")) && isPinimgURL(x736.URL) {
		return x736
	}
	return orig
}

// pinData holds the PinResource fields the pin page uses.
type pinData struct {
	ID                 string `json:"id"`
	Title              string `json:"title"`
	GridTitle          string `json:"grid_title"`
	Description        string `json:"description"`
	CloseupDescription string `json:"closeup_unified_description"`
	AltText            string `json:"alt_text"`
	AutoAltText        string `json:"auto_alt_text"`
	Link               string `json:"link"`
	Domain             string `json:"domain"`
	CreatedAt          string `json:"created_at"`
	IsVideo            bool   `json:"is_video"`

	Images map[string]pinImage `json:"images"`

	Pinner *struct {
		Username string `json:"username"`
		FullName string `json:"full_name"`
	} `json:"pinner"`
	Board *struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"board"`
	Aggregated *struct {
		Stats struct {
			Saves int `json:"saves"`
		} `json:"aggregated_stats"`
	} `json:"aggregated_pin_data"`
	DominantColor string `json:"dominant_color"`
}

// isPinID reports whether s is a numeric pin ID. Some search results carry
// opaque IDs (e.g. "e1_...") that PinResource answers with 404, so those
// cards keep linking to the image instead.
func isPinID(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// searchResult holds the fields of a search result that cards and pin pages use.
type searchResult struct {
	ID          string          `json:"id"`
	Title       string          `json:"title"`
	GridTitle   string          `json:"grid_title"`
	Description string          `json:"description"`
	AutoAltText string          `json:"auto_alt_text"`
	SeoAltText  string          `json:"seo_alt_text"`
	Domain      string          `json:"domain"`
	CreatedAt   string          `json:"created_at"`
	Videos      json.RawMessage `json:"videos"`
	Images      struct {
		Orig pinImage `json:"orig"`
		X474 pinImage `json:"474x"`
		X736 pinImage `json:"736x"`
	} `json:"images"`
	Pinner *struct {
		Username string `json:"username"`
		FullName string `json:"full_name"`
	} `json:"pinner"`
	Board *struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"board"`
	Aggregated *struct {
		Stats struct {
			Saves int `json:"saves"`
		} `json:"aggregated_stats"`
	} `json:"aggregated_pin_data"`
	DominantColor string `json:"dominant_color"`
}

// pinCard is the compact copy of a search result carried in a signed pin link.
type pinCard struct {
	T string `json:"t,omitempty"` // title
	D string `json:"d,omitempty"` // description
	O string `json:"o"`           // original image URL
	W int    `json:"w,omitempty"`
	H int    `json:"h,omitempty"`
	A string `json:"a,omitempty"` // alt text
	M string `json:"m,omitempty"` // source domain
	U string `json:"u,omitempty"` // pinner username
	N string `json:"n,omitempty"` // pinner full name
	B string `json:"b,omitempty"` // board name
	C string `json:"c,omitempty"` // created_at
	S int    `json:"s,omitempty"` // saves
	V bool   `json:"v,omitempty"` // video pin
	K string `json:"k,omitempty"` // dominant color
}

// pinView is everything the pin page renders, whatever the data source.
type pinView struct {
	Title      string
	Desc       string
	Orig       string
	Width      int
	Height     int
	Alt        string
	SourceURL  string // full source link (PinResource only)
	Domain     string
	PinnerUser string
	PinnerName string
	Board      string
	CreatedAt  string
	Saves      int
	IsVideo    bool
	Color      string // dominant color, for the placeholder
	Link       string // this pin page's link, for bookmarks
}

// isOpaquePinID reports whether s looks like the opaque IDs ("e1_...") that
// Pinterest gives logged-out clients in search results. PinResource can't
// resolve them, so their pin pages are built from signed links instead.
func isOpaquePinID(s string) bool {
	if len(s) < 4 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func truncateRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func boardName(name, boardURL string) string {
	if name != "" {
		return name
	}
	parts := strings.Split(strings.Trim(boardURL, "/"), "/")
	return parts[len(parts)-1]
}

func isPinimgURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), "i.pinimg.com")
}

func pinCardMAC(id, d string) string {
	m := hmac.New(sha256.New, pinLinkKey)
	m.Write([]byte(id))
	m.Write([]byte{0})
	m.Write([]byte(d))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

// signPinCard returns the "d" (deflated JSON, base64url) and "s" (truncated
// HMAC-SHA256 over the pin ID and d) parameters of a pin link, so pin pages
// for opaque IDs need no request to Pinterest and can't be forged.
func signPinCard(id string, c pinCard) (string, string, error) {
	js, err := json.Marshal(c)
	if err != nil {
		return "", "", err
	}
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		return "", "", err
	}
	if _, err := fw.Write(js); err != nil {
		return "", "", err
	}
	if err := fw.Close(); err != nil {
		return "", "", err
	}
	d := base64.RawURLEncoding.EncodeToString(buf.Bytes())
	return d, pinCardMAC(id, d), nil
}

// openPinCard verifies and decodes a signed pin link.
func openPinCard(id, d, sig string) (pinCard, bool) {
	var c pinCard
	if d == "" || len(d) > 8192 || !hmac.Equal([]byte(pinCardMAC(id, d)), []byte(sig)) {
		return c, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(d)
	if err != nil {
		return c, false
	}
	js, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(raw)), 64<<10))
	if err != nil || json.Unmarshal(js, &c) != nil || !isPinimgURL(c.O) {
		return c, false
	}
	return c, true
}

// cardHref returns the link for a search result card: its pin page, or ""
// to fall back to the full-size image.
func cardHref(r *searchResult) string {
	if isPinID(r.ID) {
		return "/pin/" + r.ID
	}
	full := fullImage(r.Images.Orig, r.Images.X736)
	if !isOpaquePinID(r.ID) || !isPinimgURL(full.URL) {
		return ""
	}
	c := pinCard{
		T: truncateRunes(firstNonEmpty(r.Title, r.GridTitle), 200),
		D: truncateRunes(r.Description, 500),
		O: full.URL,
		W: full.Width,
		H: full.Height,
		A: truncateRunes(firstNonEmpty(r.AutoAltText, r.SeoAltText), 300),
		M: r.Domain,
		C: r.CreatedAt,
		V: len(r.Videos) > 0 && string(r.Videos) != "null",
		K: r.DominantColor,
	}
	if r.Pinner != nil {
		c.U, c.N = r.Pinner.Username, r.Pinner.FullName
	}
	if r.Board != nil {
		c.B = boardName(r.Board.Name, r.Board.URL)
	}
	if r.Aggregated != nil {
		c.S = r.Aggregated.Stats.Saves
	}
	d, sig, err := signPinCard(r.ID, c)
	if err != nil {
		return ""
	}
	return "/pin/" + r.ID + "?d=" + d + "&s=" + sig
}

// pinHandler serves /pin/{id}. Numeric IDs (also in Pinterest's
// "<slug>--<id>" form) are fetched from PinResource; opaque IDs from search
// results are rendered from their signed link.
func pinHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if i := strings.LastIndex(id, "--"); i >= 0 {
		id = id[i+2:]
	}
	if isPinID(id) {
		pinFromPinterest(w, r, id)
		return
	}
	if isOpaquePinID(id) {
		q := r.URL.Query()
		if c, ok := openPinCard(id, q.Get("d"), q.Get("s")); ok {
			renderPinPage(w, r, pinView{
				Link: r.URL.RequestURI(), Title: c.T, Desc: c.D, Orig: c.O, Width: c.W, Height: c.H, Alt: c.A,
				Domain: c.M, PinnerUser: c.U, PinnerName: c.N, Board: c.B, CreatedAt: c.C, Saves: c.S, IsVideo: c.V, Color: c.K,
			})
			return
		}
	}
	renderErrorPage(w, r, http.StatusNotFound, "That isn't a valid pin address.")
}

func pinFromPinterest(w http.ResponseWriter, r *http.Request, id string) {
	data, _ := json.Marshal(map[string]any{"options": map[string]any{"id": id, "field_set_key": "detailed"}})
	req, err := http.NewRequestWithContext(r.Context(), "GET", pinterestPinURL+"?data="+url.QueryEscape(string(data)), nil)
	if err != nil {
		renderErrorPage(w, r, http.StatusInternalServerError, "Couldn't build the request to Pinterest.")
		return
	}
	req.Header.Set("x-pinterest-pws-handler", "www/pin/[id].js")

	resp, err := httpClient.Do(req)
	if err != nil {
		renderFetchError(w, r, "pin", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		renderErrorPage(w, r, http.StatusNotFound, "This pin doesn't exist or was deleted.")
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		renderUpstreamError(w, r, "pin", resp)
		return
	}

	var body struct {
		ResourceResponse struct {
			Data *pinData `json:"data"`
		} `json:"resource_response"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil || body.ResourceResponse.Data == nil {
		log.Printf("pinterest pin: unreadable response (decode error: %v)", err)
		renderErrorPage(w, r, http.StatusBadGateway, "Pinterest sent a response Pinata couldn't read.")
		return
	}
	p := body.ResourceResponse.Data
	orig := fullImage(p.Images["orig"], p.Images["736x"])
	if !isPinimgURL(orig.URL) {
		renderErrorPage(w, r, http.StatusBadGateway, "This pin has no image Pinata can show.")
		return
	}
	v := pinView{
		Link: "/pin/" + id, Title: p.Title, Desc: firstNonEmpty(p.CloseupDescription, p.Description),
		Orig: orig.URL, Width: orig.Width, Height: orig.Height, Alt: firstNonEmpty(p.AltText, p.AutoAltText),
		SourceURL: p.Link, Domain: p.Domain, CreatedAt: p.CreatedAt, IsVideo: p.IsVideo, Color: p.DominantColor,
	}
	if v.Title == "" {
		v.Title = p.GridTitle
	}
	if p.Pinner != nil {
		v.PinnerUser, v.PinnerName = p.Pinner.Username, p.Pinner.FullName
	}
	if p.Board != nil {
		v.Board = boardName(p.Board.Name, p.Board.URL)
	}
	if p.Aggregated != nil {
		v.Saves = p.Aggregated.Stats.Saves
	}
	renderPinPage(w, r, v)
}

func renderPinPage(w http.ResponseWriter, r *http.Request, v pinView) {
	// Many pins have a description but no title: then the heading is left out
	// instead of a bare "Pin", and the page title uses the description.
	heading := strings.TrimSpace(v.Title)
	desc := strings.TrimSpace(v.Desc)
	if strings.EqualFold(desc, heading) {
		desc = ""
	}
	title := firstNonEmpty(heading, truncateRunes(desc, 80), "Pin")
	alt := firstNonEmpty(v.Alt, title)
	full := "/image_proxy?url=" + url.QueryEscape(v.Orig)

	var b strings.Builder
	b.WriteString(pageHead(r, title+" - Pinata"))
	b.WriteString(`<div class="header"><a class="brand" href="/">Pinata</a><div class="search-box"><form class="search-inline" method="get" action="/search"><input type="text" name="q" placeholder="Search Image" required maxlength="64"><button type="submit">Search</button></form></div></div>`)
	b.WriteString(`<div class="pin"><div class="pin-media"><a href="` + html.EscapeString(full) + `" target="_blank" rel="noreferrer"><img src="` + html.EscapeString(full) + `" alt="` + html.EscapeString(alt) + `"` + placeholderStyle(v.Color))
	if v.Width > 0 && v.Height > 0 {
		b.WriteString(fmt.Sprintf(` width="%d" height="%d"`, v.Width, v.Height))
	}
	b.WriteString(` decoding="async"></a></div><div class="pin-info">`)
	if heading != "" {
		b.WriteString(`<h2>` + html.EscapeString(heading) + `</h2>`)
	}
	if desc != "" {
		b.WriteString(`<p class="pin-desc">` + html.EscapeString(desc) + `</p>`)
	}

	b.WriteString(`<div class="pin-meta">`)
	if v.IsVideo {
		b.WriteString(`<div>This is a video pin: Pinata shows its cover image, video playback isn't supported yet.</div>`)
	}
	if lu, err := url.Parse(v.SourceURL); err == nil && (lu.Scheme == "http" || lu.Scheme == "https") && lu.Host != "" {
		b.WriteString(`<div>Source: <a href="` + html.EscapeString(lu.String()) + `" target="_blank" rel="noreferrer noopener">` + html.EscapeString(firstNonEmpty(v.Domain, lu.Hostname())) + `</a></div>`)
	} else if v.Domain != "" {
		b.WriteString(`<div>Source: ` + html.EscapeString(v.Domain) + `</div>`)
	}
	if v.PinnerUser != "" {
		who := html.EscapeString("@" + v.PinnerUser)
		if v.PinnerName != "" {
			who = html.EscapeString(v.PinnerName) + " (" + who + ")"
		}
		b.WriteString(`<div>Saved by ` + who + `</div>`)
	}
	if v.Board != "" {
		b.WriteString(`<div>Board: ` + html.EscapeString(v.Board) + `</div>`)
	}
	if t, err := time.Parse(time.RFC1123Z, v.CreatedAt); err == nil {
		b.WriteString(`<div>Created ` + t.Format("2 Jan 2006") + `</div>`)
	}
	if v.Saves > 0 {
		saves := "saves"
		if v.Saves == 1 {
			saves = "save"
		}
		b.WriteString(fmt.Sprintf(`<div>%d %s</div>`, v.Saves, saves))
	}
	b.WriteString(`</div><div class="pin-actions">`)
	if !disableReverse {
		b.WriteString(`<a class="magnifier" href="/revsearch?b64=` + base64.StdEncoding.EncodeToString([]byte(v.Orig)) + `" title="Search Tineye" target="_blank">🔍</a>`)
	}
	if bookmarkingEnabled {
		b.WriteString(pinSaveFrame(v.Link, v.Orig))
	}
	b.WriteString(`<a class="btn-save" href="` + html.EscapeString(full) + `" target="_blank" rel="noreferrer" style="text-decoration:none;">Full size</a>`)
	b.WriteString(`</div></div></div></body></html>`)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, b.String())
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
	if targetW > 1040 { // the largest width cards ask for: 520 px at 200% scale
		targetW = 1040
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

// The save buttons on cards, pin pages and search pages are small iframes
// served by /frame/save. Saving or removing posts inside the frame and
// reloads only the frame, which comes back in its new state: the page isn't
// reloaded, keeps its place, and Pinterest isn't asked for the search again.

// saveTarget validates what a save button stores: a pinimg image URL or a
// search query.
// saveTarget validates what a save button stores: a pin page link (with its
// image for numeric pins), a pinimg image URL or a search query. It returns
// the value and the image to store.
func saveTarget(typ, v, img string) (string, string, bool) {
	v = strings.TrimSpace(v)
	switch typ {
	case "pin":
		if len(v) > maxPinLink {
			return "", "", false
		}
		if _, _, ok := pinLinkInfo(v, img); !ok {
			return "", "", false
		}
		if !isPinID(strings.TrimPrefix(v, "/pin/")) {
			img = "" // signed links carry their image
		}
		return v, img, true
	case "img":
		return v, "", len(v) <= maxItemLen && isPinimgURL(v)
	case "q":
		return v, "", v != "" && utf8.RuneCountInString(v) <= maxQueryRunes
	}
	return "", "", false
}

func saveFrameURL(typ, v, img string) string {
	u := "/frame/save?type=" + url.QueryEscape(typ) + "&v=" + url.QueryEscape(v)
	if img != "" {
		u += "&img=" + url.QueryEscape(img)
	}
	return u
}

// saveFrameTag returns the iframe that embeds a save button.
func saveFrameTag(typ, v, img string) string {
	class, title := "save-frame", "Save pin"
	if typ == "q" {
		class, title = "save-frame save-frame-q", "Save search"
	} else if typ == "img" {
		title = "Save image"
	}
	return `<iframe class="` + class + `" src="` + html.EscapeString(saveFrameURL(typ, v, img)) + `" title="` + title + `" loading="lazy" scrolling="no"></iframe>`
}

// pinSaveFrame returns the save button for a pin: it saves the pin page link,
// or just the image when the link is too long to store.
func pinSaveFrame(link, img string) string {
	if strings.HasPrefix(link, "/pin/") && len(link) <= maxPinLink {
		return saveFrameTag("pin", link, img)
	}
	return saveFrameTag("img", img, "")
}

func saveFrameHandler(w http.ResponseWriter, r *http.Request) {
	if !bookmarkingEnabled {
		http.Error(w, "bookmarks disabled", http.StatusNotFound)
		return
	}
	typ := r.URL.Query().Get("type")
	v, img, ok := saveTarget(typ, r.URL.Query().Get("v"), r.URL.Query().Get("img"))
	if !ok {
		http.Error(w, "invalid bookmark", http.StatusBadRequest)
		return
	}
	saved := false
	for _, e := range readBookmarksFromReq(r) {
		if e.Type == typ && e.Value == v {
			saved = true
			break
		}
	}
	accent, _ := getThemeVars(r)
	action, label, title, class := "add", "❤", "Save", "heart"
	if typ == "q" {
		label, title, class = "Save", "Save search", "text"
	}
	if saved {
		action, title, class = "remove", "Remove from bookmarks", class+" saved"
		if typ == "q" {
			label = "Saved ✓"
		}
	}
	css := `html,body{margin:0;padding:0;background:transparent;overflow:hidden}form{margin:0}button{cursor:pointer;font:700 14px ui-monospace,Menlo,Monaco,monospace;display:inline-flex;align-items:center;justify-content:center}` +
		`.heart{width:34px;height:34px;padding:0;border-radius:999px;background:rgba(0,0,0,0.45);border:1px solid rgba(255,255,255,0.06);color:#e6e6ff;font-size:15px}` +
		`.heart.saved{color:#ff4d6d;background:rgba(255,77,109,0.18);border-color:rgba(255,77,109,0.55)}` +
		`.text{height:36px;width:96px;border:none;border-radius:8px;color:#fff;background:linear-gradient(90deg,` + html.EscapeString(accent) + `,#5b21b6)}` +
		`.text.saved{background:rgba(255,255,255,0.08);border:1px solid ` + html.EscapeString(accent) + `}`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><style>`+css+`</style></head><body>`+
		`<form method="post" action="/frame/save"><input type="hidden" name="type" value="`+html.EscapeString(typ)+`"><input type="hidden" name="v" value="`+html.EscapeString(v)+`"><input type="hidden" name="img" value="`+html.EscapeString(img)+`"><input type="hidden" name="action" value="`+action+`">`+
		`<button type="submit" class="`+class+`" title="`+title+`">`+label+`</button></form></body></html>`)
}

func saveFramePostHandler(w http.ResponseWriter, r *http.Request) {
	if !bookmarkingEnabled {
		http.Error(w, "bookmarks disabled", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	typ := r.PostFormValue("type")
	v, img, ok := saveTarget(typ, r.PostFormValue("v"), r.PostFormValue("img"))
	if !ok {
		http.Error(w, "invalid bookmark", http.StatusBadRequest)
		return
	}
	out := make([]BookmarkEntry, 0, maxBookmarks)
	if r.PostFormValue("action") == "add" {
		out = append(out, BookmarkEntry{Type: typ, Value: v, Image: img})
	}
	for _, e := range readBookmarksFromReq(r) {
		if e.Type == typ && e.Value == v {
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		clearBookmarksCookie(w, r)
	} else {
		setBookmarksCookie(w, r, out)
	}
	http.Redirect(w, r, saveFrameURL(typ, v, img), http.StatusSeeOther)
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
		clearBookmarksCookie(w, r)
	} else {
		setBookmarksCookie(w, r, out)
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
		e, ok := normalizeEntry(e)
		if !ok {
			continue
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
	setBookmarksCookie(w, r, merged)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---------- main ----------
func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /settings", settingsPostHandler)
	mux.HandleFunc("GET /{$}", indexHandler)
	mux.HandleFunc("GET /search", searchHandler)
	mux.HandleFunc("GET /pin/{id}", pinHandler)
	mux.HandleFunc("GET /pin/{id}/{$}", pinHandler)
	mux.HandleFunc("GET /image_proxy", imageProxyHandler)
	mux.HandleFunc("GET /revsearch", revsearchHandler)
	mux.HandleFunc("GET /thumb_proxy", thumbImageProxyHandler)

	// bookmark endpoints: anything that changes state is POST-only
	mux.HandleFunc("GET /frame/save", saveFrameHandler)
	mux.HandleFunc("POST /frame/save", saveFramePostHandler)
	mux.HandleFunc("POST /bookmark_remove", bookmarkRemoveHandler)
	mux.HandleFunc("GET /bookmarks/export", bookmarksExportHandler)
	mux.HandleFunc("POST /bookmarks/import", bookmarksImportHandler)

	// Reject cross-origin browser requests with unsafe methods (CSRF),
	// based on Sec-Fetch-Site with an Origin/Host fallback. Stateless: no tokens.
	csrf := http.NewCrossOriginProtection()
	csrf.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		renderErrorPage(w, r, http.StatusForbidden, "This request came from another site, so it was blocked to protect your bookmarks and settings.")
	}))

	server := &http.Server{
		Addr:         ":8080",
		Handler:      securityHeaders(csrf.Handler(withErrorPages(mux))),
		ReadTimeout:  12 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
		BaseContext: func(net.Listener) context.Context { return context.Background() },
	}

	log.Println("Pinata listening on :8080 (no-JS mode). Bookmarking enabled:", bookmarkingEnabled, " Reverse disabled:", disableReverse)
	log.Fatal(server.ListenAndServe())
}