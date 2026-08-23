# anygrade landing - specification

The landing page is a single, static, self-contained marketing page for anygrade,
deployed to GitHub Pages from the `landing/` folder on `main` via a GitHub Actions
workflow, and served at `https://ekalinin.github.io/anygrade/`. Its job is to explain
what anygrade is, convince a teacher or self-hoster it is worth trying, and route them
to the repo and the quick start.

This document is the authoritative spec for the landing. README.md and `specs/SPEC.md`
remain authoritative for the product itself; landing copy is derived from them.

## 1. Goals

- Explain anygrade in one screen: what it is, who it is for, why it is different.
- Route visitors to the GitHub repo (star / clone) and to a copy-pasteable quick start.
- Ship as a single static page with zero external requests: no CDN, no web fonts, no
  analytics, no framework. Fast, private, works offline and behind firewalls -
  consistent with anygrade's self-hosted ethos.
- Stay trivially maintainable: hand-written HTML/CSS/JS, no build step.

## 2. Non-goals

- Live demo instance.
- Product screenshots / UI captures (illustration is code and terminal blocks only).
- A documentation hub or multi-page docs (README.md and SPEC.md remain the docs).
- Localization of the landing (English only, even though the product UI is EN/RU).
- Analytics or any third-party script.
- Custom domain (uses the default `github.io` project path).
- Light/dark theme toggle (dark only).

## 3. Delivery and hosting

- Source of truth: `landing/` on the `main` branch.
- Deploy mechanism: a GitHub Actions workflow (`.github/workflows/pages.yml`) uploads
  the `landing/` folder as a Pages artifact and publishes it. GitHub Pages
  "Deploy from a branch" only supports the repo root or `/docs`, so serving from
  `landing/` requires the Actions path. Enable it once in Settings -> Pages ->
  "Build and deployment" -> Source: "GitHub Actions".
- Public URL: `https://ekalinin.github.io/anygrade/` (project pages, `/anygrade/`
  subpath).
- `landing/.nojekyll` keeps Jekyll from touching the artifact (harmless belt-and-braces
  even though the Pages Actions serve files as-is).
- Custom domain later (optional): add `landing/CNAME` with the domain and configure
  DNS; no other change is needed because internal links are relative (see 6.1).

### 3.1 File layout

```
landing/
  index.html      # the single landing page
  styles.css      # hand-written dark theme
  main.js         # progressive enhancement: copy buttons, tabs, highlighting
  favicon.svg     # minimal monospace "ag" mark in the accent color
  og-image.svg    # source for the social preview (author-time only)
  og-image.png    # 1200x630 rasterized social preview (committed)
  .nojekyll       # keep Jekyll out
.github/workflows/pages.yml   # builds + deploys landing/ to Pages
```

Prerequisite outside `landing/`: a root `LICENSE` file (MIT), referenced by the footer.

## 4. Design system

Dark, developer-forward, terminal-flavored. The palette was originally derived from the
app's semantic colors. The app has since moved to its own design language (see
`specs/SPEC.ui.md`), so the two no longer share a token vocabulary; the landing keeps
the palette below until it is redesigned separately.

```css
:root{
  --bg:#0d1117; --surface:#161b22; --line:#30363d;
  --fg:#e6edf3; --muted:#8b949e;
  --accent:#58a6ff;                 /* app blue #1d4ed8, lightened for dark */
  --ok:#3fb950; --warn:#d29922; --bad:#f85149;   /* maps to app ok/warn/bad */
  --mono:ui-monospace,SFMono-Regular,"SF Mono",Menlo,Consolas,monospace;
  --sans:system-ui,-apple-system,"Segoe UI",sans-serif;  /* same stack as the app */
}
```

- Prose uses `--sans`; the wordmark, code, terminal blocks, and accents use `--mono`.
- Content column ~60rem (matches the app); hero and section wrappers up to ~72rem.
  Mobile-first and responsive; terminal/code blocks scroll horizontally when narrow.
- Terminal blocks render as a faux window: a title bar with three "traffic light"
  dots, a dim shell prompt, and colored `remote:` output lines.
- No images beyond a text wordmark and the favicon.

### 4.1 Accessibility

- Semantic landmarks: `header`, `main`, `section`, `footer`.
- WCAG AA contrast on the dark surface.
- Visible `:focus-visible` outlines; a skip-to-content link.
- `prefers-reduced-motion: reduce` disables transitions/animations.
- The HTTP/SSH switch follows the ARIA tabs pattern (`tablist`/`tab`/`tabpanel`,
  `aria-selected`, arrow-key navigation).

## 5. Content

Copy is derived from README.md and SPEC.md. Section order top to bottom.

### 5.1 Hero

- Wordmark: `anygrade`.
- Headline: "A single Go binary that turns any git repo of course tasks into a grading
  system."
- Sub: "Students push. It grades. You teach."
- CTAs: `Get started` (anchors to the quick start) and `GitHub` with a star glyph
  (links to `https://github.com/ekalinin/anygrade`).
- Visual: a faux terminal showing the real push output -

```
remote: anygrade: 2 task(s) detected
remote:   01-intro   submission #142 queued   http://host/submissions/142
remote:   02-structs rejected: hard deadline passed (2026-10-01 23:59 +03)
```

### 5.2 How it works (4 steps)

1. The teacher keeps tasks in a plain git repo; each task is a directory with a
   `task.yaml`.
2. Each student gets a personal server-side clone. They clone, edit, commit, push.
3. A push hook diffs the branch head against the last processed commit, maps changed
   paths to tasks, and queues one submission per changed task. Feedback starts in the
   push output.
4. A worker builds a clean workspace (authoritative task files + the student's
   `solution_files` + hidden tests), runs the checks in Docker or on the host, and
   streams results live in the web UI.

Callout: editing open tests, `task.yaml`, or build files in the student repo is
ineffective - the authoritative versions are restored before checking and such edits
are logged for the teacher. Pushes are never rejected for policy reasons; deadlines
and attempt limits reject the submission, reported in the push output.

### 5.3 Features grid

Two groups.

- Teachers: YAML-configured (`course.yaml` / `task.yaml`); language-agnostic checks
  (arbitrary command); score matrix with click-through to student code; manual score
  overrides with comments; CSV export; live queue (cancel / recheck); hidden tests
  (private repo or local path, offline cache fallback); weighted checks with required
  "gate" checks; soft/hard deadlines with late penalties; attempt limits and
  cooldowns; `best` or `latest` scoring policy; optional leaderboard (with anonymize).
- Students: familiar git flow; feedback in the push output; live web UI (status,
  history, per-check results, penalty breakdown, streaming logs); local self-check
  (`anygrade check`); rechecks via a `[recheck <task-id>]` commit marker or a button;
  bilingual EN/RU UI.

### 5.4 Quick start / install

Each command block is copy-pasteable and gets a copy button (5.5).

- Build:

  ```sh
  go build -o anygrade ./cmd/anygrade
  ```

- Teacher (in the course repo; add `.anygrade/` to `.gitignore`):

  ```sh
  ./anygrade validate
  ./anygrade user add --login prof --role teacher
  ./anygrade serve --http-addr :8080 --ssh-addr :2222
  ```

- Student git setup - a tabbed HTTP/SSH block:

  HTTP (username = login, password = the token):

  ```sh
  git clone http://host:8080/git/<login>/course.git
  git remote add upstream http://host:8080/git/course.git
  ```

  SSH (key-based):

  ```sh
  git clone ssh://git@host:2222/<login>/course.git
  git remote add upstream ssh://git@host:2222/course.git
  ```

- Note: the personal token is the git-over-HTTP password and the web login; SSH auth
  is by key only.
- Requirements: Go 1.26+, the `git` binary, optional Docker (colima on macOS).
- Links to the README quick start and `specs/SPEC.md`.

### 5.5 Security + why

- Untrusted student code -> Docker isolation: one ephemeral container per check;
  memory/cpu/pids limits; no network by default; read-only base image; non-root user;
  tmpfs workspace; hard wall-clock timeout. Serving on a non-loopback address with any
  task on the local runner refuses to start unless `--allow-local-runner` is passed.
- Tokens and invite links are stored hashed; SSH is limited to git commands; failed
  logins are rate-limited per (client IP, login) across git and the web; role checks
  on every route (students get 404, not 403).
- Differentiators: single Go binary; SQLite + embedded assets; everything in one data
  dir (backup = copy the dir); self-hosted with no external identity (OAuth/LMS out of
  scope, CSV export is the integration point); one instance serves exactly one course.

### 5.6 Footer

- GitHub repo link + a "Star on GitHub" call to action.
- "Built with Go" (a version string may be added once a release tag exists).
- Author credit: "Eugene Kalinin (@ekalinin)", linked to the GitHub profile.
- "MIT License", linked to the repo `LICENSE`.

## 6. Implementation notes

### 6.1 Subpath and Pages correctness

- The site is served under `/anygrade/`, so every internal asset and anchor link is
  relative (`href="styles.css"`, `href="#quick-start"`) - never root-absolute
  (`/styles.css`).
- The only absolute URLs are the SEO ones that crawlers require: `og:image`, `og:url`,
  and the canonical link.
- Performance budget: total page weight under ~100 KB (one CSS file, one JS file, one
  PNG).

### 6.2 Progressive enhancement (`main.js`)

Vanilla JS, tiny, no dependencies. The page is fully usable with JavaScript disabled.

- Copy-to-clipboard: on load, inject a copy button into each code/terminal block;
  uses `navigator.clipboard`. Without JS there is no button and the code is still
  selectable.
- Tabs: both the HTTP and SSH panels exist in the HTML; JS hides the inactive one and
  wires clicks and arrow keys. Without JS both panels are visible.
- Syntax highlighting: a hand-rolled minimal highlighter for `sh`, `yaml`, and the
  `remote:` push output only (regex-based `<span>` wrapping, dependency-free, offline).
  Without JS the code renders plain and is already legible. No external highlighter.

### 6.3 SEO / social

- `<title>` + meta description; `<html lang="en">`; a canonical link (absolute URL).
- Open Graph and Twitter `summary_large_image` cards. `og:image` is the absolute
  `https://ekalinin.github.io/anygrade/og-image.png`; `og:url` is absolute.
- OG image: `og-image.svg` (dark; wordmark + headline + a slice of the push output) is
  rasterized once to `og-image.png` at 1200x630 and committed, so visitors and
  crawlers never need the rasterizer.
- `favicon.svg` referenced via `<link rel="icon" type="image/svg+xml">`.

### 6.4 Tooling (Makefile)

Repetitive commands live in the Makefile (project convention):

- `make landing-serve` - `python3 -m http.server -d landing 8000` for local preview.
- `make landing-og` - rasterize `landing/og-image.svg` to `landing/og-image.png`
  (`rsvg-convert -w 1200 -h 630`; requires `librsvg`, e.g. `brew install librsvg`).

## 7. Verification

1. `make landing-serve`, open `http://localhost:8000/`: check desktop and mobile
   layout, copy buttons, HTTP/SSH tab switching by mouse and keyboard, highlighting,
   no console errors, and that the network panel shows only same-origin requests.
2. Disable JavaScript and reload: the page is fully readable, both git-setup blocks
   are visible, all content is present.
3. Run Lighthouse or axe for accessibility/SEO/performance sanity; confirm AA contrast.
4. `make landing-og` regenerates the PNG; verify the OG tags resolve (absolute image
   URL) via a card inspector or page source.
5. After merge: set Settings -> Pages -> Source to "GitHub Actions", let the
   `pages.yml` workflow run, and verify `https://ekalinin.github.io/anygrade/` loads
   and unfurls with the OG image.

## 8. Open items

- Add a root `LICENSE` (MIT). The footer license link depends on it; the repo has none
  today.
- The OG rasterizer (`librsvg`, or resvg / headless Chrome) is an author-time
  dependency only.
- A footer version string can be wired once the project cuts a release tag.
