# 0003. Strict-CSP, framework-free web test client

## Status

accepted

## Context

Chorus includes a web test client (`web/static/`) to exercise API endpoints, demonstrate client interactions, and facilitate manual and automated testing.

Single Page Application (SPA) frameworks (React, Vue, Angular) and heavy client build toolchains introduce significant dependency trees, complex build steps, and potential supply-chain vulnerabilities. Furthermore, web clients are prone to Cross-Site Scripting (XSS) if dynamic data is injected into HTML without strict boundaries.

## Decision

1. **Vanilla ES Modules without framework:**
   - The web client is written in standard vanilla JavaScript ES modules (`web/static/js/`), served directly without bundling or transpilation.
   - Tailwind CSS is compiled using the standalone Tailwind CLI; DaisyUI styles are included via committed assets.
   - Dynamic data from API responses must be inserted into the DOM using `textContent` only; `innerHTML` with unsanitized API strings is strictly avoided.
2. **Strict Content Security Policy (CSP):**
   - The server enforces a strict CSP header: `default-src 'self'`, `script-src 'self' https://accounts.google.com/gsi/client`, `style-src 'self' https://accounts.google.com/gsi/`, `img-src 'self' data:`, `font-src 'self'`, `connect-src 'self' https://accounts.google.com/gsi/`, `frame-src https://accounts.google.com/gsi/`, `object-src 'none'`, `base-uri 'none'`, `form-action 'self'`, `frame-ancestors 'none'`.
   - No inline scripts (`<script>...</script>`), inline event handlers (`onclick=...`), or inline style attributes (`style=...`) are permitted.
3. **Module boundary restrictions:**
   - Section modules import flat utility modules (`api.js`, `state.js`, `dom.js`) but never import `app.js`.
   - Web client scripts remain strictly within `web/` and do not leak into backend packages.

## Consequences

- Attack surface for XSS is minimized structurally via browser-enforced CSP and DOM textContent usage.
- Zero client build overhead (except Tailwind CSS compilation), simplifying development and CI pipelines.
- Test client remains a lightweight test harness rather than an elaborate SPA framework deployment.

## Enforced by

- `internal/http/middleware/securityheaders_test.go`: `TestSecurityHeaders_CSPDirectives`
- `web/client_guards_test.go`: `TestClient_NoInlineMarkupStylesOrScripts`, `TestClientFiles_UnderSizeBar`, `TestClientJS_ImportGraph`
- `internal/archtest/boundaries_test.go`: `TestWebClientImportsStayInWeb`
- `Makefile`: `test-js` (`node --test web/jstest/*.test.mjs`)
