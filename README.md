# PressClips

Small htmx + golang web app to scrape relevant press clips associated with a given PR client.

Go backend + HTMX app that searches the last 24 hours across Brave Search, Exa, and NewsAPI, then deduplicates by URL and renders a unified clip list.

## Setup

1. Copy `.env.example` to `.env`.
2. Add API keys:
   - `BRAVE_API_KEY`
   - `EXA_API_KEY`
   - `NEWS_API_KEY`
3. Run the server:

```bash
go run main.go
```

4. Open `http://localhost:3000`.

## Output format

Each result is rendered as:

- publication (date)
- title
- link

No custom frontend JavaScript is required; UI updates are handled with HTMX.
