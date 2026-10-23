// Colorized JSON/YAML surfaces — one read-only, one editable, sharing a single
// palette and a single tokenizer so the text is coloured identically in both.
//
//   CodeView   — read-only. Serializes an object to JSON/YAML and paints the
//                tokens as coloured spans in a line-numbered <pre>.
//   CodeEditor — editable. The same tokens are painted in a layer sitting exactly
//                behind a transparent <textarea>. Same PALETTE, same tokenizer,
//                same colours — only the surrounding chrome differs.
import { useState, useMemo, useRef, useLayoutEffect, Fragment } from 'react';
import { Button } from './controls.jsx';

// ---- palette: the single source of truth for both renderers ----------------
// Dark-theme tuned. The viewer and the editor overlay read the same numbers, so
// their colours cannot drift.
const PALETTE = {
  key: [124, 196, 255],   // property names — blue
  str: [95, 205, 138],    // strings — green
  num: [232, 184, 75],    // numbers — amber
  lit: [199, 146, 234],   // true / false / null — purple
  punct: [122, 128, 138], // structural punctuation — dim
};
const cssOf = (t) => (PALETTE[t] ? `rgb(${PALETTE[t].join(',')})` : 'inherit');

// ---- minimal object -> YAML (no dependency) -------------------------------
function yamlScalar(v) {
  if (v === null || v === undefined) return 'null';
  if (typeof v === 'boolean' || typeof v === 'number') return String(v);
  const s = String(v);
  if (s === '') return "''";
  if (s.includes('\n')) return JSON.stringify(s); // multi-line -> quoted flow scalar
  if (/^(true|false|null|yes|no|~)$/i.test(s) || /^[\d.+-]/.test(s)
    || /[:#[\]{}&*!|>'"%@`,]/.test(s) || /^\s|\s$/.test(s)) return JSON.stringify(s);
  return s;
}
function toYaml(v, ind = 0) {
  const pad = '  '.repeat(ind);
  if (v === null || v === undefined || typeof v !== 'object') return yamlScalar(v);
  if (Array.isArray(v)) {
    if (!v.length) return '[]';
    return v.map((item) => {
      if (item !== null && typeof item === 'object' && (Array.isArray(item) ? item.length : Object.keys(item).length)) {
        const lines = toYaml(item, ind + 1).split('\n');
        const first = lines[0].slice((ind + 1) * 2);
        return pad + '- ' + first + (lines.length > 1 ? '\n' + lines.slice(1).join('\n') : '');
      }
      return pad + '- ' + yamlScalar(item);
    }).join('\n');
  }
  const keys = Object.keys(v);
  if (!keys.length) return '{}';
  return keys.map((k) => {
    const val = v[k];
    if (val !== null && typeof val === 'object' && (Array.isArray(val) ? val.length : Object.keys(val).length)) {
      return pad + k + ':\n' + toYaml(val, ind + 1);
    }
    return pad + k + ': ' + yamlScalar(val);
  }).join('\n');
}

// ---- tokenizers -----------------------------------------------------------
// Each returns [{ t, s }] per line and is LOSSLESS: the concatenated `s` values
// rebuild the line exactly. The editor overlay depends on that — a single added
// or dropped character would slide the highlight out from under the caret.
const JSON_RE = /("(?:\\.|[^"\\])*"\s*:?|\b(?:true|false|null)\b|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)/g;
function tokenizeJsonLine(line) {
  const out = [];
  let last = 0;
  let m;
  JSON_RE.lastIndex = 0;
  while ((m = JSON_RE.exec(line)) !== null) {
    if (m.index > last) out.push({ t: 'plain', s: line.slice(last, m.index) });
    const v = m[0];
    if (v.startsWith('"')) {
      if (/:\s*$/.test(v)) {                            // "key":
        const q = v.slice(0, v.lastIndexOf('"') + 1);
        out.push({ t: 'key', s: q }, { t: 'punct', s: v.slice(q.length) });
      } else {
        out.push({ t: 'str', s: v });                   // string value
      }
    } else {
      out.push({ t: /^(true|false|null)$/.test(v) ? 'lit' : 'num', s: v });
    }
    last = m.index + v.length;
  }
  if (last < line.length) out.push({ t: 'plain', s: line.slice(last) });
  return out;
}
function scalarTokens(v) {
  if (v === '') return [];
  if (/^(true|false|null|~)$/i.test(v)) return [{ t: 'lit', s: v }];
  if (/^-?\d+(\.\d+)?$/.test(v)) return [{ t: 'num', s: v }];
  if (/^[|>]/.test(v)) return [{ t: 'punct', s: v }];   // block-scalar indicator
  return [{ t: 'str', s: v }];                          // string (quoted or bare)
}
function tokenizeYamlLine(line) {
  const kv = line.match(/^(\s*(?:-\s+)?)([^\s:][^:]*?)(:)(\s*)(.*)$/);
  if (kv) {
    const [, pre, key, colon, sp, val] = kv;
    const out = [];
    for (const part of pre.split(/(-)/)) {              // colour the dash, keep the spaces
      if (part) out.push({ t: part === '-' ? 'punct' : 'plain', s: part });
    }
    out.push({ t: 'key', s: key }, { t: 'punct', s: colon });
    if (sp) out.push({ t: 'plain', s: sp });
    return out.concat(scalarTokens(val));
  }
  const li = line.match(/^(\s*)(-\s+)(.*)$/);
  if (li) {
    const out = [];
    if (li[1]) out.push({ t: 'plain', s: li[1] });
    out.push({ t: 'punct', s: '-' }, { t: 'plain', s: li[2].slice(1) });
    return out.concat(scalarTokens(li[3]));
  }
  return scalarTokens(line);
}
const tokenize = (text, fmt) =>
  text.split('\n').map(fmt === 'yaml' ? tokenizeYamlLine : tokenizeJsonLine);

// ---- renderer 1: a line-numbered, coloured read-only view ------------------
export function CodeView({ obj, height = 420 }) {
  const [fmt, setFmt] = useState('yaml');
  const tokenLines = useMemo(
    () => tokenize(fmt === 'yaml' ? toYaml(obj) : JSON.stringify(obj, null, 2), fmt),
    [obj, fmt],
  );
  return (
    <div className="qc-codeview" style={{ height }}>
      <div className="qc-codeview-toolbar">
        <span className="qc-codeview-count">
          {tokenLines.length.toLocaleString()} {tokenLines.length === 1 ? 'line' : 'lines'}
        </span>
        <div className="row" style={{ gap: 2 }}>
          {['yaml', 'json'].map((f) => (
            <Button key={f} size="sm" variant={fmt === f ? 'secondary' : 'ghost'} onClick={() => setFmt(f)}>
              {f.toUpperCase()}
            </Button>
          ))}
        </div>
      </div>
      <pre className="qc-codeview-output">
        {tokenLines.map((toks, i) => (
          <div key={i} className="qc-codeview-row">
            <span>{toks.map((tk, j) => <span key={j} style={{ color: cssOf(tk.t) }}>{tk.s}</span>)}</span>
          </div>
        ))}
      </pre>
    </div>
  );
}

// ---- renderer 2: CSS spans behind a transparent textarea -------------------
// The highlight layer and the textarea share the .code-edit-text class, so font,
// size, line-height and padding are defined once and cannot diverge — that is
// what keeps the colours sitting exactly under the caret. The textarea scrolls
// natively; the layer and gutter are translated to follow it.
export function CodeEditor({ value, onChange, height = 420, fmt = 'json', ariaLabel = 'Editor' }) {
  const taRef = useRef(null);
  const hlRef = useRef(null);
  const gutRef = useRef(null);
  const lines = useMemo(() => value.split('\n'), [value]);
  const tokenLines = useMemo(() => tokenize(value, fmt), [value, fmt]);

  const sync = () => {
    const ta = taRef.current;
    if (!ta) return;
    const shift = `translate(${-ta.scrollLeft}px, ${-ta.scrollTop}px)`;
    if (hlRef.current) hlRef.current.style.transform = shift;
    if (gutRef.current) gutRef.current.style.transform = `translateY(${-ta.scrollTop}px)`;
  };
  // Re-sync after a value change: replacing the text can move the scroll offset.
  useLayoutEffect(sync, [value]);

  return (
    <div className="code-edit" style={{ height }}>
      <div className="code-edit-gutter" aria-hidden="true">
        <div className="code-edit-gutter-inner" ref={gutRef}>
          {lines.map((_, i) => <span key={i}>{i + 1}</span>)}
        </div>
      </div>
      <div className="code-edit-hl" aria-hidden="true">
        <pre className="code-edit-text" ref={hlRef}>
          {tokenLines.map((toks, i) => (
            <Fragment key={i}>
              {toks.map((tk, j) => <span key={j} style={{ color: cssOf(tk.t) }}>{tk.s}</span>)}
              {i < tokenLines.length - 1 ? '\n' : null}
            </Fragment>
          ))}
        </pre>
      </div>
      <textarea
        ref={taRef} className="code-edit-text code-edit-input" value={value}
        onChange={(e) => onChange(e.target.value)} onScroll={sync}
        spellCheck="false" autoComplete="off" autoCorrect="off" autoCapitalize="off"
        wrap="off" aria-label={ariaLabel}
      />
    </div>
  );
}
