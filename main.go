package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type clip struct {
	Publication string
	PublishedAt *time.Time
	Title       string
	Link        string
	Snippet     string
	SearchText  string
	Source      string
}

type providerResult struct {
	Name        string
	Clips       []clip
	RawCount    int
	RecentCount int
	DurationMS  int64
	Err         error
}

type searchDiagnostics struct {
	RawCount      int
	RecentCount   int
	Normalized    int
	UniqueURLs    int
	QueryMatched  int
	OutletAllowed int
	EnglishKept   int
}

func main() {
	loadDotEnv(".env")
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "3000"
	}
	log.Printf("env status BRAVE_API_KEY=%s EXA_API_KEY=%s", maskedEnvStatus("BRAVE_API_KEY"), maskedEnvStatus("EXA_API_KEY"))

	http.HandleFunc("/", handleStatic)
	http.HandleFunc("/styles.css", handleStatic)
	http.HandleFunc("/search", handleSearch)

	listener, actualPort, err := listenOnAvailablePort(port)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("PressClips server running at http://localhost:%s", actualPort)
	if err := http.Serve(listener, nil); err != nil {
		log.Fatal(err)
	}
}

func listenOnAvailablePort(port string) (net.Listener, string, error) {
	listener, err := net.Listen("tcp", ":"+port)
	if err == nil {
		return listener, port, nil
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		return nil, "", err
	}

	listener, err = net.Listen("tcp", ":0")
	if err != nil {
		return nil, "", err
	}

	actualPort := listener.Addr().(*net.TCPAddr).Port
	log.Printf("port %s is busy; using available port %d instead", port, actualPort)
	return listener, fmt.Sprintf("%d", actualPort), nil
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
	log.Printf("request env BRAVE_API_KEY=%s EXA_API_KEY=%s", maskedValue(braveKey), maskedValue(exaKey))

	if braveKey == "" || exaKey == "" {
		writeHTML(w, http.StatusInternalServerError, "<p>Missing API keys. Set BRAVE_API_KEY and EXA_API_KEY in your environment.</p>")
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
	diag := searchDiagnostics{}

	for res := range resultsCh {
		stats = append(stats, res)
		if res.Err != nil {
			errors = append(errors, fmt.Sprintf("%s: %s", res.Name, res.Err.Error()))
			continue
		}
		diag.RawCount += res.RawCount
		diag.RecentCount += res.RecentCount
		diag.Normalized += len(res.Clips)
		merged = append(merged, res.Clips...)
	}

	sort.Slice(stats, func(i, j int) bool {
		return stats[i].Name < stats[j].Name
	})

	unique := dedupeAndSort(merged)
	diag.UniqueURLs = len(unique)
	unique = filterClipsByQuery(unique, query)
	diag.QueryMatched = len(unique)
	unique = filterNonOutletClips(unique)
	diag.OutletAllowed = len(unique)
	unique = filterNonEnglish(unique)
	diag.EnglishKept = len(unique)
	log.Printf("search done query=%q providers=%d raw=%d recent=%d normalized=%d unique_urls=%d after_query_filter=%d after_outlet_filter=%d after_lang_filter=%d", query, len(stats), diag.RawCount, diag.RecentCount, diag.Normalized, diag.UniqueURLs, diag.QueryMatched, diag.OutletAllowed, diag.EnglishKept)

	fragment := renderResultsFragment(unique, query, errors, stats, diag)
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
		"2006-01-02T15:04:05",     // ISO 8601 without timezone (Brave page_age)
		"2006-01-02T15:04:05.000", // ISO 8601 with millis, no timezone
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

		// Merge the best fields from both sources.
		merged := existing

		// Prefer the longer, non-truncated title.
		if isBetterTitle(c.Title, existing.Title) {
			merged.Title = c.Title
		}

		// Prefer a valid timestamp, or the more recent one.
		if c.PublishedAt != nil && (existing.PublishedAt == nil || c.PublishedAt.After(*existing.PublishedAt)) {
			merged.PublishedAt = c.PublishedAt
		}

		// Prefer a meaningful publication name.
		if existing.Publication == "" || existing.Publication == "Unknown publication" {
			if c.Publication != "" && c.Publication != "Unknown publication" {
				merged.Publication = c.Publication
			}
		}

		// Prefer a longer snippet.
		if len(c.Snippet) > len(existing.Snippet) {
			merged.Snippet = c.Snippet
		}

		merged.SearchText = mergeTextContent(existing.SearchText, c.SearchText)

		byURL[key] = merged
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

func mergeTextContent(current, candidate string) string {
	current = strings.TrimSpace(current)
	candidate = strings.TrimSpace(candidate)

	switch {
	case current == "":
		return candidate
	case candidate == "":
		return current
	case strings.Contains(current, candidate):
		return current
	case strings.Contains(candidate, current):
		return candidate
	default:
		return current + "\n" + candidate
	}
}

func isBetterTitle(candidate, current string) bool {
	candidateTrunc := strings.HasSuffix(strings.TrimSpace(candidate), "...")
	currentTrunc := strings.HasSuffix(strings.TrimSpace(current), "...")
	// Prefer non-truncated over truncated.
	if !candidateTrunc && currentTrunc {
		return true
	}
	if candidateTrunc && !currentTrunc {
		return false
	}
	// Both truncated or both complete: prefer longer.
	return len(candidate) > len(current)
}

func cleanTitle(title string) string {
	t := strings.TrimSpace(title)
	// Strip trailing ellipsis variants.
	t = strings.TrimRight(t, " ")
	for _, suffix := range []string{"...", "…"} {
		t = strings.TrimSuffix(t, suffix)
	}
	return strings.TrimSpace(t)
}

func isLikelyEnglish(s string) bool {
	if s == "" {
		return true
	}
	total := 0
	ascii := 0
	for _, r := range s {
		if r == ' ' || (r >= '0' && r <= '9') {
			continue
		}
		total++
		if r <= 127 {
			ascii++
		}
	}
	if total == 0 {
		return true
	}
	return float64(ascii)/float64(total) >= 0.90
}

func filterNonEnglish(items []clip) []clip {
	filtered := make([]clip, 0, len(items))
	for _, c := range items {
		if isLikelyEnglish(c.Title) && isLikelyEnglish(c.Snippet) {
			filtered = append(filtered, c)
		}
	}
	return filtered
}

func filterClipsByQuery(items []clip, query string) []clip {
	normQuery := normalizeText(query)
	if normQuery == "" {
		return items
	}
	tokens := strings.Fields(normQuery)

	filtered := make([]clip, 0, len(items))
	for _, c := range items {
		haystack := normalizeText(strings.Join([]string{c.Title, c.Snippet, c.SearchText, c.Link, c.Publication}, " "))
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

func joinTextParts(parts ...string) string {
	seen := make(map[string]bool, len(parts))
	joined := make([]string, 0, len(parts))

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		joined = append(joined, part)
	}

	return strings.Join(joined, "\n")
}

func bestSnippet(primary string, fallbacks ...string) string {
	primary = strings.TrimSpace(primary)
	if primary != "" {
		return primary
	}
	for _, fallback := range fallbacks {
		fallback = strings.TrimSpace(fallback)
		if fallback != "" {
			return fallback
		}
	}
	return ""
}

func quoteForSearch(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	return `"` + strings.ReplaceAll(raw, `"`, `\"`) + `"`
}

func buildBraveBodyAwareQuery(clientName string) string {
	clientName = strings.TrimSpace(clientName)
	if clientName == "" {
		return ""
	}
	return "inpage:" + quoteForSearch(clientName)
}

func exaIncludeText(clientName string) []string {
	trimmed := strings.TrimSpace(clientName)
	if trimmed == "" {
		return nil
	}
	if len(strings.Fields(trimmed)) > 5 {
		return nil
	}
	return []string{trimmed}
}

func formatPublicationName(raw string) string {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "UNKNOWN"
	}

	// If it looks like a domain name (no spaces, contains dots), strip the TLD.
	if !strings.Contains(name, " ") && strings.Contains(name, ".") {
		name = strings.TrimPrefix(name, "www.")
		// Try multi-level TLDs first, then single-level.
		for _, suffix := range []string{".co.uk", ".com.au", ".co.nz", ".com", ".net", ".org", ".io", ".ca", ".co", ".uk", ".us", ".tv", ".info", ".me", ".biz"} {
			if strings.HasSuffix(strings.ToLower(name), suffix) {
				name = name[:len(name)-len(suffix)]
				break
			}
		}
	}

	return strings.ToUpper(name)
}

var socialDomains = map[string]bool{
	"instagram.com": true,
	"tiktok.com":    true,
	"x.com":         true,
	"twitter.com":   true,
	"facebook.com":  true,
	"threads.net":   true,
	"snapchat.com":  true,
	"pinterest.com": true,
	"linkedin.com":  true,
	"youtube.com":   true,
	"youtu.be":      true,
	"bsky.app":      true,
	"tumblr.com":    true,
	"discord.com":   true,
	"discord.gg":    true,
	"telegram.me":   true,
	"t.me":          true,
	"weibo.com":     true,
}

var forumDomains = map[string]bool{
	"reddit.com":        true,
	"quora.com":         true,
	"stackoverflow.com": true,
}

var referenceDomains = map[string]bool{
	"wikipedia.org": true,
	"wikimedia.org": true,
}

var commerceDomains = map[string]bool{
	"amazon.com": true,
	"ebay.com":   true,
}

var searchEngineDomains = map[string]bool{
	"google.com": true,
	"bing.com":   true,
}

var aggregatorDomains = map[string]bool{
	"msn.com":       true,
	"aol.com":       true,
	"yahoo.com":     true,
	"newsbreak.com": true,
	"smartnews.com": true,
	"flipboard.com": true,
}

var excludedDomainSets = []map[string]bool{
	socialDomains,
	forumDomains,
	referenceDomains,
	commerceDomains,
	searchEngineDomains,
	aggregatorDomains,
}

func hostMatchesDomainSet(host string, domains map[string]bool) bool {
	if domains[host] {
		return true
	}
	for domain := range domains {
		if strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func isNonOutletURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimPrefix(u.Hostname(), "www."))

	for _, domainSet := range excludedDomainSets {
		if hostMatchesDomainSet(host, domainSet) {
			return true
		}
	}
	return false
}

func filterNonOutletClips(items []clip) []clip {
	filtered := make([]clip, 0, len(items))
	for _, c := range items {
		if isNonOutletURL(c.Link) {
			continue
		}
		filtered = append(filtered, c)
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

func renderResultsFragment(results []clip, query string, errs []string, stats []providerResult, diag searchDiagnostics) string {
	var b strings.Builder

	if len(results) == 0 {
		b.WriteString(renderDiagnosticsFragment(errs, stats, diag))
		b.WriteString(fmt.Sprintf(`<p class="empty-state">No clips found for <strong>%s</strong> in the past 24 hours.</p>`, html.EscapeString(query)))
		return b.String()
	}

	b.WriteString(renderDiagnosticsFragment(errs, stats, diag))
	b.WriteString(fmt.Sprintf(`<p class="count" data-results-count>%d unique result(s)</p>`, len(results)))
	b.WriteString(`<div class="email-preview-shell" data-results-preview><div class="email-preview" data-copy-root="press-clips" style="font-family:'Helvetica Neue', Helvetica, Arial, sans-serif; font-size:10pt; line-height:1.45; color:#000000;"><table role="presentation" cellpadding="0" cellspacing="0" border="0" style="width:100%; border-collapse:collapse; border-spacing:0; font-family:'Helvetica Neue', Helvetica, Arial, sans-serif; font-size:10pt; line-height:1.45; color:#000000; background-color:transparent;"><tr><td style="padding:0 0 12px 0; font-family:'Helvetica Neue', Helvetica, Arial, sans-serif; font-size:10pt; line-height:1.45; mso-line-height-rule:exactly; font-weight:400; color:#000000;">for your files and information, below please find the following press breaks</td></tr><tr><td style="padding:0 0 12px 0; font-family:'Helvetica Neue', Helvetica, Arial, sans-serif; font-size:10pt; line-height:1.45; mso-line-height-rule:exactly; font-weight:400; color:#000000;">[ONLINE]</td></tr>`)
	for _, row := range results {
		published := "Unknown date"
		if row.PublishedAt != nil {
			published = row.PublishedAt.Local().Format("January 2, 2006")
		}

		pub := formatPublicationName(row.Publication)

		b.WriteString(`<tr data-clip-row><td style="padding:0 0 12px 0; font-family:'Helvetica Neue', Helvetica, Arial, sans-serif; font-size:10pt; line-height:1.45; mso-line-height-rule:exactly; color:#000000;">`)
		b.WriteString(`<div class="clip-row-card">`)
		b.WriteString(`<button class="clip-remove-button" type="button" data-delete-clip data-copy-strip aria-label="Remove this result from the list" title="Remove this result"><span aria-hidden="true">&times;</span></button>`)
		b.WriteString(`<div style="font-family:'Helvetica Neue', Helvetica, Arial, sans-serif; font-size:10pt; line-height:1.45; mso-line-height-rule:exactly; color:#000000;">`)
		b.WriteString(html.EscapeString(pub))
		b.WriteString(` <span style="font-weight:400;">(`)
		b.WriteString(html.EscapeString(published))
		b.WriteString(`)</span></div>`)
		b.WriteString(`<div style="padding-top:2px; font-family:'Helvetica Neue', Helvetica, Arial, sans-serif; font-size:10pt; line-height:1.45; mso-line-height-rule:exactly; font-weight:400; color:#000000;">`)
		b.WriteString(html.EscapeString(cleanTitle(row.Title)))
		b.WriteString(`</div>`)
		b.WriteString(`<div style="padding-top:2px; font-family:'Helvetica Neue', Helvetica, Arial, sans-serif; font-size:10pt; line-height:1.45; mso-line-height-rule:exactly; color:#6fa8dc;"><a href="`)
		b.WriteString(html.EscapeString(row.Link))
		b.WriteString(`" target="_blank" rel="noopener noreferrer" style="font-family:'Helvetica Neue', Helvetica, Arial, sans-serif; font-size:10pt; line-height:1.45; mso-line-height-rule:exactly; color:#6fa8dc; text-decoration:underline;">`)
		b.WriteString(html.EscapeString(row.Link))
		b.WriteString(`</a></div></div></td></tr>`)
	}
	b.WriteString(`</table></div></div>`)

	return b.String()
}

func renderDiagnosticsFragment(errs []string, stats []providerResult, diag searchDiagnostics) string {
	var b strings.Builder

	b.WriteString(`<div id="diagnostics-body" class="diagnostics-body" hx-swap-oob="innerHTML">`)
	if len(errs) > 0 {
		b.WriteString(`<p class="warning">Partial results:</p><ul class="diag">`)
		for _, errMsg := range errs {
			b.WriteString(`<li>`)
			b.WriteString(html.EscapeString(errMsg))
			b.WriteString(`</li>`)
		}
		b.WriteString(`</ul>`)
	}

	b.WriteString(`<p class="count">Overall widdling:</p><ul class="diag">`)
	overallLine := fmt.Sprintf("All providers -> raw: %d -> dated within 24h: %d -> normalized: %d -> unique URLs: %d -> name match: %d -> official outlets: %d -> English/final: %d", diag.RawCount, diag.RecentCount, diag.Normalized, diag.UniqueURLs, diag.QueryMatched, diag.OutletAllowed, diag.EnglishKept)
	b.WriteString(`<li>`)
	b.WriteString(html.EscapeString(overallLine))
	b.WriteString(`</li></ul>`)

	b.WriteString(`<p class="count">Latest provider run:</p><ul class="diag">`)
	for _, s := range stats {
		line := fmt.Sprintf("%s -> raw: %d -> dated within 24h: %d -> normalized: %d, latency: %dms", s.Name, s.RawCount, s.RecentCount, len(s.Clips), s.DurationMS)
		if s.Err != nil {
			line = fmt.Sprintf("%s -> error: %s", s.Name, s.Err.Error())
		}
		b.WriteString(`<li>`)
		b.WriteString(html.EscapeString(line))
		b.WriteString(`</li>`)
	}
	b.WriteString(`</ul></div>`)

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
	q.Set("q", buildBraveBodyAwareQuery(clientName))
	q.Set("freshness", "pd")
	q.Set("search_lang", "en")
	q.Set("count", "50")
	q.Set("extra_snippets", "true")
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
			Title       string   `json:"title"`
			URL         string   `json:"url"`
			Description string   `json:"description"`
			ExtraSnips  []string `json:"extra_snippets"`
			PageAge     string   `json:"page_age"`
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
			Snippet:     bestSnippet(item.Description, strings.Join(item.ExtraSnips, " ")),
			SearchText:  joinTextParts(item.Description, strings.Join(item.ExtraSnips, "\n")),
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
		"query":              strings.TrimSpace(clientName),
		"type":               "auto",
		"category":           "news",
		"numResults":         50,
		"startPublishedDate": since.Format(time.RFC3339),
		"contents": map[string]any{
			"highlights": map[string]any{
				"maxCharacters": 4000,
			},
		},
	}
	if includeText := exaIncludeText(clientName); len(includeText) > 0 {
		payload["includeText"] = includeText
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
			Title         string   `json:"title"`
			URL           string   `json:"url"`
			Text          string   `json:"text"`
			Highlights    []string `json:"highlights"`
			PublishedDate string   `json:"publishedDate"`
			Author        string   `json:"author"`
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
			Snippet:     bestSnippet(firstNonEmpty(item.Highlights...), item.Text),
			SearchText:  joinTextParts(item.Text, strings.Join(item.Highlights, "\n")),
			Source:      "Exa",
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
		log.Printf("provider=%s status=ok latency_ms=%d raw=%d dated_within_24h=%d normalized=%d", res.Name, res.DurationMS, res.RawCount, res.RecentCount, len(res.Clips))
	}
	return res
}

func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func fallback(value, defaultValue string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return defaultValue
	}
	return trimmed
}
