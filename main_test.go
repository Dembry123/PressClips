package main

import "testing"

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
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := extractPublicationNameFromHTML([]byte(test.html)); got != test.want {
				t.Fatalf("extractPublicationNameFromHTML() = %q, want %q", got, test.want)
			}
		})
	}
}
