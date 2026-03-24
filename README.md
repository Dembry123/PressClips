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

Results are grouped under an `[ONLINE]` section header. Each entry is rendered as:

- PUBLICATION NAME (Month Day, Year)
- Article title
- Link

Results are filtered to English-language articles from official media outlets only (social media, forums, and UGC platforms are excluded). Titles are merged across providers to prefer the longest, non-truncated version.

No custom frontend JavaScript is required; UI updates are handled with HTMX.
