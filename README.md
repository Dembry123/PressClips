# PressClips
Go backend + HTMX app that searches the last 24 hours across Brave Search and Exa, then deduplicates by URL and renders a unified clip list formatted to match PR industry press break emails.

## Setup

1. Copy `.env.example` to `.env`.
2. Add API keys:
   - `BRAVE_API_KEY`
   - `EXA_API_KEY`
3. Run the server:

```bash
go run main.go
```

4. Open `http://localhost:3000`.

## Deployment

Deployed on Fly.io at `pressclips.fly.dev`.

```bash
fly deploy
```

API keys are set as Fly secrets (`fly secrets set KEY=value`).

## Output format

Results are grouped under an `[ONLINE]` section header in an Outlook-friendly HTML block. Each entry is rendered as:

- PUBLICATION NAME (Month Day, Year)
- Article title
- Link

Results are filtered to English-language articles from official media outlets only. Social platforms, forums, search engines, and syndicated aggregation hosts such as AOL, MSN, Yahoo, NewsBreak, SmartNews, and Flipboard are excluded. Titles are merged across providers to prefer the longest, non-truncated version.

The UI uses HTMX for search updates and a small amount of frontend JavaScript for rich clipboard copy so pasted results retain their email formatting more reliably in Outlook.
