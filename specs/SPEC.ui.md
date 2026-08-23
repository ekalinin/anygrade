# anygrade app UI - design specification

The app UI is the server-rendered web interface in `internal/web`: the pages a student
and a teacher actually work in. This document is the authoritative spec for its visual
design. `specs/SPEC.md` remains authoritative for behavior, and `specs/SPEC.landing.md`
for the marketing page.

Today the UI is competent but anonymous: `system-ui` everywhere, a GitHub-blue accent,
pill badges, and a plain table on every screen. It could be any Go admin panel. The job
of this redesign is a visual identity that is unmistakably anygrade, applied across all
screens, without changing behavior.

## 1. Goals

- Give the app a design language of its own, derived from what the product actually is.
- Keep the diff small: extend the structures already in the templates rather than
  replacing them. No new dependencies, no build step, and exactly one line of Go (7.1).
- Preserve every behavioral contract: htmx SSE row swaps, Go-generated CSS class names,
  EN/RU parity, offline operation, single-binary delivery.
- Stay accessible: WCAG AA contrast in both schemes, visible focus, reduced motion.

## 2. Non-goals

- Restructuring pages, navigation, or information architecture.
- New screens, or new data in existing screens.
- A client-side framework. htmx plus SSE stays exactly as it is.
- Any external request at runtime. No CDN, no font service, no analytics.
- Realigning `landing/styles.css` to the new language (see 10).

## 3. Concept: the ledger

`internal/gradebook` already names the thing. anygrade is a mark book: rows of records,
a machine entering marks, and one human who can overrule it. The design takes that
literally.

The horizontal rule becomes the primary structural device instead of the card or the
box. This is deliberately continuous with what the templates already do, since every
screen is a table whose rows are separated by `border-bottom`. The redesign promotes
that accident into the system.

### 3.1 Signature: the correction

The one element the interface is remembered by. Wherever a score has been overridden,
the machine's number is shown struck through and the teacher's number beside it in red,
with the override comment as a margin note under the record's name.

```
03-interfaces │ Интерфейсы                    OVERRIDDEN    7̶2̶ 85 / 100
              │ ✎ teacher: partial credit for the tricky edge case
```

Red appears nowhere else in the interface. It means exactly one thing: a human
intervened. Machine verdicts, including failures, use the earthy quartet in 4.1.

This uses data already present in the view models, and today thrown away:

- `web.TaskView.Score` (computed) against `TaskView.Override.Score` (hand-set)
- `gradebook.Cell.Computed` against `gradebook.Cell.Override`

Both are currently collapsed into a single displayed number, with the fact of an
override surviving only as a `title` tooltip and a `*` glyph. The signature is a
template change only.

Restraint: this is the only place the design spends boldness. Everything else stays
quiet, and the `st-overridden` stamp itself stays neutral so the number carries the
meaning rather than competing with it.

## 4. Design system

### 4.1 Color

Cool sage-tinted paper and ink, not the current GitHub surfaces. Machine verdicts are
earthy and low-chroma; the pen is high-chroma and always hotter than `--fail`, in both
schemes, so the two can never read as the same signal.

```css
:root{
  color-scheme:light;
  --paper:#EFF3F0; --sheet:#FFFFFF; --rule:#D2DAD5; --rule-heavy:#15201B;
  --ink:#15201B; --ink-2:#59685F; --ink-3:#8A988F;
  --pass:#16643F; --fail:#8E3024; --warn:#7E5A0B; --live:#0B5D6B;
  --pen:#C42D1A;                      /* the human hand, and nothing else */
  --blue:#2C5F8A;                     /* focus ring and selection only */
  --pass-w:rgba(22,100,63,.09);  --fail-w:rgba(142,48,36,.09);
  --warn-w:rgba(126,90,11,.10);  --live-w:rgba(11,93,107,.09);
}
:root[data-theme=dark]{
  color-scheme:dark;
  --paper:#121915; --sheet:#19211D; --rule:#2E3A34; --rule-heavy:#9DB0A6;
  --ink:#DFE8E2; --ink-2:#93A29A; --ink-3:#6E7D75;
  --pass:#58C68C; --fail:#D98A7E; --warn:#DDA53A; --live:#4FC3D8;
  --pen:#FF5A3D;
  --blue:#7FB2E0;
  --pass-w:rgba(88,198,140,.10); --fail-w:rgba(217,138,126,.10);
  --warn-w:rgba(221,165,58,.10); --live-w:rgba(79,195,216,.10);
}
```

Notes:

- The dark scheme is a green-black, not `#0d1117`. Ledger at night, not GitHub dark.
- `--fail` in dark is deliberately dusty and desaturated so that `--pen` reads as the
  hotter of the two at small sizes and in dense matrix cells.
- The existing three-way theme mechanism is unchanged: `:root` carries
  `color-scheme: light dark` under `auto`, and `[data-theme=light|dark]` pins it.
- The tokens are written with `light-dark()`, one declaration per token carrying both
  values, as the current file already does. The two blocks above are the source values,
  not the target form. Keeping `light-dark()` preserves `auto` following the OS and
  keeps the diff to changed values rather than a restructured file.

### 4.2 Type

Three roles, each with exactly one job.

| Role | Face | Used for |
|---|---|---|
| Display | Geologica, variable 400-700 | headings, section captions, column heads, stamps, nav, record names, buttons |
| Body | `system-ui` stack (unchanged) | prose, task statements, hints, flashes |
| Figures | existing mono stack (unchanged) | every number, plus code, logs, SHAs, IDs, timestamps |

Setting every figure in mono is functional, not decorative: score, attempt, duration and
deadline columns align by construction, which is what a ledger requires. It also costs
nothing, since the mono stack is already there.

Geologica is embedded, latin plus cyrillic, variable across the 400-700 range:

- `static/fonts/geologica-latin.woff2` - 24.4 KB
- `static/fonts/geologica-cyrillic.woff2` - 16.0 KB

40 KB total against a 22 MB binary. Split by `unicode-range` so a page of English text
never downloads the Cyrillic subset. `font-display: swap` so text is readable before the
face lands, and the `system-ui` stack is the declared fallback, meaning a failed or
blocked font request degrades to today's appearance rather than to invisible text.

Cyrillic coverage is a hard requirement, not a nicety: the UI ships EN and RU and the
display face sets headings and record names in both.

Scale, all in the display face unless noted:

- Page title (`h1`): 1.45rem / 600 / -0.02em
- Section caption (`h2`): 0.8rem / 600 / 0.18em / uppercase, over a 2px `--rule-heavy`
- Column head (`th`): 0.68rem / 600 / 0.13em / uppercase / `--ink-3`
- Record name: 0.92rem / 500 / -0.005em
- Stamp: 0.66rem / 600 / 0.12em / uppercase
- Figures: 0.84rem mono, `font-variant-numeric: tabular-nums`

### 4.3 Structure

**The gutter.** Every register's first column is the record's key, set in mono, muted,
right-aligned, and ruled off from the entry with a `border-right`. The key is the task
ID on the dashboard, `#<id>` on submissions and the queue, the check name in a results
table, and the login in the matrix and students list. This is useful rather than
ornamental: the task ID is the string a student types in a `[recheck <task-id>]` commit
marker, and it is currently not shown on the dashboard at all.

**The caption.** Section headings are tracked uppercase over a 2px `--rule-heavy` rule,
with an optional mono caption-note flush right for counts and totals.

**The register.** Rows on hairline rules, no zebra striping, hover lifts the row to
`--sheet`. Matrix rows use tighter vertical padding than dashboard rows, because thirty
students need density where six tasks do not.

**Radius.** Containers (statement sheet, log pane, inputs, buttons) keep a soft
0.4rem radius. Stamps take a hard 2px. The contrast between the two is deliberate.

### 4.4 Components

**Stamp** (`.badge`, plus the Go-generated `.st-*` modifier). Replaces the pill: squared
2px radius, 1px `currentColor` border, tracked uppercase display face, and a ~10% tint
of its own hue. A stamped verdict rather than a chip.

The modifier set is generated in Go by `statusClass` as `"st-" + status` with spaces and
underscores replaced by hyphens, so the stylesheet must keep covering exactly:
`st-passed`, `st-partial`, `st-failed`, `st-queued`, `st-running`, `st-retrying`,
`st-error`, `st-rejected`, `st-rejected-deadline`, `st-rejected-limit`, `st-skipped`,
`st-not-started`, `st-overridden`, `st-canceled`. None of these names may change.

`st-canceled` is absent from the current stylesheet even though `handlers_queue.go`
produces it and the matrix status filter offers it, so a canceled submission renders as
an unstyled bare badge today. The new sheet covers it, as a neutral stamp.

`st-overridden` is neutral (`--ink-2` on transparent, `--rule` border). A hand-set score
is not a machine pass, and the red number is already carrying the signal.

**Link.** Ink-colored with a 1px `--rule` underline that darkens to `--ink` on hover.
Print convention, no browser blue anywhere. Nav links and the brand opt out of the
underline. This must be set as a base `a` rule so that links inside tables and panes
cannot fall through to the UA default.

**Meta strip.** The task and submission header facts become stacked label-over-value
pairs: a tracked uppercase micro-label in `--ink-3` above a mono value.

**Sheet.** The task statement keeps its card: `--sheet` background, `--rule` border,
0.4rem radius. It is the one place the design uses a container, because a statement is a
document, not a record.

**Pane.** Collapsible log output keeps `<details>`, with a mono summary on `--paper` and
the log body in mono on `--sheet`.

### 4.5 CSS hygiene

The current stylesheet is 262 lines of mostly flat selectors and should stay that way.
Specifically:

- Set page-level rhythm on section wrappers, not on both a type selector and an element
  selector that then cancel out.
- Keep `.st-*` modifiers as single-class selectors so Go-generated names always win.
- No `!important` outside the existing `prefers-reduced-motion` block.

## 5. Screens

CSS-only, no template change:

- Login, register, invite, token display, key challenge
- Audit, leaderboard, settings, code view
- Submission page and `sub-results` partial

Template changes, rendering the correction. The override is displayed on four surfaces,
and all four get the same treatment so the gesture reads as one convention:

- `dashboard.html` / `partials/task_row.html` - task ID into the gutter column; struck
  computed score plus pen override; comment as a margin note
- `task.html` - same correction treatment in the score line of the meta strip
- `matrix.html` / `partials/matrix_row.html` - struck computed plus pen override in the
  cell, replacing the `*` glyph and the `.ovr` class
- `student.html` - the teacher's own view of one student already has separate score and
  override columns, so the computed score is struck in place and the override rendered
  in pen, with its comment no longer duplicated as both `title` and visible text

Template changes, gutter class only (`class="key"` on the existing first column, plus
the matching `<th>`): `queue.html` / `partials/queue_row.html`, `students.html`.

One semantic fix, in scope because it misuses the verdict palette: `students.html` and
`student.html` render account state with `st-passed` / `st-failed`. A disabled account is
not a failed check. Active becomes a neutral stamp and disabled a warn stamp.

## 6. Constraints preserved

- **SSE row swaps.** `task-row`, `matrix-row` and `queue-row` are swapped whole via
  `sse-swap` with `hx-swap="outerHTML"`, so each must remain a single `<tr>` with the
  same `sse-swap` name. The gutter is a `<td>` inside that row, never a wrapper around
  it. A partial re-rendered by SSE receives no loop index, which is why the gutter shows
  the record's own key rather than an ordinal.
- **Go-generated class names.** `statusClass` output, per 4.4.
- **i18n.** Every string stays a `{{t ...}}` lookup. The margin note reuses the existing
  `task.override_note` key; no new user-visible English is introduced in templates.
- **Embedding.** `//go:embed templates static` is already recursive, so
  `static/fonts/*.woff2` is embedded with no directive change. Serving the correct
  content type needs one line, see 7.1.
- **Auth.** `/static/` is unauthenticated, so the login page gets the font like any
  other page.

## 7. Font pipeline

- Source: Geologica, SIL Open Font License 1.1, via Google Fonts. The license permits
  embedding and redistribution; the OFL text ships alongside the files as
  `static/fonts/LICENSE-Geologica.txt`.
- The two `.woff2` subsets are committed, exactly like `htmx.min.js` is today.
- `static/VERSIONS` gains a line recording the family, version, and source URL, matching
  the existing format for htmx.
- A `make fonts` target refetches the subsets, so the provenance is reproducible rather
  than folklore. It is an author-time target only; nothing at build or run time needs
  network access.

### 7.1 Content type

`.woff2` is absent from Go's builtin MIME table (`mime.builtinTypesLower`). It resolves
on a developer Mac only because `osInitMime()` loads a system table; in a minimal Linux
container with no `/etc/mime.types`, `mime.TypeByExtension(".woff2")` returns `""` and
`http.ServeContent` falls back to sniffing, serving `application/octet-stream`.

Browsers do not enforce the content type for `@font-face` the way they do for ES
modules, so the font would still load, but the response would differ between a developer
machine and a deployed container for no good reason. `staticHandler` therefore registers
the type explicitly:

```go
mime.AddExtensionType(".woff2", "font/woff2")
```

This is the only Go change in the redesign. A test asserts the served content type so
the guarantee holds on every platform rather than depending on the host's MIME table.

## 8. Accessibility

- WCAG AA contrast for every token pair, in both schemes, including each status color on
  `--paper` and on its own tint.
- `--pen` and `--fail` must stay distinguishable by more than hue. The correction is
  always a pair, machine number struck plus pen number, so strikethrough carries the
  distinction independently of color perception.
- `:focus-visible` outlines in `--blue` at 2px with 2px offset, on every interactive
  element.
- The skip link, semantic landmarks, and the `prefers-reduced-motion` block are kept.
- Status is never encoded by color alone: every stamp carries its translated word, and
  matrix cells keep their `title` attribute.

## 9. Verification

1. `make check` passes. The web tests assert on rendered markup, so template edits must
   keep existing assertions green or update them deliberately.
2. `make e2e` passes.
3. Serve the example course and walk every screen in both themes and both locales:
   dashboard, task, submission mid-run, matrix, queue, students, student, audit,
   leaderboard, settings, login, register, code view.
4. Confirm the correction renders on all three surfaces: dashboard row, task page, and
   matrix cell, in EN and RU.
5. Confirm live behavior is intact: SSE status swap on the submission page, row swaps on
   dashboard, matrix and queue, and log streaming into the pane.
6. Check contrast with an automated auditor in both schemes; check keyboard traversal
   and visible focus on every page.
7. Confirm in the network panel that only same-origin requests are made, and that a page
   of pure English text does not fetch the Cyrillic subset.
8. Load with the font blocked and confirm the UI degrades to the `system-ui` fallback
   with no layout break.
9. Confirm binary growth is within ~40 KB.

## 10. Open items

- **Landing divergence.** `app.css` currently states that its token vocabulary mirrors
  `landing/styles.css` so the app and the marketing page share one design language, and
  `SPEC.landing.md` §4 states the reverse dependency. This redesign breaks that
  coupling, and the landing keeps the old GitHub-dark palette until it is redesigned
  separately. Both comments must be updated to say so plainly, rather than left
  asserting a link that no longer holds. Realigning the landing is a follow-up.
- The task statement is teacher-authored Markdown. Its rendered typography inside the
  sheet should be reviewed against real course content, not lorem text.
- The code view (`code.html`) has open TODO items for syntax highlighting and diffs.
  This spec only restyles it; those features remain out of scope.
