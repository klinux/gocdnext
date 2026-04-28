# Screenshots

Canonical screenshots used by the repo README + the docs landing page.
File names are stable so swapping the image content doesn't break the
references.

## Capture conventions

- **Viewport**: 1440 × 900 (DevTools "Responsive" mode, manual size).
- **Format**: PNG (lossless), no shadow/border decoration.
- **State**: pick a moment with realistic data — a couple of green
  runs, one or two with logs streaming, no error toasts.
- **Theme**: dark, the platform's default.

## Slots

| File                          | Page                     | Used in                           |
|-------------------------------|--------------------------|-----------------------------------|
| `01-dashboard.png`            | `/`                      | README hero, docs landing         |
| `02-run-detail.png`           | `/runs/<id>`             | README, docs landing              |
| `03-project-pipelines.png`    | `/projects/<slug>`       | README                            |
| `04-vsm.png`                  | `/projects/<slug>/vsm`   | README — the differentiator shot  |
| `05-plugins-catalog.png`      | `/plugins`               | README                            |
| `06-settings.png`             | `/settings/users` or `/settings/audit` | README       |

## Adding more

Drop the PNG in this directory and reference it. Resize +
re-export through ImageOptim / `pngcrush` to keep repo weight low.
