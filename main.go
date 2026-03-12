package main

import (
	"bytes"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type clip struct {
	Publication string
	PublishedAt *time.Time
	Title       string
	Link        string
	Snippet     string
	Source      string
}

type providerResult struct {
	Name         string
	Clips        []clip
	RawCount     int
	RecentCount  int
	DurationMS   int64
	Err          error
}

func main() {
	loadDotEnv(".env")
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "3000"
	}
	log.Printf("env status BRAVE_API_KEY=%s EXA_API_KEY=%s NEWS_API_KEY=%s", maskedEnvStatus("BRAVE_API_KEY"), maskedEnvStatus("EXA_API_KEY"), maskedEnvStatus("NEWS_API_KEY"))

	http.HandleFunc("/", handleStatic)
	http.HandleFunc("/styles.css", handleStatic)
	http.HandleFunc("/search", handleSearch)

	addr := ":" + port
	log.Printf("PressClips server running at http://localhost:%s", port)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}

func loadDotEnv(path string) {
	file, err := os.Open(path)
	if err != nil {
		log.Printf("dotenv: %s not loaded (%v)", path, err)
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		val = strings.Trim(val, `"'`)
		if key == "" {
			continue
		}

		// .env should be source of truth for local dev.
		_ = os.Setenv(key, val)
	}

	if err := scanner.Err(); err != nil {
		log.Printf("dotenv: scan error: %v", err)
	}
}

func maskedEnvStatus(name string) string {
	v := strings.TrimSpace(os.Getenv(name))
	return maskedValue(v)
}

func maskedValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "missing"
	}
	if len(v) <= 6 {
		return "***"
	}
	return v[:3] + "..." + v[len(v)-3:]
}

func handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}

	var file string
	switch r.URL.Path {
	case "/":
		file = "index.html"
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case "/styles.css":
		file = "styles.css"
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	default:
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	path := filepath.Join(".", file)
	content, err := os.ReadFile(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}

	if err := r.ParseForm(); err != nil {
		writeHTML(w, http.StatusBadRequest, "<p>Invalid form body.</p>")
		return
	}

	query := strings.TrimSpace(r.FormValue("clientName"))
	if query == "" {
		writeHTML(w, http.StatusBadRequest, "<p>Please enter a client name.</p>")
		return
	}

	braveKey := strings.TrimSpace(os.Getenv("BRAVE_API_KEY"))
	exaKey := strings.TrimSpace(os.Getenv("EXA_API_KEY"))
	newsAPIKey := strings.TrimSpace(os.Getenv("NEWS_API_KEY"))
	log.Printf("request env BRAVE_API_KEY=%s EXA_API_KEY=%s NEWS_API_KEY=%s", maskedValue(braveKey), maskedValue(exaKey), maskedValue(newsAPIKey))

	if braveKey == "" || exaKey == "" || newsAPIKey == "" {
		writeHTML(w, http.StatusInternalServerError, "<p>Missing API keys. Set BRAVE_API_KEY, EXA_API_KEY, and NEWS_API_KEY in your environment.</p>")
		return
	}

	since := time.Now().UTC().Add(-24 * time.Hour)
	log.Printf("search start query=%q since=%s", query, since.Format(time.RFC3339))

	type call struct {
		name string
		fn   func(context.Context) providerResult
	}

	calls := []call{
		{name: "Brave", fn: func(ctx context.Context) providerResult { return searchBrave(ctx, query, since, braveKey) }},
		{name: "Exa", fn: func(ctx context.Context) providerResult { return searchExa(ctx, query, since, exaKey) }},
		{name: "NewsAPI", fn: func(ctx context.Context) providerResult { return searchNewsAPI(ctx, query, since, newsAPIKey) }},
	}

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	resultsCh := make(chan providerResult, len(calls))
	var wg sync.WaitGroup

	for _, c := range calls {
		wg.Add(1)
		go func(c call) {
			defer wg.Done()
			resultsCh <- c.fn(ctx)
		}(c)
	}

	wg.Wait()
	close(resultsCh)

	merged := make([]clip, 0, 256)
	errors := make([]string, 0, len(calls))
	stats := make([]providerResult, 0, len(calls))

	for res := range resultsCh {
		stats = append(stats, res)
		if res.Err != nil {
			errors = append(errors, fmt.Sprintf("%s: %s", res.Name, res.Err.Error()))
			continue
		}
		merged = append(merged, res.Clips...)
	}

	unique := dedupeAndSort(merged)
	beforeFilter := len(unique)
	unique = filterClipsByQuery(unique, query)
	log.Printf("search done query=%q providers=%d merged=%d unique_before_filter=%d unique_after_filter=%d", query, len(stats), len(merged), beforeFilter, len(unique))

	fragment := renderResultsFragment(unique, query, errors, stats)
	writeHTML(w, http.StatusOK, fragment)
}

func writeHTML(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func normalizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	host := strings.ToLower(strings.TrimPrefix(u.Hostname(), "www."))
	path := strings.TrimSuffix(u.EscapedPath(), "/")
	if path == "" {
		path = "/"
	}
	return host + path
}

func domainFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || strings.TrimSpace(u.Hostname()) == "" {
		return "Unknown publication"
	}
	return strings.TrimPrefix(u.Hostname(), "www.")
}

func parseAnyTime(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	layouts := []string{
		time.RFC3339,
		time.RFC3339Nano,
		time.RFC1123Z,
		time.RFC1123,
		time.RFC822,
		time.RFC822Z,
		time.ANSIC,
		time.UnixDate,
		"2006-01-02",
	}

	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			t := parsed.UTC()
			return &t
		}
	}

	return nil
}

func isWithinLast24h(ts *time.Time, since time.Time) bool {
	if ts == nil {
		return false
	}
	return ts.After(since)
}

func dedupeAndSort(items []clip) []clip {
	byURL := make(map[string]clip, len(items))
	for _, c := range items {
		if strings.TrimSpace(c.Link) == "" {
			continue
		}

		key := normalizeURL(c.Link)
		existing, found := byURL[key]
		if !found {
			byURL[key] = c
			continue
		}

		existingUnix := int64(0)
		if existing.PublishedAt != nil {
			existingUnix = existing.PublishedAt.Unix()
		}

		nextUnix := int64(0)
		if c.PublishedAt != nil {
			nextUnix = c.PublishedAt.Unix()
		}

		if nextUnix > existingUnix {
			byURL[key] = c
		}
	}

	unique := make([]clip, 0, len(byURL))
	for _, c := range byURL {
		unique = append(unique, c)
	}

	sort.Slice(unique, func(i, j int) bool {
		ti := int64(0)
		tj := int64(0)
		if unique[i].PublishedAt != nil {
			ti = unique[i].PublishedAt.Unix()
		}
		if unique[j].PublishedAt != nil {
			tj = unique[j].PublishedAt.Unix()
		}
		return ti > tj
	})

	return unique
}

func filterClipsByQuery(items []clip, query string) []clip {
	normQuery := normalizeText(query)
	if normQuery == "" {
		return items
	}
	tokens := strings.Fields(normQuery)

	filtered := make([]clip, 0, len(items))
	for _, c := range items {
		haystack := normalizeText(strings.Join([]string{c.Title, c.Snippet, c.Link, c.Publication}, " "))
		if haystack == "" {
			continue
		}

		// Keep direct phrase matches first.
		if strings.Contains(haystack, normQuery) {
			filtered = append(filtered, c)
			continue
		}

		// Fallback: keep only if every token from the client name appears.
		allTokensPresent := true
		for _, token := range tokens {
			if !strings.Contains(haystack, token) {
				allTokensPresent = false
				break
			}
		}
		if allTokensPresent {
			filtered = append(filtered, c)
		}
	}

	return filtered
}

func normalizeText(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(s))
	lastSpace := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastSpace = false
			continue
		}
		if !lastSpace {
			b.WriteRune(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

func renderResultsFragment(results []clip, query string, errs []string, stats []providerResult) string {
	var b strings.Builder

	if len(errs) > 0 {
		b.WriteString(`<p class="warning">Partial results: `)
		b.WriteString(html.EscapeString(strings.Join(errs, " | ")))
		b.WriteString(`</p>`)
	}

	b.WriteString(`<p class="count">Provider diagnostics:</p><ul class="diag">`)
	for _, s := range stats {
		line := fmt.Sprintf("%s -> raw: %d, within 24h: %d, kept: %d, latency: %dms", s.Name, s.RawCount, s.RecentCount, len(s.Clips), s.DurationMS)
		if s.Err != nil {
			line = fmt.Sprintf("%s -> error: %s", s.Name, s.Err.Error())
		}
		b.WriteString(`<li>`)
		b.WriteString(html.EscapeString(line))
		b.WriteString(`</li>`)
	}
	b.WriteString(`</ul>`)

	if len(results) == 0 {
		b.WriteString(fmt.Sprintf("<p>No clips found for <strong>%s</strong> in the past 24 hours.</p>", html.EscapeString(query)))
		return b.String()
	}

	b.WriteString(fmt.Sprintf(`<p class="count">%d unique result(s)</p>`, len(results)))
	for _, row := range results {
		published := "Unknown date"
		if row.PublishedAt != nil {
			published = row.PublishedAt.Local().Format("Jan 2, 2006 3:04 PM MST")
		}

		b.WriteString(`<article class="result-item" role="listitem"><div class="pub-date">`)
		b.WriteString(html.EscapeString(row.Publication))
		b.WriteString(` (`)
		b.WriteString(html.EscapeString(published))
		b.WriteString(`)</div><h3 class="title">`)
		b.WriteString(html.EscapeString(row.Title))
		b.WriteString(`</h3><a class="url" href="`)
		b.WriteString(html.EscapeString(row.Link))
		b.WriteString(`" target="_blank" rel="noopener noreferrer">`)
		b.WriteString(html.EscapeString(row.Link))
		b.WriteString(`</a></article>`)
	}

	return b.String()
}

func searchBrave(ctx context.Context, clientName string, since time.Time, apiKey string) providerResult {
	started := time.Now()
	res := providerResult{Name: "Brave"}

	u, err := url.Parse("https://api.search.brave.com/res/v1/news/search")
	if err != nil {
		res.Err = err
		return finalizeProviderResult(res, started)
	}

	q := u.Query()
	q.Set("q", fmt.Sprintf("\"%s\"", clientName))
	q.Set("freshness", "pd")
	q.Set("count", "50")
	u.RawQuery = q.Encode()

	log.Printf("provider=Brave request url=%s", u.String())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		res.Err = err
		return finalizeProviderResult(res, started)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", apiKey)

	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		res.Err = err
		return finalizeProviderResult(res, started)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 2048))
		res.Err = fmt.Errorf("api error (%d): %s", httpResp.StatusCode, strings.TrimSpace(string(body)))
		return finalizeProviderResult(res, started)
	}

	type braveResponse struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
			PageAge     string `json:"page_age"`
			MetaURL     struct {
				Hostname string `json:"hostname"`
			} `json:"meta_url"`
		} `json:"results"`
	}

	var payload braveResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&payload); err != nil {
		res.Err = err
		return finalizeProviderResult(res, started)
	}

	res.RawCount = len(payload.Results)
	clipped := make([]clip, 0, len(payload.Results))
	recent := 0
	for _, item := range payload.Results {
		link := strings.TrimSpace(item.URL)
		if link == "" {
			continue
		}

		published := parseAnyTime(item.PageAge)
		if isWithinLast24h(published, since) {
			recent++
		}

		pub := strings.TrimSpace(item.MetaURL.Hostname)
		if pub == "" {
			pub = domainFromURL(link)
		}

		clipped = append(clipped, clip{
			Publication: pub,
			PublishedAt: published,
			Title:       fallback(item.Title, "Untitled"),
			Link:        link,
			Snippet:     item.Description,
			Source:      "Brave",
		})
	}

	res.Clips = clipped
	res.RecentCount = recent
	return finalizeProviderResult(res, started)
}

func searchExa(ctx context.Context, clientName string, since time.Time, apiKey string) providerResult {
	started := time.Now()
	res := providerResult{Name: "Exa"}

	payload := map[string]any{
		"query":              fmt.Sprintf("\"%s\"", clientName),
		"type":               "auto",
		"category":           "news",
		"num_results":        50,
		"startPublishedDate": since.Format(time.RFC3339),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		res.Err = err
		return finalizeProviderResult(res, started)
	}

	log.Printf("provider=Exa request endpoint=https://api.exa.ai/search payload=%s", truncateForLog(string(body), 512))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.exa.ai/search", bytes.NewReader(body))
	if err != nil {
		res.Err = err
		return finalizeProviderResult(res, started)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)

	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		res.Err = err
		return finalizeProviderResult(res, started)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(httpResp.Body, 2048))
		res.Err = fmt.Errorf("api error (%d): %s", httpResp.StatusCode, strings.TrimSpace(string(respBody)))
		return finalizeProviderResult(res, started)
	}

	type exaResponse struct {
		Results []struct {
			Title         string `json:"title"`
			URL           string `json:"url"`
			Text          string `json:"text"`
			PublishedDate string `json:"publishedDate"`
			Author        string `json:"author"`
		} `json:"results"`
	}

	var response exaResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&response); err != nil {
		res.Err = err
		return finalizeProviderResult(res, started)
	}

	res.RawCount = len(response.Results)
	clipped := make([]clip, 0, len(response.Results))
	recent := 0
	for _, item := range response.Results {
		link := strings.TrimSpace(item.URL)
		if link == "" {
			continue
		}

		published := parseAnyTime(item.PublishedDate)
		if isWithinLast24h(published, since) {
			recent++
		}

		pub := strings.TrimSpace(item.Author)
		if pub == "" {
			pub = domainFromURL(link)
		}

		clipped = append(clipped, clip{
			Publication: pub,
			PublishedAt: published,
			Title:       fallback(item.Title, "Untitled"),
			Link:        link,
			Snippet:     item.Text,
			Source:      "Exa",
		})
	}

	res.Clips = clipped
	res.RecentCount = recent
	return finalizeProviderResult(res, started)
}

func searchNewsAPI(ctx context.Context, clientName string, since time.Time, apiKey string) providerResult {
	started := time.Now()
	res := providerResult{Name: "NewsAPI"}

	u, err := url.Parse("https://newsapi.org/v2/everything")
	if err != nil {
		res.Err = err
		return finalizeProviderResult(res, started)
	}

	q := u.Query()
	q.Set("q", fmt.Sprintf("\"%s\"", clientName))
	q.Set("from", since.Format(time.RFC3339))
	q.Set("sortBy", "publishedAt")
	q.Set("language", "en")
	q.Set("searchIn", "title,description,content")
	q.Set("pageSize", "100")
	u.RawQuery = q.Encode()

	log.Printf("provider=NewsAPI request url=%s", u.String())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		res.Err = err
		return finalizeProviderResult(res, started)
	}
	req.Header.Set("X-Api-Key", apiKey)

	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		res.Err = err
		return finalizeProviderResult(res, started)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(httpResp.Body, 2048))
		res.Err = fmt.Errorf("api error (%d): %s", httpResp.StatusCode, strings.TrimSpace(string(respBody)))
		return finalizeProviderResult(res, started)
	}

	type newsResponse struct {
		Articles []struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			URL         string `json:"url"`
			PublishedAt string `json:"publishedAt"`
			Source      struct {
				Name string `json:"name"`
			} `json:"source"`
		} `json:"articles"`
	}

	var response newsResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&response); err != nil {
		res.Err = err
		return finalizeProviderResult(res, started)
	}

	res.RawCount = len(response.Articles)
	clipped := make([]clip, 0, len(response.Articles))
	recent := 0
	for _, item := range response.Articles {
		link := strings.TrimSpace(item.URL)
		if link == "" {
			continue
		}

		published := parseAnyTime(item.PublishedAt)
		if isWithinLast24h(published, since) {
			recent++
		}

		pub := strings.TrimSpace(item.Source.Name)
		if pub == "" {
			pub = domainFromURL(link)
		}

		clipped = append(clipped, clip{
			Publication: pub,
			PublishedAt: published,
			Title:       fallback(item.Title, "Untitled"),
			Link:        link,
			Snippet:     item.Description,
			Source:      "NewsAPI",
		})
	}

	res.Clips = clipped
	res.RecentCount = recent
	return finalizeProviderResult(res, started)
}

func finalizeProviderResult(res providerResult, started time.Time) providerResult {
	res.DurationMS = time.Since(started).Milliseconds()
	if res.Err != nil {
		log.Printf("provider=%s status=error latency_ms=%d error=%s", res.Name, res.DurationMS, res.Err.Error())
	} else {
		log.Printf("provider=%s status=ok latency_ms=%d raw=%d within_24h=%d kept=%d", res.Name, res.DurationMS, res.RawCount, res.RecentCount, len(res.Clips))
	}
	return res
}

func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func fallback(value, defaultValue string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return defaultValue
	}
	return trimmed
}
