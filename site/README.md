# Coverage site

A dependency-free static frontend for the provenance + lifejacket coverage
results. Served on GitHub Pages.

Navigation: Services -> service (RPC list + totals) -> RPC (metrics, gaps,
statuses, errors, a copyable agent prompt for test generation, and raw JSON).

## Layout

- `index.html`, `style.css`, `app.js` plain HTML/CSS/vanilla JS, hash-routed.
- `gen_site_data.py` builds `data/` from `provenance/results/*.json`:
  - `data/services.json` service list + aggregates
  - `data/<svc>/index.json` service totals + per-RPC summary rows
  - `data/<svc>/<RPC>.json` per-RPC detail + agent prompt

## Regenerate locally

```sh
python3 site/gen_site_data.py
python3 -m http.server 8099 --directory site
# open http://localhost:8099
```

## Hosting

`.github/workflows/pages.yml` regenerates `data/` and deploys `site/` to Pages
on push. Set the repo's Pages source to "GitHub Actions" once (Settings ->
Pages -> Build and deployment -> Source: GitHub Actions).
