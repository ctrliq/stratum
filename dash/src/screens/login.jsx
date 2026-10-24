// Login — the sign-in panel.
//
// The dashboard authenticates by ServiceAccount bearer-token passthrough:
// there are no dashboard accounts, no password to check and no IdP.
// You paste a token, the server asks the cluster whose it is, and from then on
// every request the dashboard makes on your behalf carries that token — so
// what you can see and change is exactly your own RBAC.
//
// POST /api/v1/login → { user, csrfToken } (the cookie, carrying the sealed
// token, is set by the server) → redirect to the route the user was heading
// for (router state.from) or /.
//
// The token is never held client-side: it goes straight into the POST and the
// field is cleared. Nothing writes it to localStorage.
import { useState } from 'react';
import { useNavigate, useLocation } from 'react-router-dom';
import { Button, TextInput } from '../ui/controls.jsx';
import { isSessionCurrent, login } from '../session.js';
import stratumLogoStacked from '../assets/stratum-logo-stacked.svg';

export function Login({ config, onSignedIn }) {
  const navigate = useNavigate();
  const location = useLocation();
  const [token, setToken] = useState('');
  const [err, setErr] = useState(null);
  const [busy, setBusy] = useState(false);
  const from = (location.state && location.state.from) || '/';

  const submit = async (e) => {
    if (e) e.preventDefault();
    if (!token.trim() || busy) return;
    setBusy(true); setErr(null);
    const r = await login(token.trim());
    setBusy(false);
    if (r.stale) return;
    if (!r.ok) {
      setErr(r.code === 'invalid_token' ? 'The cluster did not accept that token.'
        : r.status === 0 ? 'Cannot reach the dashboard server.' : (r.error || `Sign-in failed (HTTP ${r.status})`));
      return;
    }
    if (!isSessionCurrent(r.revision)) return;
    // Only on success: a rejected token stays in the field so a truncated
    // paste can be fixed without fetching another one.
    setToken('');
    if (onSignedIn) await onSignedIn(r.body && r.body.user, r.body && r.body.csrfToken, r.revision);
    if (!isSessionCurrent(r.revision)) return;
    navigate(from === '/login' ? '/' : from, { replace: true });
  };

  return (
    <main className="login-split">
      <section className="login-split-panel" aria-labelledby="login-split-title">
        <div className="login-split-content">
          <header className="login-split-brand">
            <h1 id="login-split-title"><img className="login-split-logo" src={stratumLogoStacked} alt="Stratum" /></h1>
            <p>Sign in with a Kubernetes token</p>
          </header>

          <form className="login-split-form" onSubmit={submit} aria-busy={busy}>
            <label className="login-split-field">
              <span>Service account token</span>
              <TextInput className="login-split-input" name="token" ariaLabel="Service account token"
                type="password" value={token} placeholder="eyJhbGciOi…" autoComplete="off" disabled={busy}
                onChange={(e) => setToken(e.target.value)} />
            </label>

            <p className="login-split-hint">
              Get one with <span className="mono">kubectl create token &lt;serviceaccount&gt; -n &lt;namespace&gt;</span>.
              The dashboard acts as you: it can do what your RBAC allows, and no more.
            </p>

            <div className="login-split-error" role="alert">{err}</div>

            <Button variant="primary" size="sm" grow="always" type="submit" disabled={busy || !token.trim()}>
              <span className="login-split-button-label">{busy ? 'Signing in…' : 'Sign in'}</span>
            </Button>
          </form>
          {config?.version && <p className="login-split-version">{config.version}</p>}
        </div>
      </section>

      <aside className="login-split-visual" aria-hidden="true">
        <div className="login-split-caption">
          <p>Stratum</p>
          <span>Your cluster, at a glance.</span>
        </div>
      </aside>
    </main>
  );
}
