# Third-party notices

Third-party material redistributed **inside this repository** — files checked
in here, rather than dependencies fetched at build time. Dependencies declared
in `go.mod` and `dash/package.json` carry their own licenses through their own
distributions and are not restated here.

## Inter (font)

- **Files:** `dash/src/fonts/Inter-Regular.woff2`, `Inter-Medium.woff2`,
  `Inter-SemiBold.woff2`, `Inter-Bold.woff2`
- **Version:** 4.001 (`git-9221beed3`), the four upright weights the dashboard uses
- **Copyright:** Copyright (c) 2016 The Inter Project Authors
- **Upstream:** https://github.com/rsms/inter
- **License:** SIL Open Font License, Version 1.1 — the full text is in
  `dash/src/fonts/OFL.txt`, beside the font files

Inter is the dashboard's UI font. It is declared in `dash/src/fonts/inter.css`
and served from the built bundle, so no build or runtime fetches it from a
third party.

## Lucide (icon geometry)

- **File:** `dash/src/icons-data.js`
- **Version:** lucide-static 0.511.0
- **Upstream:** https://github.com/lucide-icons/lucide
- **License:** ISC

`dash/src/icons-data.js` holds the path geometry of the 71 Lucide outline icons
the dashboard renders, as the child element list of each 24×24 glyph.
`dash/src/icons.jsx` wraps them in the SVG frame. The geometry is verbatim
Lucide; nothing else of the project is included.

ISC license text:

```
ISC License

Copyright (c) for portions of Lucide are held by Cole Bemis 2013-2022 as part of Feather (MIT). All other copyright (c) for Lucide are held by Lucide Contributors 2022.

Permission to use, copy, modify, and/or distribute this software for any
purpose with or without fee is hereby granted, provided that the above
copyright notice and this permission notice appear in all copies.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
```

## Still to clear: the dashboard's design tokens and control styles

`dash/src/theme.css` and `dash/src/ui/controls.css` were derived from an
earlier design system, Quantic (`quantic-tokens` 1.60.0, `quantic-css` 1.47.0,
`quantic-components` 1.60.0). The dependency has been removed, but the
provenance of the values and rules carried across is not yet established. Facts
recorded while removing the dependency, for whoever picks it up:

- None of the five Quantic tarballs contained a `LICENSE` or `COPYING` file.
- Only `quantic-css` declared a license at all (`"license": "ISC"` in its
  `package.json`); `quantic-components`, `quantic-fonts`, `quantic-react` and
  `quantic-tokens` declared none.
- `quantic-fonts` shipped three families, none with a license file: Inter
  (SIL OFL 1.1, vendored above), Roboto 2.001101 (Apache-2.0, unused by this
  SPA and not vendored), and **Satoshi 2.000 — Indian Type Foundry, whose name
  table reads "All rights reserved" and points at https://fontshare.com/terms**.
  Satoshi was the login screen's display face; it is **not** OFL, so it was not
  vendored and those headings now use Inter.
