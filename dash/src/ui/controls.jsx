// Controls — the dashboard's first-party plain-JSX components, so the SPA
// carries no design-system dependency. The markup each one renders is a
// handful of elements; it lives here, and its styles live in controls.css.
//
// Fields are ordinary controlled inputs with one `onChange` (no separate
// value-changed event), and composition is props (`icon`, `after`,
// `footer`) or children, never named slots.
import { Children, Fragment, useId, useState, useRef, useEffect, useCallback, forwardRef } from 'react';
import { Icon } from '../icons.jsx';
import { parseSplit, paneBounds, layoutOf, constrainPair } from './split.js';
// NB: controls.css is imported by main.jsx, before app.css — the cascade order
// matters and belongs where the other stylesheets are ordered, not here.

const cx = (...parts) => parts.filter(Boolean).join(' ');

// ---- Button ---------------------------------------------------------------
// variant: primary | secondary | outline | ghost.  size: xs | sm | md | lg.
// `icon` makes it square (icon-only); grow="always" makes it full width.
export function Button({
  variant = 'primary', size = 'md', icon = false, grow, type = 'button',
  disabled = false, className, children, ...rest
}) {
  return (
    <button
      type={type}
      disabled={disabled}
      className={cx('qc-btn', `qc-btn--${variant}`, `qc-btn--${size}`,
        icon && 'qc-btn--icon', grow === 'always' && 'qc-btn--grow-always', className)}
      {...rest}
    >
      {children}
    </button>
  );
}

// ---- Card -----------------------------------------------------------------
// Only the default (outline, md) shape is used; the DS's interactive/action-icon
// card modes had no call site here.
// Two boxes, as the DS had: the outer one is the size container its own padding
// rule queries, the inner one is the padded, bordered block the caller fills.
// (The DS's flex `gap` fell between its header/body/footer slots, never between
// the caller's children — they all landed in the body.)
export function Card({ className, children, ...rest }) {
  return (
    <div className={cx('qc-card', className)} {...rest}>
      <div className="qc-card-main">{children}</div>
    </div>
  );
}

// ---- Table ----------------------------------------------------------------
// Real table elements. The DS built the grid from divs with ARIA roles, which
// cost it `colspan` — the empty-state row it renders under every list sat in
// the first column instead of spanning the table. A <td colSpan> just works.
export function Table({ size = 'md', className, children, ...rest }) {
  return (
    <div className={cx('qc-table', `qc-table--${size}`, className)} {...rest}>
      <table>{children}</table>
    </div>
  );
}
export const TableHeader = ({ children, ...rest }) => <thead {...rest}>{children}</thead>;
export const TableBody = ({ children, ...rest }) => <tbody {...rest}>{children}</tbody>;
export const TableRow = ({ selected, className, children, ...rest }) => (
  <tr className={cx(selected && 'is-selected', className)} {...rest}>{children}</tr>
);
// `width` is passed through to the inline style exactly as the DS did, so a
// unitless value stays inert and a "140px" one takes effect.
export const TableHead = ({ width, style, children, ...rest }) => (
  <th style={width ? { width, ...style } : style} {...rest}>{children}</th>
);
export const TableCell = ({ width, style, children, ...rest }) => (
  <td style={width ? { width, ...style } : style} {...rest}>{children}</td>
);

// ---- TextInput ------------------------------------------------------------
// forwardRef so the command palette can still focus the field on open.
export const TextInput = forwardRef(function TextInput(
  { size = 'md', type = 'text', ariaLabel, className, style, disabled, ...rest }, ref,
) {
  const [showPassword, setShowPassword] = useState(false);
  const isPassword = type === 'password';
  return (
    <div className={cx('qc-field', `qc-field--${size}`, className)} style={style}>
      <input
        ref={ref}
        type={isPassword && showPassword ? 'text' : type}
        aria-label={ariaLabel}
        disabled={disabled}
        {...rest}
      />
      {isPassword && (
        <Button variant="ghost" size="xs" icon className="qc-field-toggle" disabled={disabled}
          aria-label={showPassword ? 'Hide password' : 'Show password'}
          onClick={() => setShowPassword((v) => !v)}>
          <Icon name={showPassword ? 'eyeOff' : 'eye'} size={20} />
        </Button>
      )}
    </div>
  );
});

// ---- Select ---------------------------------------------------------------
export function Select({ size = 'md', ariaLabel, className, style, children, ...rest }) {
  return (
    <div className={cx('qc-field', 'qc-field--select', `qc-field--${size}`, className)} style={style}>
      <select aria-label={ariaLabel} {...rest}>{children}</select>
      <span className="qc-field-suffix"><Icon name="chevDown" size={20} /></span>
    </div>
  );
}

// ---- Badge / StatusBadge --------------------------------------------------
export function Badge({ color = 'brand', size = 'md', className, children, ...rest }) {
  return (
    <span className={cx('qc-badge', `qc-badge--${size}`, `qc-c-${color}`, className)} {...rest}>
      {children}
    </span>
  );
}
export function StatusBadge({ color = 'gray', size = 'md', children, ...rest }) {
  return (
    <Badge color={color} size={size} {...rest}>
      <span className="qc-status-dot" />
      {children}
    </Badge>
  );
}

// ---- Tabs -----------------------------------------------------------------
// Buttons only: every caller renders its panel itself.
//
// Arrow keys move between tabs and select as they go. A tablist puts only the
// selected tab in the tab order (the roving tabindex below), so without this a
// keyboard could reach the current tab and never leave it — the DS shipped the
// roving tabindex with no key handling at all. These tabs select on click, so
// following focus with a click keeps one activation path.
const TAB_STEP = { ArrowLeft: -1, ArrowRight: 1 };

export function Tabs({ className, children, ...rest }) {
  const onKeyDown = (e) => {
    if (!(e.key in TAB_STEP) && e.key !== 'Home' && e.key !== 'End') return;
    const tabs = [...e.currentTarget.querySelectorAll('[role="tab"]:not([disabled])')];
    const i = tabs.indexOf(document.activeElement);
    if (i < 0) return;
    e.preventDefault();
    const next = e.key === 'Home' ? 0
      : e.key === 'End' ? tabs.length - 1
        : (i + TAB_STEP[e.key] + tabs.length) % tabs.length;
    tabs[next].focus();
    tabs[next].click();
  };
  return (
    <div className={cx('qc-tabs', className)} role="tablist" onKeyDown={onKeyDown} {...rest}>
      {children}
    </div>
  );
}
export function TabButton({ selected, className, children, ...rest }) {
  return (
    <button type="button" role="tab" aria-selected={!!selected} tabIndex={selected ? 0 : -1}
      className={cx('qc-tab', selected && 'is-active', className)} {...rest}>
      {children}
    </button>
  );
}

// ---- Progress -------------------------------------------------------------
export function Progress({ value = 0, color = 'brand', size = 'md', className, style }) {
  const pct = Math.min(100, Math.max(0, Number(value) || 0));
  return (
    <div className={cx('qc-progress', `qc-progress--${size}`, className)} style={style}>
      <div className={cx('qc-progress-track', `qc-c-${color}`)} role="progressbar"
        aria-valuemin={0} aria-valuemax={100} aria-valuenow={pct} aria-valuetext={`${pct}%`}>
        <div className="qc-progress-fill" style={{ width: `${pct}%` }} />
      </div>
    </div>
  );
}

// ---- Sparkline ------------------------------------------------------------
// Same 100×40 viewBox and the same point mapping as the DS, so a series drawn
// before and after this change traces the same line.
function toSvgPoints(values) {
  if (values.length < 2) return '';
  const min = Math.min(...values);
  const max = Math.max(...values);
  return values
    .map((v, i) => `${(i / (values.length - 1)) * 100},${min === max ? 20 : (1 - (v - min) / (max - min)) * 40}`)
    .join(' ');
}
export function Sparkline({ values = [], color = 'brand', variant = 'line', className }) {
  const points = values.length >= 2 ? toSvgPoints(values) : null;
  // The gradient is referenced by id, so each instance needs its own.
  const gid = useId();
  return (
    <svg className={cx('qc-sparkline', `qc-c-${color}`, className)} viewBox="0 0 100 40"
      preserveAspectRatio="none" aria-hidden="true">
      {points && variant === 'gradient' && (
        <>
          <defs>
            <linearGradient id={gid} x1="0" y1="0" x2="0" y2="1.5">
              <stop offset="0%" />
              <stop offset="100%" />
            </linearGradient>
          </defs>
          <polygon points={`${points} 100,40 0,40`} fill={`url(#${gid})`} />
        </>
      )}
      {points && variant === 'filled' && <polygon className="fill" points={`${points} 100,40 0,40`} />}
      {points && <polyline points={points} />}
    </svg>
  );
}

// ---- Splitter -------------------------------------------------------------
// The geometry lives in ./split.js. `split` is space-separated ("64% 36%") and
// the declared shares are what render: the DS parsed it identically but skipped
// the recompute whenever the pane list changed in the same update — always true
// under React — so its panes always came out 50/50 whatever the markup asked
// for. Panes are declared as <SplitterPane> children; this component reads
// their props and draws the boxes and the dividers itself.
export function SplitterGroup({
  direction = 'horizontal', resizable = false, split = '', collapseBelow = '640px',
  className, style, children,
}) {
  const panes = Children.toArray(children)
    .filter((c) => c && c.props)
    .map((c) => ({
      node: c,
      collapsed: !!c.props.collapsed,
      minSize: c.props.minSize ?? 10,
      maxSize: c.props.maxSize ?? 90,
    }));
  const [sizes, setSizes] = useState(() => parseSplit(split, panes.length));
  const [stacked, setStacked] = useState(false);
  const wrapRef = useRef(null);
  const dragRef = useRef(null);
  // Re-derive the split when the declaration or the pane count changes; a drag
  // owns the sizes from then on.
  const shape = `${split}|${panes.length}`;
  const shapeRef = useRef(shape);
  if (shapeRef.current !== shape) {
    shapeRef.current = shape;
    setSizes(parseSplit(split, panes.length));
  }

  useEffect(() => {
    const el = wrapRef.current;
    const threshold = parseFloat(collapseBelow);
    if (!el || direction === 'vertical' || !String(collapseBelow).trim().endsWith('px')
      || !Number.isFinite(threshold)) return undefined;
    const ro = new ResizeObserver(([entry]) => {
      if (entry) setStacked(entry.contentRect.width <= threshold);
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, [collapseBelow, direction]);

  const layout = layoutOf(sizes.length === panes.length ? sizes : parseSplit(split, panes.length), panes);

  const onPointerMove = useCallback((e) => {
    const d = dragRef.current;
    const el = wrapRef.current;
    if (!d || !el) return;
    const containerSize = d.vertical ? el.clientHeight : el.clientWidth;
    if (containerSize === 0) return;
    const deltaPx = d.vertical ? e.clientY - d.startY : e.clientX - d.startX;
    const delta = (deltaPx / containerSize) * 100;
    const [newA, newB] = constrainPair(d.startA, delta,
      { a: d.boundsA, b: d.boundsB, total: d.startA + d.startB });
    setSizes((prev) => {
      const next = [...prev];
      next[d.index - 1] = (newA * d.activeSum) / 100;
      next[d.index] = (newB * d.activeSum) / 100;
      return next;
    });
  }, []);

  const endDrag = useCallback(() => { dragRef.current = null; }, []);

  useEffect(() => {
    window.addEventListener('pointermove', onPointerMove);
    window.addEventListener('pointerup', endDrag);
    window.addEventListener('pointercancel', endDrag);
    return () => {
      window.removeEventListener('pointermove', onPointerMove);
      window.removeEventListener('pointerup', endDrag);
      window.removeEventListener('pointercancel', endDrag);
    };
  }, [onPointerMove, endDrag]);

  const startDrag = (e, i) => {
    e.preventDefault();
    const activeSum = panes.reduce((s, p, idx) => (p.collapsed ? s : s + sizes[idx]), 0);
    dragRef.current = {
      index: i, startX: e.clientX, startY: e.clientY, activeSum,
      vertical: direction === 'vertical',
      startA: layout[i - 1], startB: layout[i],
      boundsA: paneBounds(panes[i - 1]), boundsB: paneBounds(panes[i]),
    };
    // Hold the node, not the event: React clears currentTarget as soon as the
    // handler returns, so reading it from the pointerup listener below left the
    // divider highlighted for the rest of the session.
    const divider = e.currentTarget;
    divider.classList.add('is-dragging');
    const clear = () => {
      divider.classList.remove('is-dragging');
      window.removeEventListener('pointerup', clear);
      window.removeEventListener('pointercancel', clear);
    };
    window.addEventListener('pointerup', clear);
    window.addEventListener('pointercancel', clear);
  };

  return (
    <div ref={wrapRef} style={style}
      className={cx('qc-splitter', `qc-splitter--${direction}`, stacked && 'qc-splitter--stacked', className)}>
      {panes.map((p, i) => {
        const hidden = i > 0 && (panes[i - 1].collapsed || p.collapsed);
        const disabled = !resizable || hidden;
        return (
          <Fragment key={i}>
            {i > 0 && !hidden && (
              <div className={cx('qc-splitter-divider', disabled && 'is-disabled')} role="separator"
                tabIndex={disabled ? undefined : 0}
                aria-orientation={direction === 'vertical' ? 'horizontal' : 'vertical'}
                aria-valuenow={Math.round(layout[i - 1])}
                onPointerDown={disabled ? undefined : (e) => startDrag(e, i)} />
            )}
            <div className={cx('qc-splitter-pane', p.collapsed && 'is-collapsed',
              p.node.props.variant === 'solid' && 'qc-splitter-pane--solid')}
              style={{ '--qc-pane-grow': layout[i] }}>
              {p.node.props.children}
            </div>
          </Fragment>
        );
      })}
    </div>
  );
}
// SplitterGroup reads its panes' props and renders the boxes itself, so the
// element is only ever a carrier for them.
export function SplitterPane() { return null; }

// ---- Dialog ---------------------------------------------------------------
// size: sm | md | lg | xl. Escape, the backdrop and the close button all call
// onClose, matching what the DS's `open-changed` event reported.
const FOCUSABLE = 'a[href],button:not([disabled]),input:not([disabled]),'
  + 'select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])';

export function Dialog({ size = 'md', onClose, showCloseButton = true, header, footer, children }) {
  const boxRef = useRef(null);
  // The DS generated an id for its header slot and pointed aria-labelledby at
  // it; without that a screen reader announces an unnamed modal.
  const headerId = useId();

  // Move focus in, and give it back to whatever opened the dialog. The DS put
  // focus on the dialog container too; losing that would leave the keyboard
  // behind the modal, on a tree these dialogs cover to confirm a delete.
  useEffect(() => {
    const opener = document.activeElement;
    boxRef.current?.focus();
    return () => { if (opener instanceof HTMLElement && opener.isConnected) opener.focus(); };
  }, []);

  // Escape closes; Tab stays inside. aria-modal claims the rest of the page is
  // inert, so Tab must not walk out of it — the DS asserted the same attribute
  // without keeping that promise.
  useEffect(() => {
    const onKey = (e) => {
      if (e.key === 'Escape') {
        if (!onClose) return;
        e.stopPropagation();
        onClose();
        return;
      }
      // Defer to anything that has already claimed the key: the exec terminal
      // sends Tab to the remote shell for completion, and xterm preventDefaults
      // it on its own textarea before this bubbles up to the document.
      if (e.key !== 'Tab' || e.defaultPrevented || !boxRef.current) return;
      const stops = [...boxRef.current.querySelectorAll(FOCUSABLE)]
        .filter((el) => el.getClientRects().length > 0);
      if (!stops.length) { e.preventDefault(); boxRef.current.focus(); return; }
      const first = stops[0];
      const last = stops[stops.length - 1];
      const on = document.activeElement;
      if (e.shiftKey && (on === first || on === boxRef.current)) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && on === last) { e.preventDefault(); first.focus(); }
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [onClose]);

  return (
    <>
      <div className="qc-dialog-backdrop" aria-hidden="true" />
      <div className="qc-dialog-wrapper"
        onClick={(e) => { if (e.target === e.currentTarget && onClose) onClose(); }}>
        <div ref={boxRef} className={cx('qc-dialog', `qc-dialog--${size}`)}
          role="dialog" aria-modal="true" tabIndex={-1}
          aria-labelledby={header ? headerId : undefined}>
          {showCloseButton && onClose && (
            <Button variant="ghost" size="sm" icon className="qc-dialog-close"
              aria-label="Close" onClick={onClose}>
              <Icon name="close" size={20} />
            </Button>
          )}
          {header && <div className="qc-dialog-header" id={headerId}>{header}</div>}
          <div className="qc-dialog-body">{children}</div>
          {footer && <div className="qc-dialog-footer">{footer}</div>}
        </div>
      </div>
    </>
  );
}
