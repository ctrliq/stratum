// Package dash carries the built dashboard SPA into the stratum binary.
//
// It exists only because `//go:embed` cannot name a parent directory: the SPA
// sources and the Vite build live at dash/, so the embed directive has to live
// there too. Everything else about the dashboard — the server, the proxy, the
// session layer — is internal/dashboard. Nothing outside this module should
// import this package; it is not a supported API, and the one deliberate
// public surface remains pkg/cli.
//
// `-tags noui` swaps the embedded tree for an empty one so a Go-only build
// needs no Node and no dist. It is an escape hatch, not the default: the build
// produces dist itself (`make dash`, which `make build` and `make test` depend
// on), CI builds and tests the SPA on every PR, and `make smoke` refuses an
// artifact that does not carry it.
package dash
