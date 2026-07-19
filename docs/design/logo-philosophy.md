# Glyph Reverence — codex-api-gateway Logo

## The Movement: "Glyph Reverence"

Communication through the veneration of a single, irreducible typographic
gesture. Where Brutalism celebrates the monumental block and Swiss formalism
worships the grid, Glyph Reverence worships the **prompt** — the smallest
unit of computational dialogue.

The movement treats the terminal prompt (`>`), the code bracket (`</>`), and
the cursor underscore (`_`) not as punctuation but as **sacred glyphs**. Each
is a threshold between intent and execution, a doorway between human thought
and machine action.

## Visual Expression

**Form.** A single rounded square, obsessively proportioned. Inside, one
glyph — the terminal prompt `>` — rendered with master-level precision: no
decorative flourishes, no gradients, no noise. The glyph is constructed from
geometric primitives (two strokes meeting at a clean angle), not typeset from
a font.

**Space and Silence.** Negative space is the loudest element. The glyph
floats within the square, breathing, never touching the edges. This silence
is the visual equivalent of a terminal waiting for input.

**Color.** A two-tone discipline. A deep charcoal near-black (never pure
`#000000` — a whisper of warmth, `#0F1419`) holds the ground. The glyph is
rendered in luminous off-white (`#F4F4F5`), the color of freshly printed
paper, of a clean terminal on a dark theme. No third color is permitted.

**Scale and Rhythm.** The glyph occupies roughly 55–60% of the square's
inner area, optically centered on the square's center (slightly right-shifted to
counter the leftward visual bias of the diagonal). The stroke weight is
calibrated so the glyph stays legible at 16×16 — the smallest canvas the tray
will ever demand.

## The Glyph: ">" — The Codex Signature

The mark is a single `>` prompt — and that is the whole point. **Codex**,
OpenAI's coding agent, is built on the terminal prompt: the `>` is the
threshold where a command begins, the universal symbol of "ready for your
instruction." By reducing the identity to this one irreducible gesture, the
gateway reads, at a glance, as part of the Codex lineage — a local bridge
that lets Codex speak to Anthropic backends without losing its own face.

Nothing else. No channel line, no cursor, no arch. Restraint *is* the design.

## Craftsmanship

Every element must appear as though it took countless hours to create. The
corners are not arbitrary; the stroke weight is not default; the color is not
pure. This is the product of deep expertise — an icon that lives at 16 pixels
and at 256 pixels and must command attention and convey meaning at both.

## Source & Build

- `docs/design/logo.svg` — editable vector source (256×256 viewBox).
- `assets/logo.png` — rasterized 256×256 RGBA (transparent), embedded via
  `go:embed` and shared by the system tray and the admin favicon.
- Rebuild the PNG from the SVG (preserves alpha):
  `rsvg-convert -w 256 -h 256 -b none docs/design/logo.svg -o assets/logo.png`
