# Coverage site

A dependency-free static frontend for the provenance + lifejacket coverage
results. Served on GitHub Pages. Vanilla HTML/CSS/JS, hash-routed, no runtime
libraries.

Navigation: Services -> service (RPC table + aggregate totals) -> RPC detail
(input-field coverage by depth, outcome-aware branch metrics, explicit
asserted/missing HTTP statuses, covered-field evidence, actionable field and
branch gaps with source locations and predicate paths, provenance sinks, error
exits, mapping-review anomalies, mapped test facts, a copyable agent prompt, and
raw JSON).

## Honest-metric conventions

The UI never over-claims coverage:

- **Mapped vs unmapped.** Only RPCs whose operation appears in the lifejacket
  suite are mapped. Unmapped RPCs show `—` for coverage (undefined, *not* a
  verified zero); service aggregates are computed over mapped RPCs only.
- **Zero denominators.** When a mapped RPC's real denominator is known to be
  zero (e.g. it reads no leaf request fields), coverage shows `n/a`, not `0%`.
- **Static reachability.** All metrics are static field-sensitive provenance
  intersected with test inputs/assertions, not executed-branch coverage. This is
  stated in the footer and repeated in context.
- **Anomalies are suggestions.** HTTP↔RPC field mismatches are surfaced as
  human-review candidates for `fieldmap.json`; nothing is renamed automatically.

The redesign uses serif editorial headings (Georgia/Cambria) with a system
sans body and system monospace, a flat black/white/light-grey palette with a
single yellow accent, and no external fonts, logos, or gradients. Metric help
tooltips are keyboard- and touch-accessible; service search/filter/sort persist
across RPC drilldown.

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
