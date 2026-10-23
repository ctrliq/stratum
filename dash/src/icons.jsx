// Icon façade — maps the app's icon names to vendored Lucide glyphs
// (src/icons-data.js). Screens keep calling <Icon name="server" size={16} />.
//
// This file vendors the glyphs directly instead of re-exporting them from
// the design system's web-component package (a 5 MB dependency for 71
// glyphs). The geometry is Lucide's, ISC-licensed: same paths, same 24×24
// frame, no dependency and no token.
// Icon() below reproduces the design system's IconBase element for
// element: a box sized in px wrapping the 24×24 glyph, stroke-width 2,
// inheriting currentColor.
import { GLYPHS } from './icons-data.js';

const cx = (...parts) => parts.filter(Boolean).join(' ');


export function Icon({ name, size = 16, style, className, ...rest }) {
  const glyph = GLYPHS[name];
  if (!glyph) return null;
  // Two details of IconBase that matter:
  //   - the wrapper box. A box around the glyph sits on the text baseline
  //     differently from a bare (replaced) <svg>; without it every table row
  //     with an icon beside a label lost 2.5px.
  //   - `size` as a custom property, not width/height. The DS set --icon-size on
  //     the host, which let the sidebar and navbar override the box size from a
  //     stylesheet (they force 16px and 20px); an inline width would win over
  //     those rules and the labels would shift.
  return (
    <span className={cx('qc-icon', className)} style={{ '--qc-icon-size': `${size}px`, ...style }} {...rest}>
      <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"
        fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"
        aria-hidden="true" focusable="false">
        {glyph.map(([tag, attrs], i) => {
          const El = tag;
          return <El key={i} {...attrs} />;
        })}
      </svg>
    </span>
  );
}
