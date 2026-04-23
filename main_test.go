package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFormatPublicationName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		link string
		want string
	}{
		{
			name: "daily mail domain",
			raw:  "www.dailymail.co.uk",
			want: "Daily Mail",
		},
		{
			name: "east bay times domain",
			raw:  "eastbaytimes.com",
			want: "East Bay Times",
		},
		{
			name: "tmz override",
			raw:  "tmz.com",
			want: "TMZ",
		},
		{
			name: "spaced title case",
			raw:  "daily mail",
			want: "Daily Mail",
		},
		{
			name: "metadata alias override",
			raw:  "Mail Online",
			want: "Daily Mail",
		},
		{
			name: "the sun ireland domain override",
			raw:  "thesun.ie",
			want: "The Irish Sun",
		},
		{
			name: "the irish sun metadata override",
			raw:  "The Irish Sun",
			want: "The Irish Sun",
		},
		{
			name: "thejournal ie metadata override",
			raw:  "TheJournal",
			want: "The Journal",
		},
		{
			name: "thejournal metadata override",
			raw:  "TheJournal.ie",
			want: "The Journal",
		},
		{
			name: "fallback to link host",
			link: "https://www.foxnews.com/entertainment/story",
			want: "Fox News",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := formatPublicationName(test.raw, test.link); got != test.want {
				t.Fatalf("formatPublicationName(%q, %q) = %q, want %q", test.raw, test.link, got, test.want)
			}
		})
	}
}

func TestExtractPublicationNameFromHTML(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		html string
		want string
	}{
		{
			name: "og site name",
			html: `<!doctype html><html><head><meta property="og:site_name" content="Daily Mail"></head><body></body></html>`,
			want: "Daily Mail",
		},
		{
			name: "jsonld publisher",
			html: `<!doctype html><html><head><script type="application/ld+json">{"@context":"https://schema.org","@type":"NewsArticle","publisher":{"@type":"Organization","name":"East Bay Times"}}</script></head><body></body></html>`,
			want: "East Bay Times",
		},
		{
			name: "publisher alias override",
			html: `<!doctype html><html><head><meta name="publisher" content="Mail Online"></head><body></body></html>`,
			want: "Daily Mail",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, _, err := extractPublicationNameFromHTML([]byte(test.html))
			if err != nil {
				t.Fatalf("extractPublicationNameFromHTML() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("extractPublicationNameFromHTML() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestResolveClipPublicationsUsesMetadataForSurvivingResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><meta property="og:site_name" content="Example News"></head><body></body></html>`))
	}))
	defer server.Close()

	originalClient := publicationMetadataHTTPClient
	publicationMetadataHTTPClient = server.Client()
	t.Cleanup(func() {
		publicationMetadataHTTPClient = originalClient
	})

	items := []clip{{
		Publication: "Random Author",
		Link:        server.URL + "/story",
	}}

	got := resolveClipPublications(context.Background(), "test-search", items)
	if len(got) != 1 {
		t.Fatalf("resolveClipPublications() length = %d, want 1", len(got))
	}
	if got[0].Publication != "Example News" {
		t.Fatalf("resolveClipPublications() publication = %q, want %q", got[0].Publication, "Example News")
	}
}

func TestFetchPublicationNameFromArticleIncludesHTMLSnippetWhenMetadataMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html>
<html>
  <head>
    <title>Fallback Case</title>
    <meta charset="utf-8">
  </head>
  <body><h1>No publication metadata here</h1></body>
</html>`))
	}))
	defer server.Close()

	originalClient := publicationMetadataHTTPClient
	publicationMetadataHTTPClient = server.Client()
	t.Cleanup(func() {
		publicationMetadataHTTPClient = originalClient
	})

	outcome, err := fetchPublicationNameFromArticle(context.Background(), server.URL+"/story")
	if err != nil {
		t.Fatalf("fetchPublicationNameFromArticle() error = %v", err)
	}
	if outcome.Name != "" {
		t.Fatalf("fetchPublicationNameFromArticle() name = %q, want empty", outcome.Name)
	}
	if outcome.HTMLSnippet == "" {
		t.Fatal("fetchPublicationNameFromArticle() html snippet = empty, want preview")
	}
	if !strings.Contains(outcome.HTMLSnippet, "<head>") {
		t.Fatalf("fetchPublicationNameFromArticle() html snippet = %q, want head preview", outcome.HTMLSnippet)
	}
}

func TestIsNonOutletURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "aol uk excluded",
			raw:  "https://www.aol.co.uk/articles/love-island-star-maura-higgins-114756151.html",
			want: true,
		},
		{
			name: "news outlet kept",
			raw:  "https://www.thejournal.ie/maura-higgins-lands-a-spot-on-dancing-with-the-stars-us-7021495-Apr2026/",
			want: false,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := isNonOutletURL(test.raw); got != test.want {
				t.Fatalf("isNonOutletURL(%q) = %t, want %t", test.raw, got, test.want)
			}
		})
	}
}
