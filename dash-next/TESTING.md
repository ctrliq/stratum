# Testing

<!-- quantic-testing:start -->

`@ctrliq/quantic-playbooks` carries this: Deciding what to unit test versus what belongs in an end-to-end test in a project that uses Quantic, or reviewing whether an existing test reaches for the right level.

See `node_modules/@ctrliq/quantic-playbooks/playbooks/quantic-testing/playbook.md`.

<!-- quantic-testing:end -->

## Why no jsdom?

The Vitest environment is set to `node`. This means:

- `document`, `window`, and browser APIs are not available in tests
- `@testing-library/react` and similar DOM testing tools will not work
- Tests stay fast and deterministic — no synthetic browser environment

If you're tempted to mount a component in a test, ask first whether the logic you want to
verify can be extracted into a pure function and tested directly.

## Functional style

Prefer pure functions with explicit inputs and outputs. Avoid stateful classes and mutating
shared state in utilities. Side effects should be pushed to the edges (hooks, query functions)
and kept out of pure logic.

## Running E2E tests

If E2E tests are included, they live in `e2e/` and run with Playwright.
