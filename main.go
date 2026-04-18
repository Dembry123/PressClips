package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdhtml "html"
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
	"unicode"

	xhtml "golang.org/x/net/html"
	"golang.org/x/net/publicsuffix"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
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

type searchWindow struct {
	Value          string
	Label          string
	Duration       time.Duration
	BraveFreshness string
}

type publicationLookupTarget struct {
	Key  string
	Link string
}

var publicationTitleCaser = cases.Title(language.English)

var publicationMetadataHTTPClient = &http.Client{
	Timeout: 2500 * time.Millisecond,
}

var publicationNameOverrides = map[string]string{
	"abcnews":        "ABC News",
	"apnews":         "AP News",
	"cbsnews":        "CBS News",
	"dailymail":      "Daily Mail",
	"eastbaytimes":   "East Bay Times",
	"eonline":        "E! News",
	"foxnews":        "Fox News",
	"latimes":        "Los Angeles Times",
	"nbcnews":        "NBC News",
	"nypost":         "New York Post",
	"nytimes":        "The New York Times",
	"ok":             "OK!",
	"okmagazine":     "OK! Magazine",
	"tmz":            "TMZ",
	"usatoday":       "USA Today",
	"washingtonpost": "The Washington Post",
	"wsj":            "The Wall Street Journal",
}

var outletLexicon = []string{
	"associated",
	"angeles",
	"business",
	"chronicle",
	"daily",
	"dispatch",
	"east",
	"entertainment",
	"evening",
	"examiner",
	"express",
	"financial",
	"gazette",
	"globe",
	"herald",
	"hollywood",
	"independent",
	"insider",
	"journal",
	"leader",
	"london",
	"los",
	"magazine",
	"mail",
	"mirror",
	"morning",
	"national",
	"news",
	"observer",
	"online",
	"people",
	"post",
	"press",
	"record",
	"register",
	"reporter",
	"review",
	"standard",
	"star",
	"sun",
	"telegraph",
	"times",
	"today",
	"tribune",
	"usa",
	"variety",
	"weekly",
	"west",
	"wire",
	"world",
	"york",
	"bay",
	"new",
}

var shortPublicationAcronyms = map[string]bool{
	"ABC": true,
	"AP":  true,
	"BBC": true,
	"CBS": true,
	"CNN": true,
	"E":   true,
	"LA":  true,
	"NBC": true,
	"NY":  true,
	"OK":  true,
	"TMZ": true,
	"TV":  true,
	"UK":  true,
	"UPI": true,
	"USA": true,
	"WSJ": true,
}

var publicationSmallWords = map[string]bool{
	"and": true,
	"for": true,
	"in":  true,
	"of":  true,
	"on":  true,
	"the": true,
	"to":  true,
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

	window, err := parseSearchWindow(r.FormValue("searchWindow"))
	if err != nil {
		writeHTML(w, http.StatusBadRequest, "<p>Please choose a valid time range.</p>")
		return
	}

	braveKey := strings.TrimSpace(os.Getenv("BRAVE_API_KEY"))
	exaKey := strings.TrimSpace(os.Getenv("EXA_API_KEY"))
	log.Printf("request env BRAVE_API_KEY=%s EXA_API_KEY=%s", maskedValue(braveKey), maskedValue(exaKey))

	if braveKey == "" || exaKey == "" {
		writeHTML(w, http.StatusInternalServerError, "<p>Missing API keys. Set BRAVE_API_KEY and EXA_API_KEY in your environment.</p>")
		return
	}

	since := time.Now().UTC().Add(-window.Duration)
	searchID := fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	logSearchEvent(searchID, "trace_start", map[string]any{
		"query":  query,
		"window": window.Value,
		"since":  since.Format(time.RFC3339),
	})

	type call struct {
		name string
		fn   func(context.Context) providerResult
	}

	calls := []call{
		{name: "Brave", fn: func(ctx context.Context) providerResult {
			return searchBrave(ctx, searchID, query, since, window, braveKey)
		}},
		{name: "Exa", fn: func(ctx context.Context) providerResult { return searchExa(ctx, searchID, query, since, exaKey) }},
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
	logProviderRunSummary(searchID, stats, errors)
	logClipStage(searchID, "provider-normalized clips", merged)

	unique := dedupeAndSort(merged)
	diag.UniqueURLs = len(unique)
	logClipStage(searchID, "after dedupeAndSort", unique)
	unique = filterClipsByQuery(unique, query)
	diag.QueryMatched = len(unique)
	logClipStage(searchID, "after filterClipsByQuery", unique)
	unique = filterNonOutletClips(unique)
	diag.OutletAllowed = len(unique)
	logClipStage(searchID, "after filterNonOutletClips", unique)
	unique = filterNonEnglish(unique)
	diag.EnglishKept = len(unique)
	logClipStage(searchID, "after filterNonEnglish", unique)
	publicationCtx, publicationCancel := context.WithTimeout(r.Context(), 6*time.Second)
	unique = resolveClipPublications(publicationCtx, unique)
	publicationCancel()
	logClipStage(searchID, "after resolveClipPublications", unique)
	logRenderedResults(searchID, unique)
	logSearchEvent(searchID, "trace_complete", map[string]any{
		"provider_count":       len(stats),
		"raw_count":            diag.RawCount,
		"recent_count":         diag.RecentCount,
		"normalized_count":     diag.Normalized,
		"unique_url_count":     diag.UniqueURLs,
		"query_matched_count":  diag.QueryMatched,
		"outlet_allowed_count": diag.OutletAllowed,
		"english_kept_count":   diag.EnglishKept,
		"rendered_count":       len(unique),
	})

	fragment := renderResultsFragment(unique, query, window, errors, stats, diag)
	writeHTML(w, http.StatusOK, fragment)
}

func writeHTML(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func logSearchEvent(searchID, event string, fields map[string]any) {
	entry := map[string]any{
		"kind":      "search_trace",
		"search_id": searchID,
		"event":     event,
	}
	for key, value := range fields {
		if value == nil {
			continue
		}
		entry[key] = sanitizeLogValue(value)
	}

	encoded, err := json.Marshal(entry)
	if err != nil {
		log.Printf(`{"kind":"search_trace","search_id":%q,"event":"log_encoding_error","error":%q}`, searchID, err.Error())
		return
	}
	log.Print(string(encoded))
}

func sanitizeLogValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		sanitized := make(map[string]any, len(typed))
		for key, child := range typed {
			sanitized[key] = sanitizeLogValue(child)
		}
		return sanitized
	case []any:
		sanitized := make([]any, 0, len(typed))
		for _, child := range typed {
			sanitized = append(sanitized, sanitizeLogValue(child))
		}
		return sanitized
	case string:
		return truncateLogString(typed, 800)
	default:
		return value
	}
}

func truncateLogString(raw string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(raw)
	if len(runes) <= max {
		return raw
	}
	return string(runes[:max]) + fmt.Sprintf("... [truncated %d chars]", len(runes)-max)
}

func decodeJSONLogPayload(raw []byte) any {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "<empty>"
	}

	var payload any
	if err := json.Unmarshal(raw, &payload); err == nil {
		return payload
	}

	return string(raw)
}

func sortedMapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func logProviderRequest(searchID, provider string, payload map[string]any) {
	logSearchEvent(searchID, "provider_request", map[string]any{
		"provider": provider,
		"payload":  payload,
	})
}

func logProviderRawResponse(searchID, provider string, statusCode int, raw []byte) {
	payload := decodeJSONLogPayload(raw)
	switch typed := payload.(type) {
	case map[string]any:
		summary := map[string]any{
			"provider":       provider,
			"status_code":    statusCode,
			"top_level_keys": sortedMapKeys(typed),
		}
		if results, ok := typed["results"].([]any); ok {
			summary["result_count"] = len(results)

			meta := make(map[string]any, len(typed))
			for key, value := range typed {
				if key == "results" {
					continue
				}
				meta[key] = value
			}
			if len(meta) > 0 {
				summary["payload"] = meta
			}

			logSearchEvent(searchID, "provider_raw_response_summary", summary)
			for index, item := range results {
				logSearchEvent(searchID, "provider_raw_response_item", map[string]any{
					"provider":    provider,
					"status_code": statusCode,
					"item_index":  index,
					"payload":     item,
				})
			}
			return
		}

		logSearchEvent(searchID, "provider_raw_response", map[string]any{
			"provider":    provider,
			"status_code": statusCode,
			"payload":     typed,
		})
	case []any:
		logSearchEvent(searchID, "provider_raw_response_summary", map[string]any{
			"provider":     provider,
			"status_code":  statusCode,
			"result_count": len(typed),
		})
		for index, item := range typed {
			logSearchEvent(searchID, "provider_raw_response_item", map[string]any{
				"provider":    provider,
				"status_code": statusCode,
				"item_index":  index,
				"payload":     item,
			})
		}
	default:
		logSearchEvent(searchID, "provider_raw_response", map[string]any{
			"provider":    provider,
			"status_code": statusCode,
			"payload":     payload,
		})
	}
}

func logProviderNormalized(searchID, provider string, rawCount, recentCount int, clips []clip) {
	logSearchEvent(searchID, "provider_normalized_summary", map[string]any{
		"provider":         provider,
		"raw_count":        rawCount,
		"recent_count":     recentCount,
		"normalized_count": len(clips),
	})

	for index, entry := range clipLogEntries(clips) {
		logSearchEvent(searchID, "provider_normalized_item", map[string]any{
			"provider":   provider,
			"item_index": index,
			"payload":    entry,
		})
	}
}

func clipLogEntries(clips []clip) []map[string]any {
	entries := make([]map[string]any, 0, len(clips))
	for _, c := range clips {
		entry := map[string]any{
			"source":      c.Source,
			"publication": c.Publication,
			"title":       cleanTitle(c.Title),
			"link":        c.Link,
		}
		if c.PublishedAt != nil {
			entry["publishedAt"] = c.PublishedAt.UTC().Format(time.RFC3339)
		}
		if snippet := previewText(c.Snippet, 240); snippet != "" {
			entry["snippetPreview"] = snippet
		}
		entries = append(entries, entry)
	}
	return entries
}

func renderedResultLogEntries(clips []clip) []map[string]any {
	entries := make([]map[string]any, 0, len(clips))
	for _, c := range clips {
		published := "Unknown date"
		if c.PublishedAt != nil {
			published = c.PublishedAt.Local().Format("January 2, 2006")
		}
		entries = append(entries, map[string]any{
			"publication": formatPublicationName(c.Publication, c.Link),
			"published":   published,
			"title":       cleanTitle(c.Title),
			"link":        c.Link,
		})
	}
	return entries
}

func previewText(raw string, max int) string {
	raw = strings.Join(strings.Fields(strings.TrimSpace(raw)), " ")
	if raw == "" || max <= 0 {
		return ""
	}
	if len(raw) <= max {
		return raw
	}
	return raw[:max] + "..."
}

func logClipStage(searchID, stage string, clips []clip) {
	logSearchEvent(searchID, "clip_stage_summary", map[string]any{
		"stage": stage,
		"count": len(clips),
	})
	for index, entry := range clipLogEntries(clips) {
		logSearchEvent(searchID, "clip_stage_item", map[string]any{
			"stage":      stage,
			"item_index": index,
			"payload":    entry,
		})
	}
}

func logRenderedResults(searchID string, clips []clip) {
	logSearchEvent(searchID, "rendered_results_summary", map[string]any{
		"count": len(clips),
	})
	for index, entry := range renderedResultLogEntries(clips) {
		logSearchEvent(searchID, "rendered_results_item", map[string]any{
			"item_index": index,
			"payload":    entry,
		})
	}
}

func logProviderRunSummary(searchID string, stats []providerResult, errs []string) {
	logSearchEvent(searchID, "provider_run_summary", map[string]any{
		"provider_count": len(stats),
		"error_count":    len(errs),
		"errors":         errs,
	})
	for _, stat := range stats {
		fields := map[string]any{
			"provider":         stat.Name,
			"duration_ms":      stat.DurationMS,
			"raw_count":        stat.RawCount,
			"recent_count":     stat.RecentCount,
			"normalized_count": len(stat.Clips),
			"status":           "ok",
		}
		if stat.Err != nil {
			fields["status"] = "error"
			fields["error"] = stat.Err.Error()
		}
		logSearchEvent(searchID, "provider_run_item", fields)
	}
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

func isWithinWindow(ts *time.Time, since time.Time) bool {
	if ts == nil {
		return false
	}
	return ts.After(since)
}

func parseSearchWindow(raw string) (searchWindow, error) {
	switch strings.TrimSpace(raw) {
	case "", "1d":
		return searchWindow{
			Value:          "1d",
			Label:          "past day",
			Duration:       24 * time.Hour,
			BraveFreshness: "pd",
		}, nil
	case "3d":
		return searchWindow{
			Value:    "3d",
			Label:    "past 3 days",
			Duration: 72 * time.Hour,
			// Brave offers day/week freshness buckets, so we widen to a week
			// here and then keep the exact 72-hour cutoff with local filtering.
			BraveFreshness: "pw",
		}, nil
	default:
		return searchWindow{}, fmt.Errorf("unsupported search window %q", raw)
	}
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

func resolveClipPublications(ctx context.Context, items []clip) []clip {
	if len(items) == 0 {
		return items
	}

	resolved := append([]clip(nil), items...)
	lookupTargets := make(map[string]publicationLookupTarget)

	for i := range resolved {
		rawPublication := firstNonEmpty(resolved[i].Publication, domainFromURL(resolved[i].Link))
		resolved[i].Publication = formatPublicationName(rawPublication, resolved[i].Link)

		if !shouldLookupPublicationMetadata(rawPublication, resolved[i].Publication, resolved[i].Link) {
			continue
		}

		target := buildPublicationLookupTarget(rawPublication, resolved[i].Link)
		if target.Key == "" || target.Link == "" {
			continue
		}
		if _, found := lookupTargets[target.Key]; !found {
			lookupTargets[target.Key] = target
		}
	}

	metadataNames := fetchPublicationMetadataNames(ctx, lookupTargets)
	if len(metadataNames) == 0 {
		return resolved
	}

	for i := range resolved {
		target := buildPublicationLookupTarget(items[i].Publication, resolved[i].Link)
		if target.Key == "" {
			continue
		}
		if metadataName := metadataNames[target.Key]; metadataName != "" {
			resolved[i].Publication = metadataName
		}
	}

	return resolved
}

func formatPublicationName(raw, link string) string {
	name := strings.TrimSpace(stdhtml.UnescapeString(raw))
	if name == "" {
		name = domainFromURL(link)
	}
	if name == "" {
		return "Unknown Publication"
	}

	if host := publicationHostFromValue(name); host != "" {
		if normalized := normalizePublicationHost(host); normalized != "" {
			return normalized
		}
	}

	if override := publicationOverride(name); override != "" {
		return override
	}

	name = strings.NewReplacer("_", " ", "-", " ").Replace(name)
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "Unknown Publication"
	}

	if segmented := segmentPublicationToken(name); segmented != "" {
		return segmented
	}

	return titleCasePublicationPhrase(name)
}

func publicationOverride(values ...string) string {
	for _, value := range values {
		slug := publicationSlug(value)
		if slug == "" {
			continue
		}
		if override := publicationNameOverrides[slug]; override != "" {
			return override
		}
	}
	return ""
}

func publicationSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func publicationHostFromValue(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" || strings.Contains(raw, " ") {
		return ""
	}

	if strings.Contains(raw, "://") {
		if parsed, err := url.Parse(raw); err == nil {
			return strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
		}
	}

	if strings.Contains(raw, "/") {
		if parsed, err := url.Parse("https://" + raw); err == nil {
			return strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
		}
	}

	if strings.Contains(raw, ".") {
		return strings.TrimPrefix(strings.ToLower(raw), "www.")
	}

	return ""
}

func effectivePublicationDomain(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	host = strings.TrimPrefix(host, "www.")
	if host == "" {
		return ""
	}

	if domain, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return domain
	}
	return host
}

func publicationDomainLabel(host string) string {
	domain := effectivePublicationDomain(host)
	if domain == "" {
		return ""
	}
	parts := strings.Split(domain, ".")
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

func normalizePublicationHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	host = strings.TrimPrefix(host, "www.")
	if host == "" {
		return ""
	}

	if override := publicationOverride(host, effectivePublicationDomain(host), publicationDomainLabel(host)); override != "" {
		return override
	}

	label := publicationDomainLabel(host)
	if label == "" {
		return ""
	}

	if segmented := segmentPublicationToken(label); segmented != "" {
		return segmented
	}

	return titleCasePublicationPhrase(label)
}

func titleCasePublicationPhrase(raw string) string {
	raw = strings.Join(strings.Fields(strings.TrimSpace(raw)), " ")
	if raw == "" {
		return ""
	}

	parts := strings.Fields(raw)
	for i, part := range parts {
		parts[i] = normalizePublicationWord(part, i, len(parts))
	}
	return strings.Join(parts, " ")
}

func normalizePublicationWord(word string, index, total int) string {
	runes := []rune(word)
	start := 0
	end := len(runes)
	for start < end && !unicode.IsLetter(runes[start]) && !unicode.IsDigit(runes[start]) {
		start++
	}
	for end > start && !unicode.IsLetter(runes[end-1]) && !unicode.IsDigit(runes[end-1]) {
		end--
	}
	if start >= end {
		return word
	}

	prefix := string(runes[:start])
	core := string(runes[start:end])
	suffix := string(runes[end:])
	upperCore := strings.ToUpper(core)
	lowerCore := strings.ToLower(core)

	switch {
	case shortPublicationAcronyms[upperCore]:
		core = upperCore
	case publicationSmallWords[lowerCore] && index > 0 && index < total-1:
		core = lowerCore
	default:
		core = publicationTitleCaser.String(lowerCore)
	}

	return prefix + core + suffix
}

func segmentPublicationToken(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return ""
	}

	if override := publicationOverride(raw); override != "" {
		return override
	}

	if strings.ContainsAny(raw, " _-") {
		cleaned := strings.NewReplacer("_", " ", "-", " ").Replace(raw)
		return titleCasePublicationPhrase(cleaned)
	}

	parts := segmentLexiconToken(raw)
	if len(parts) >= 2 {
		return titleCasePublicationPhrase(strings.Join(parts, " "))
	}

	return ""
}

func segmentLexiconToken(token string) []string {
	type segmentation struct {
		parts []string
		score int
	}

	token = strings.TrimSpace(strings.ToLower(token))
	if token == "" {
		return nil
	}

	best := make([]segmentation, len(token)+1)
	reachable := make([]bool, len(token)+1)
	reachable[0] = true

	for i := 0; i < len(token); i++ {
		if !reachable[i] {
			continue
		}
		for _, word := range outletLexicon {
			if !strings.HasPrefix(token[i:], word) {
				continue
			}
			next := i + len(word)
			candidate := segmentation{
				parts: append(append([]string(nil), best[i].parts...), word),
				score: best[i].score + len(word)*len(word) + 1,
			}
			if !reachable[next] || candidate.score > best[next].score {
				best[next] = candidate
				reachable[next] = true
			}
		}
	}

	if !reachable[len(token)] || len(best[len(token)].parts) < 2 {
		return nil
	}

	return best[len(token)].parts
}

func shouldLookupPublicationMetadata(raw, resolved, link string) bool {
	if strings.TrimSpace(link) == "" {
		return false
	}

	raw = strings.TrimSpace(raw)
	resolved = strings.TrimSpace(resolved)
	if raw == "" || resolved == "" || strings.EqualFold(resolved, "Unknown Publication") {
		return true
	}

	if publicationHostFromValue(raw) != "" {
		return true
	}

	if !strings.Contains(resolved, " ") && !shortPublicationAcronyms[strings.ToUpper(resolved)] && len(resolved) > 4 {
		return true
	}

	return false
}

func buildPublicationLookupTarget(raw, link string) publicationLookupTarget {
	host := publicationHostFromValue(raw)
	if host == "" {
		if parsed, err := url.Parse(strings.TrimSpace(link)); err == nil {
			host = parsed.Hostname()
		}
	}
	host = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(host)), "www.")
	if host == "" {
		return publicationLookupTarget{}
	}

	key := effectivePublicationDomain(host)
	if key == "" {
		key = host
	}

	return publicationLookupTarget{
		Key:  key,
		Link: strings.TrimSpace(link),
	}
}

func fetchPublicationMetadataNames(ctx context.Context, lookups map[string]publicationLookupTarget) map[string]string {
	if len(lookups) == 0 || ctx.Err() != nil {
		return nil
	}

	results := make(map[string]string, len(lookups))
	var mu sync.Mutex
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 4)

	for _, target := range lookups {
		wg.Add(1)
		go func(target publicationLookupTarget) {
			defer wg.Done()

			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-semaphore }()

			lookupCtx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
			defer cancel()

			name, err := fetchPublicationNameFromArticle(lookupCtx, target.Link)
			if err != nil || name == "" {
				return
			}

			mu.Lock()
			results[target.Key] = name
			mu.Unlock()
		}(target)
	}

	wg.Wait()
	return results
}

func fetchPublicationNameFromArticle(ctx context.Context, articleURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, articleURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("User-Agent", "PressClips/1.0")

	resp, err := publicationMetadataHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if contentType != "" && !strings.Contains(contentType, "html") {
		return "", fmt.Errorf("content type %q", contentType)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return "", err
	}

	return extractPublicationNameFromHTML(body), nil
}

func extractPublicationNameFromHTML(body []byte) string {
	doc, err := xhtml.Parse(bytes.NewReader(body))
	if err != nil {
		return ""
	}

	candidates := map[string]string{}
	var walk func(*xhtml.Node)
	walk = func(node *xhtml.Node) {
		if node.Type == xhtml.ElementNode {
			switch node.Data {
			case "meta":
				source, value := publicationNameFromMetaNode(node)
				if source != "" && value != "" && candidates[source] == "" {
					candidates[source] = value
				}
			case "script":
				if candidates["jsonld"] == "" && isJSONLDNode(node) {
					if value := extractPublicationNameFromJSONLD(nodeText(node)); value != "" {
						candidates["jsonld"] = value
					}
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)

	for _, key := range []string{"og", "application", "publisher", "jsonld"} {
		if normalized := formatPublicationName(candidates[key], ""); normalized != "" && !strings.EqualFold(normalized, "Unknown Publication") {
			return normalized
		}
	}
	return ""
}

func publicationNameFromMetaNode(node *xhtml.Node) (string, string) {
	property := strings.ToLower(strings.TrimSpace(nodeAttr(node, "property")))
	name := strings.ToLower(strings.TrimSpace(nodeAttr(node, "name")))
	itemProp := strings.ToLower(strings.TrimSpace(nodeAttr(node, "itemprop")))
	content := strings.TrimSpace(nodeAttr(node, "content"))
	if content == "" {
		return "", ""
	}

	switch {
	case property == "og:site_name":
		return "og", content
	case name == "application-name":
		return "application", content
	case name == "publisher", itemProp == "publisher":
		return "publisher", content
	default:
		return "", ""
	}
}

func isJSONLDNode(node *xhtml.Node) bool {
	return strings.Contains(strings.ToLower(nodeAttr(node, "type")), "ld+json")
}

func nodeText(node *xhtml.Node) string {
	var b strings.Builder
	var walk func(*xhtml.Node)
	walk = func(current *xhtml.Node) {
		if current.Type == xhtml.TextNode {
			b.WriteString(current.Data)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return b.String()
}

func nodeAttr(node *xhtml.Node, key string) string {
	for _, attr := range node.Attr {
		if strings.EqualFold(attr.Key, key) {
			return attr.Val
		}
	}
	return ""
}

func extractPublicationNameFromJSONLD(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return ""
	}

	return findPublicationNameInJSONLD(value)
}

func findPublicationNameInJSONLD(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		if name := extractNameFromJSONLDField(typed["publisher"]); name != "" {
			return name
		}
		if name := extractNameFromJSONLDField(typed["isPartOf"]); name != "" {
			return name
		}
		if jsonLDObjectIsPublicationContainer(typed) {
			if name := jsonLDStringField(typed["name"]); name != "" {
				return name
			}
		}
		if name := findPublicationNameInJSONLD(typed["@graph"]); name != "" {
			return name
		}
		for _, child := range typed {
			if name := findPublicationNameInJSONLD(child); name != "" {
				return name
			}
		}
	case []any:
		for _, item := range typed {
			if name := findPublicationNameInJSONLD(item); name != "" {
				return name
			}
		}
	}
	return ""
}

func extractNameFromJSONLDField(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		if name := jsonLDStringField(typed["name"]); name != "" {
			return name
		}
		return findPublicationNameInJSONLD(typed)
	case []any:
		for _, item := range typed {
			if name := extractNameFromJSONLDField(item); name != "" {
				return name
			}
		}
	}
	return ""
}

func jsonLDObjectIsPublicationContainer(obj map[string]any) bool {
	for _, objectType := range jsonLDTypes(obj["@type"]) {
		switch objectType {
		case "website", "organization", "newsmediaorganization", "newspaper", "publication", "periodical":
			return true
		}
	}
	return false
}

func jsonLDTypes(value any) []string {
	switch typed := value.(type) {
	case string:
		return []string{strings.ToLower(strings.TrimSpace(typed))}
	case []any:
		types := make([]string, 0, len(typed))
		for _, item := range typed {
			if str, ok := item.(string); ok {
				types = append(types, strings.ToLower(strings.TrimSpace(str)))
			}
		}
		return types
	default:
		return nil
	}
}

func jsonLDStringField(value any) string {
	if str, ok := value.(string); ok {
		return strings.TrimSpace(str)
	}
	return ""
}

var socialDomains = map[string]bool{
	"instagram.com": true,
	"tiktok.com":    true,
	"x.com":         true,
	"twitter.com":   true,
	"facebook.com":  true,
	"threads.com":   true,
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

func renderResultsFragment(results []clip, query string, window searchWindow, errs []string, stats []providerResult, diag searchDiagnostics) string {
	var b strings.Builder

	if len(results) == 0 {
		b.WriteString(renderDiagnosticsFragment(errs, stats, diag, window))
		b.WriteString(fmt.Sprintf(`<p class="empty-state">No clips found for <strong>%s</strong> in the %s.</p>`, stdhtml.EscapeString(query), stdhtml.EscapeString(window.Label)))
		return b.String()
	}

	b.WriteString(renderDiagnosticsFragment(errs, stats, diag, window))
	b.WriteString(fmt.Sprintf(`<p class="count" data-results-count>%d unique result(s)</p>`, len(results)))
	b.WriteString(`<div class="email-preview-shell" data-results-preview><div class="email-preview" data-copy-root="press-clips" style="font-family:'Helvetica Neue', Helvetica, Arial, sans-serif; font-size:10pt; line-height:1.45; color:#000000;"><table role="presentation" cellpadding="0" cellspacing="0" border="0" style="width:100%; border-collapse:collapse; border-spacing:0; font-family:'Helvetica Neue', Helvetica, Arial, sans-serif; font-size:10pt; line-height:1.45; color:#000000; background-color:transparent;"><tr><td style="padding:0 0 12px 0; font-family:'Helvetica Neue', Helvetica, Arial, sans-serif; font-size:10pt; line-height:1.45; mso-line-height-rule:exactly; font-weight:400; color:#000000;">for your files and information, below please find the following press breaks</td></tr><tr><td style="padding:0 0 12px 0; font-family:'Helvetica Neue', Helvetica, Arial, sans-serif; font-size:10pt; line-height:1.45; mso-line-height-rule:exactly; font-weight:400; color:#000000;">[ONLINE]</td></tr>`)
	for _, row := range results {
		published := "Unknown date"
		if row.PublishedAt != nil {
			published = row.PublishedAt.Local().Format("January 2, 2006")
		}

		pub := formatPublicationName(row.Publication, row.Link)

		b.WriteString(`<tr data-clip-row><td style="padding:0 0 12px 0; font-family:'Helvetica Neue', Helvetica, Arial, sans-serif; font-size:10pt; line-height:1.45; mso-line-height-rule:exactly; color:#000000;">`)
		b.WriteString(`<div class="clip-row-card">`)
		b.WriteString(`<button class="clip-remove-button" type="button" data-delete-clip data-copy-strip aria-label="Remove this result from the list" title="Remove this result"><span aria-hidden="true">&times;</span></button>`)
		b.WriteString(`<div style="font-family:'Helvetica Neue', Helvetica, Arial, sans-serif; font-size:10pt; line-height:1.45; mso-line-height-rule:exactly; color:#000000;">`)
		b.WriteString(`<strong style="font-weight:700;">`)
		b.WriteString(stdhtml.EscapeString(pub))
		b.WriteString(`</strong>`)
		b.WriteString(` <span style="font-weight:400;">(`)
		b.WriteString(stdhtml.EscapeString(published))
		b.WriteString(`)</span></div>`)
		b.WriteString(`<div style="padding-top:2px; font-family:'Helvetica Neue', Helvetica, Arial, sans-serif; font-size:10pt; line-height:1.45; mso-line-height-rule:exactly; font-weight:400; color:#000000;">`)
		b.WriteString(stdhtml.EscapeString(cleanTitle(row.Title)))
		b.WriteString(`</div>`)
		b.WriteString(`<div style="padding-top:2px; font-family:'Helvetica Neue', Helvetica, Arial, sans-serif; font-size:10pt; line-height:1.45; mso-line-height-rule:exactly; color:#6fa8dc;"><a href="`)
		b.WriteString(stdhtml.EscapeString(row.Link))
		b.WriteString(`" target="_blank" rel="noopener noreferrer" style="font-family:'Helvetica Neue', Helvetica, Arial, sans-serif; font-size:10pt; line-height:1.45; mso-line-height-rule:exactly; color:#6fa8dc; text-decoration:underline;">`)
		b.WriteString(stdhtml.EscapeString(row.Link))
		b.WriteString(`</a></div></div></td></tr>`)
	}
	b.WriteString(`</table></div></div>`)

	return b.String()
}

func renderDiagnosticsFragment(errs []string, stats []providerResult, diag searchDiagnostics, window searchWindow) string {
	var b strings.Builder

	b.WriteString(`<div id="diagnostics-body" class="diagnostics-body" hx-swap-oob="innerHTML">`)
	if len(errs) > 0 {
		b.WriteString(`<p class="warning">Partial results:</p><ul class="diag">`)
		for _, errMsg := range errs {
			b.WriteString(`<li>`)
			b.WriteString(stdhtml.EscapeString(errMsg))
			b.WriteString(`</li>`)
		}
		b.WriteString(`</ul>`)
	}

	b.WriteString(`<p class="count">Overall widdling:</p><ul class="diag">`)
	overallLine := fmt.Sprintf("All providers -> raw: %d -> dated within %s: %d -> normalized: %d -> unique URLs: %d -> name match: %d -> official outlets: %d -> English/final: %d", diag.RawCount, window.Label, diag.RecentCount, diag.Normalized, diag.UniqueURLs, diag.QueryMatched, diag.OutletAllowed, diag.EnglishKept)
	b.WriteString(`<li>`)
	b.WriteString(stdhtml.EscapeString(overallLine))
	b.WriteString(`</li></ul>`)

	b.WriteString(`<p class="count">Latest provider run:</p><ul class="diag">`)
	for _, s := range stats {
		line := fmt.Sprintf("%s -> raw: %d -> dated within %s: %d -> normalized: %d, latency: %dms", s.Name, s.RawCount, window.Label, s.RecentCount, len(s.Clips), s.DurationMS)
		if s.Err != nil {
			line = fmt.Sprintf("%s -> error: %s", s.Name, s.Err.Error())
		}
		b.WriteString(`<li>`)
		b.WriteString(stdhtml.EscapeString(line))
		b.WriteString(`</li>`)
	}
	b.WriteString(`</ul></div>`)

	return b.String()
}

func searchBrave(ctx context.Context, searchID, clientName string, since time.Time, window searchWindow, apiKey string) providerResult {
	started := time.Now()
	res := providerResult{Name: "Brave"}

	u, err := url.Parse("https://api.search.brave.com/res/v1/news/search")
	if err != nil {
		res.Err = err
		return finalizeProviderResult(searchID, res, started)
	}

	q := u.Query()
	q.Set("q", buildBraveBodyAwareQuery(clientName))
	q.Set("freshness", window.BraveFreshness)
	q.Set("search_lang", "en")
	q.Set("count", "50")
	q.Set("extra_snippets", "true")
	u.RawQuery = q.Encode()
	requestPayload := map[string]any{
		"method": "GET",
		"url":    u.String(),
		"query": map[string]any{
			"q":              q.Get("q"),
			"freshness":      q.Get("freshness"),
			"search_lang":    q.Get("search_lang"),
			"count":          q.Get("count"),
			"extra_snippets": q.Get("extra_snippets"),
		},
	}
	logProviderRequest(searchID, "Brave", requestPayload)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		res.Err = err
		return finalizeProviderResult(searchID, res, started)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", apiKey)

	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		res.Err = err
		return finalizeProviderResult(searchID, res, started)
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, 2<<20))
	if err != nil {
		res.Err = err
		return finalizeProviderResult(searchID, res, started)
	}
	logProviderRawResponse(searchID, "Brave", httpResp.StatusCode, body)

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		res.Err = fmt.Errorf("api error (%d): %s", httpResp.StatusCode, strings.TrimSpace(string(body)))
		return finalizeProviderResult(searchID, res, started)
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
	if err := json.Unmarshal(body, &payload); err != nil {
		res.Err = err
		return finalizeProviderResult(searchID, res, started)
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
		if isWithinWindow(published, since) {
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
	logProviderNormalized(searchID, "Brave", res.RawCount, res.RecentCount, res.Clips)
	return finalizeProviderResult(searchID, res, started)
}

func searchExa(ctx context.Context, searchID, clientName string, since time.Time, apiKey string) providerResult {
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
	requestPayload := map[string]any{
		"method": "POST",
		"url":    "https://api.exa.ai/search",
		"body":   payload,
	}
	logProviderRequest(searchID, "Exa", requestPayload)

	body, err := json.Marshal(payload)
	if err != nil {
		res.Err = err
		return finalizeProviderResult(searchID, res, started)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.exa.ai/search", bytes.NewReader(body))
	if err != nil {
		res.Err = err
		return finalizeProviderResult(searchID, res, started)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)

	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		res.Err = err
		return finalizeProviderResult(searchID, res, started)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, 2<<20))
	if err != nil {
		res.Err = err
		return finalizeProviderResult(searchID, res, started)
	}
	logProviderRawResponse(searchID, "Exa", httpResp.StatusCode, respBody)

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		res.Err = fmt.Errorf("api error (%d): %s", httpResp.StatusCode, strings.TrimSpace(string(respBody)))
		return finalizeProviderResult(searchID, res, started)
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
	if err := json.Unmarshal(respBody, &response); err != nil {
		res.Err = err
		return finalizeProviderResult(searchID, res, started)
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
		if isWithinWindow(published, since) {
			recent++
		}

		clipped = append(clipped, clip{
			Publication: domainFromURL(link),
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
	logProviderNormalized(searchID, "Exa", res.RawCount, res.RecentCount, res.Clips)
	return finalizeProviderResult(searchID, res, started)
}

func finalizeProviderResult(searchID string, res providerResult, started time.Time) providerResult {
	res.DurationMS = time.Since(started).Milliseconds()
	if res.Err != nil {
		logSearchEvent(searchID, "provider_call_complete", map[string]any{
			"provider":     res.Name,
			"status":       "error",
			"latency_ms":   res.DurationMS,
			"error":        res.Err.Error(),
			"raw_count":    res.RawCount,
			"recent_count": res.RecentCount,
		})
	} else {
		logSearchEvent(searchID, "provider_call_complete", map[string]any{
			"provider":         res.Name,
			"status":           "ok",
			"latency_ms":       res.DurationMS,
			"raw_count":        res.RawCount,
			"recent_count":     res.RecentCount,
			"normalized_count": len(res.Clips),
		})
	}
	return res
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
