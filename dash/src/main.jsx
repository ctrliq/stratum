import { createRoot } from 'react-dom/client';
import './theme.css';        // --quantic-* design tokens (dark theme), first-party
import './fonts/inter.css';  // @font-face for the UI font
import './ui/controls.css';  // the controls, on those tokens
import './app.css';          // token overrides + layout — last, so it wins
import { App } from './app.jsx';

// Boot (config → session → cluster) happens inside <App/> so the
// router can send an unauthenticated visitor to /login before anything else.
createRoot(document.getElementById('root')).render(<App />);
