package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sqrll-net/squirrel-communicator-image/internal/auth"
	"github.com/sqrll-net/squirrel-communicator-image/internal/cache"
	"github.com/sqrll-net/squirrel-communicator-image/internal/config"
	"github.com/sqrll-net/squirrel-communicator-image/internal/sniff"
	"github.com/sqrll-net/squirrel-communicator-image/internal/storage"
)

const (
	klipyBaseURL = "https://api.klipy.com"
	maxGifBytes  = 8 * 1024 * 1024
	klipyTimeout = 12 * time.Second
	fetchTimeout = 25 * time.Second
)

/** GifsHandler proxies KLIPY GIF search/trending and fetches remote GIFs for re-upload. */
type GifsHandler struct {
	Auth     *auth.Manager
	KlipyKey string
	Storage  *storage.Manager
	Cache    *cache.Cache

	klipy   *http.Client // KLIPY API calls (12s timeout)
	fetcher *http.Client // remote GIF fetch (25s timeout, no redirects)
}

/** NewGifsHandler builds a GifsHandler with its two HTTP clients. */
func NewGifsHandler(authMgr *auth.Manager, klipyKey string, store *storage.Manager, c *cache.Cache) *GifsHandler {
	return &GifsHandler{
		Auth:     authMgr,
		KlipyKey: klipyKey,
		Storage:  store,
		Cache:    c,
		klipy:    &http.Client{Timeout: klipyTimeout},
		fetcher: &http.Client{
			Timeout: fetchTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// Do not follow redirects: a redirect could bypass the SSRF host check.
				return http.ErrUseLastResponse
			},
		},
	}
}

/** ServeHTTP implements http.Handler for /api/gifs/*. */
func (h *GifsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Deployment-level: GIF provider not configured.
	if h.KlipyKey == "" {
		writeError(w, "GIF provider not configured", http.StatusServiceUnavailable)
		return
	}

	// Auth: same API key issued on login, accepted via either header.
	apiKey := h.extractKey(r)
	if !h.Auth.Authenticate(apiKey) {
		writeError(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Per-key rate limit (reuses the sliding-window limiter).
	if ok, retry := h.Auth.Allow(apiKey); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		writeError(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	switch strings.TrimPrefix(r.URL.Path, "/api/gifs/") {
	case "search":
		h.handleSearch(w, r)
	case "trending":
		h.handleTrending(w, r)
	case "fetch":
		h.handleFetch(w, r)
	default:
		writeError(w, "not found", http.StatusNotFound)
	}
}

/** extractKey accepts the key from X-SQRLL-API-KEY first, then X-API-Token. */
func (h *GifsHandler) extractKey(r *http.Request) string {
	if k := r.Header.Get("X-SQRLL-API-KEY"); k != "" {
		return k
	}
	return r.Header.Get("X-API-Token")
}

/** handleSearch proxies GET /api/gifs/search. */
func (h *GifsHandler) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeError(w, "missing query", http.StatusBadRequest)
		return
	}

	limit := clampInt(r.URL.Query().Get("limit"), 25, 1, 50)
	page := clampInt(r.URL.Query().Get("page"), 1, 1, 1000)

	query := url.Values{}
	query.Set("q", q)
	query.Set("per_page", strconv.Itoa(limit))
	query.Set("page", strconv.Itoa(page))
	query.Set("content_filter", "off")

	h.proxyKlipy(w, "gifs/search", query)
}

/** handleTrending proxies GET /api/gifs/trending. */
func (h *GifsHandler) handleTrending(w http.ResponseWriter, r *http.Request) {
	limit := clampInt(r.URL.Query().Get("limit"), 25, 1, 50)
	page := clampInt(r.URL.Query().Get("page"), 1, 1, 1000)

	query := url.Values{}
	query.Set("per_page", strconv.Itoa(limit))
	query.Set("page", strconv.Itoa(page))
	query.Set("content_filter", "off")

	h.proxyKlipy(w, "gifs/trending", query)
}

/** handleFetch downloads a remote GIF (SSRF-guarded) and re-uploads it into storage. */
func (h *GifsHandler) handleFetch(w http.ResponseWriter, r *http.Request) {
	rawURL := strings.TrimSpace(r.URL.Query().Get("url"))
	if rawURL == "" {
		writeError(w, "missing url", http.StatusBadRequest)
		return
	}

	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		writeError(w, "invalid url", http.StatusBadRequest)
		return
	}

	// SSRF guard: only public hosts.
	if !isPublicHost(u.Hostname()) {
		writeError(w, "invalid host", http.StatusBadRequest)
		return
	}

	resp, err := h.fetcher.Get(u.String())
	if err != nil {
		log.Printf("gif fetch transport error: %v", err)
		writeError(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// 8 MB cap: read one extra byte to detect oversized content.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxGifBytes+1))
	if err != nil {
		writeError(w, "upstream read failed", http.StatusBadGateway)
		return
	}
	if len(data) > maxGifBytes {
		writeError(w, "content too large", http.StatusRequestEntityTooLarge)
		return
	}
	if resp.StatusCode != http.StatusOK {
		writeError(w, "upstream error", http.StatusBadGateway)
		return
	}
	if len(data) == 0 {
		writeError(w, "empty content", http.StatusBadGateway)
		return
	}

	// Content validation: reuse the magic-byte sniffing from uploads.
	mimeType := sniff.Detect(data)
	if !config.AllowedMIMETypes[mimeType] {
		log.Printf("Rejected GIF fetch: detected MIME %s is not allowed", mimeType)
		writeError(w, "unsupported file type", http.StatusUnsupportedMediaType)
		return
	}

	hash := sha256.Sum256(data)
	hashHex := hex.EncodeToString(hash[:])

	if h.Storage.Exists(hashHex) {
		h.Cache.Put(hashHex, data, mimeType)
		writeJSON(w, http.StatusOK, uploadResponse{
			ID:     hashHex,
			Status: "duplicate",
			Type:   mimeType,
			Size:   int64(len(data)),
		})
		return
	}

	if err := h.Storage.Write(hashHex, data, mimeType); err != nil {
		log.Printf("gif fetch write error: %v", err)
		writeError(w, "storage write failed", http.StatusInternalServerError)
		return
	}
	h.Cache.Put(hashHex, data, mimeType)

	writeJSON(w, http.StatusCreated, uploadResponse{
		ID:     hashHex,
		Status: "ok",
		Type:   mimeType,
		Size:   int64(len(data)),
	})
}

/** proxyKlipy calls the KLIPY upstream and normalizes the envelope into the frontend shape. */
func (h *GifsHandler) proxyKlipy(w http.ResponseWriter, path string, query url.Values) {
	body, err := klipyGet(h.KlipyKey, path, query, h.klipy)
	if err != nil {
		log.Printf("klipy upstream error: %v", err)
		writeError(w, "bad gateway", http.StatusBadGateway)
		return
	}

	var env klipyEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		log.Printf("klipy malformed JSON: %v", err)
		writeError(w, "bad gateway", http.StatusBadGateway)
		return
	}

	writeJSON(w, http.StatusOK, gifsResponse{
		Results: normalizeGifs(env.Data.Data),
		HasNext: env.Data.HasNext,
	})
}

/** klipyGet performs a GET against the KLIPY API and returns the raw body.
 *  The key is embedded in the URL path server-side and never returned or logged. */
func klipyGet(key, path string, query url.Values, client *http.Client) ([]byte, error) {
	u := fmt.Sprintf("%s/api/v1/%s/%s", klipyBaseURL, url.PathEscape(key), path)
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	resp, err := client.Get(u)
	if err != nil {
		// Do NOT wrap err: it contains the URL (which embeds the key).
		return nil, errors.New("upstream request failed")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGifBytes))
	if err != nil {
		return nil, fmt.Errorf("read upstream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	return body, nil
}

/** isPublicHost reports whether a hostname resolves only to public IPs. */
func isPublicHost(host string) bool {
	addrs, err := net.LookupIP(host)
	if err != nil || len(addrs) == 0 {
		return false
	}
	for _, ip := range addrs {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
			return false
		}
	}
	return true
}

/** clampInt parses s as an int clamped to [min, max], falling back to def when empty/invalid. */
func clampInt(s string, def, min, max int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

/* ---- KLIPY wire types ---- */

type klipyEnvelope struct {
	Result bool `json:"result"`
	Data   struct {
		Data    []klipyMedia `json:"data"`
		HasNext bool         `json:"has_next"`
	} `json:"data"`
}

type klipyMedia struct {
	ID    int                          `json:"id"`
	Slug  string                       `json:"slug"`
	Title string                       `json:"title"`
	Type  string                       `json:"type"`
	File  map[string]klipyRenditionSet `json:"file"` // key: hd | md | sm | xs
}

type klipyRenditionSet struct {
	GIF  *klipyRendition `json:"gif"`
	WebP *klipyRendition `json:"webp"`
	JPG  *klipyRendition `json:"jpg"`
	MP4  *klipyRendition `json:"mp4"`
	WebM *klipyRendition `json:"webm"`
	PNG  *klipyRendition `json:"png"`
}

type klipyRendition struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Size   int    `json:"size"`
}

/* ---- Normalized frontend types ---- */

type gifsResponse struct {
	Results []gifResult `json:"results"`
	HasNext bool        `json:"has_next"`
}

type gifResult struct {
	ID      int           `json:"id"`
	Slug    string        `json:"slug"`
	Title   string        `json:"title"`
	Preview *gifRendition `json:"preview"`
	GIF     *gifRendition `json:"gif"`
	MP4     *gifRendition `json:"mp4,omitempty"`
}

type gifRendition struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

/** normalizeGifs skips ads and items without a usable GIF, and selects renditions. */
func normalizeGifs(items []klipyMedia) []gifResult {
	results := make([]gifResult, 0, len(items))
	for _, it := range items {
		if strings.EqualFold(it.Type, "ad") {
			continue
		}
		gif := pickGif(it.File)
		if gif == nil {
			continue
		}
		results = append(results, gifResult{
			ID:      it.ID,
			Slug:    it.Slug,
			Title:   it.Title,
			Preview: pickPreview(it.File),
			GIF:     gif,
			MP4:     pickMP4(it.File),
		})
	}
	return results
}

/** pickGif picks the gif rendition from sizes md > sm > hd > xs. */
func pickGif(f map[string]klipyRenditionSet) *gifRendition {
	for _, size := range []string{"md", "sm", "hd", "xs"} {
		if rs, ok := f[size]; ok && rs.GIF != nil && rs.GIF.URL != "" {
			return toRendition(rs.GIF)
		}
	}
	return nil
}

/** pickMP4 picks the mp4 rendition from sizes md > sm > hd > xs; nil if absent. */
func pickMP4(f map[string]klipyRenditionSet) *gifRendition {
	for _, size := range []string{"md", "sm", "hd", "xs"} {
		if rs, ok := f[size]; ok && rs.MP4 != nil && rs.MP4.URL != "" {
			return toRendition(rs.MP4)
		}
	}
	return nil
}

/** pickPreview picks a static rendition (webp > jpg > png > gif) from sm > md > xs > hd. */
func pickPreview(f map[string]klipyRenditionSet) *gifRendition {
	for _, size := range []string{"sm", "md", "xs", "hd"} {
		rs, ok := f[size]
		if !ok {
			continue
		}
		for _, r := range []*klipyRendition{rs.WebP, rs.JPG, rs.PNG, rs.GIF} {
			if r != nil && r.URL != "" {
				return toRendition(r)
			}
		}
	}
	return nil
}

func toRendition(r *klipyRendition) *gifRendition {
	return &gifRendition{URL: r.URL, Width: r.Width, Height: r.Height}
}
