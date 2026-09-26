# BDT Wallet — Mobile App (PWA)

A lightweight, installable web wallet. No native Android build tooling
needed to get something real on a phone today; a path to a real signed
`.apk` is at the bottom for when you want one.

## What this is

- Plain HTML/CSS/JS — no framework, no build step, no npm install
- Talks to the mobile API added at `internal/api/mobile.go`
  (`/api/v1/mobile/*`) — separate from the admin console, with real
  per-user auth (phone + PIN → session token), not the shared admin token
- Installable on Android today via "Add to Home Screen" (manifest.json +
  service-worker.js already wired up) — opens fullscreen, has an icon, no
  browser chrome. This is a real, working app experience, not a fake demo.

## 1. Apply the new database migration

This app needs the phone/PIN/session tables:

```bash
psql "$DATABASE_URL" -f ../../ledger/migrate-003-user-auth.sql
```

## 2. Point the app at your API

Open `app.js` and change this line near the top:

```js
: 'https://your-api-domain.example.com'; // <-- CHANGE THIS for production
```

If you're testing on the same machine as the API, this is skipped
automatically (it detects `localhost`/`127.0.0.1`).

## 3. Enable CORS-safe testing locally

The mobile API already sends CORS headers for `/api/v1/mobile/*` (see
`withMobileCORS` in `mobile.go`), so the app can be hosted on a different
origin than the API. For local testing, serve this folder with any static
server:

```bash
cd relayer/mobile-app
python3 -m http.server 5173
```

Then run your API server (`go run ./cmd/api`) alongside it, and open
**http://localhost:5173** in a phone's browser (same WiFi network — use your
computer's LAN IP, not `localhost`, from the phone) or Chrome DevTools'
device emulation on desktop.

## 4. Test the flow

1. Register with a phone number + PIN
2. Tap **Receive** — QR code + deposit address should load
3. Send a small BDT deposit from MetaMask to that address (needs the
   relayer daemon `go run ./cmd` running to detect it)
4. Balance updates within a few seconds
5. Register a second test account, **Send** between them (3 BDT fee)
6. **Withdraw** to an external address (10 BDT fee, needs the relayer
   daemon running to actually broadcast it)

## 5. "Add to Home Screen" (works today, no build needed)

On an Android phone, open the site in Chrome → menu (⋮) → **Add to Home
Screen**. This installs it as a real app icon that opens fullscreen. This
is a legitimate, common distribution method — many real products ship this
way and nothing more.

## 6. Wrapping into a real signed `.apk` (optional, later)

When you want an actual installable `.apk` file (e.g. for a Play Store
listing), use **Capacitor** — it wraps this exact web app in a thin native
shell, no rewrite needed. This requires Android Studio installed on your
own machine (not something I can run in this sandbox):

```bash
npm install -g @capacitor/cli
cd relayer/mobile-app
npm init -y
npm install @capacitor/core @capacitor/android
npx cap init "BDT Wallet" "com.yourname.bdtwallet" --web-dir .
npx cap add android
npx cap sync
npx cap open android
```

That last command opens Android Studio, where **Build → Generate Signed
Bundle / APK** produces the real `.apk`. This is a one-time setup; after
that, `npx cap sync` after any change to `index.html`/`app.js`/`style.css`
keeps the native shell in sync.

## 7. Passkey (WebAuthn) login — optional

Passkeys let a user log in with their device's fingerprint/face unlock
instead of typing a PIN. **This is additive** — phone+PIN (section 4 above)
keeps working exactly as before, with or without this enabled. Nobody can
ever be locked out because a passkey failed to set up or a device was lost.

### Setup

1. Apply the migration:
   ```bash
   psql "$DATABASE_URL" -f ../../ledger/migrate-004-passkeys.sql
   ```
2. In `relayer/go.mod`, `go-webauthn/webauthn` was added as a dependency.
   **Run this on your own machine (where Go is installed) before anything
   else** — this sandbox has no Go toolchain, so this dependency has not
   been compiled or tested by me:
   ```bash
   cd relayer
   go mod tidy
   go build ./...
   ```
   If `go build` shows an error inside `internal/api/passkey.go` or
   `internal/auth/webauthn.go` about a method signature not matching, it
   means the installed `go-webauthn` version's `User` or `Credential`
   interface differs slightly from what I wrote from memory — the compiler
   error will point at the exact line; check
   https://pkg.go.dev/github.com/go-webauthn/webauthn/webauthn for that
   version's exact field/method names and adjust. This is a normal
   "adapting to a library's real API" fix, not a sign anything is
   fundamentally wrong.
3. Set these two env vars (both required together, or leave both unset to
   keep passkeys disabled):
   ```
   WEBAUTHN_RPID=localhost
   WEBAUTHN_RPORIGIN=http://localhost:5173
   ```
   `WEBAUTHN_RPID` is the bare domain (no scheme/port). `WEBAUTHN_RPORIGIN`
   is the exact scheme+host+port the browser shows in its address bar. In
   production, set these to your real domain — e.g. `WEBAUTHN_RPID=wallet.
   example.com`, `WEBAUTHN_RPORIGIN=https://wallet.example.com`. Getting
   these wrong makes passkeys fail closed (safe) — not open.
4. Restart `go run ./cmd/api`. Its startup log will say either "passkey
   login enabled" or "passkey login disabled" — confirm it says enabled
   before testing.

### Testing

1. Log in normally with phone + PIN (passkeys must be added to an existing
   account, not used to create one — this avoids a whole class of
   "register with a stolen phone number" issues).
2. On the dashboard, tap **"🔑 Passkey যোগ করো"** — your device will prompt
   for fingerprint/face/PIN unlock. Confirm it.
3. Log out, then log back in — the phone number field should now show a
   **"🔑 Passkey দিয়ে লগইন করো"** button. Use it instead of typing the PIN.
4. **Test that PIN login still works too** — this is the important safety
   check. Log in with the PIN again to confirm the fallback path wasn't
   broken by any of this.

### What to watch for in the logs

A line like `SECURITY: passkey clone warning for phone=... credential=...`
means an authenticator's sign counter didn't advance as expected — possibly
(not certainly) a cloned credential. It's logged, not auto-blocked, since a
few legitimate authenticators don't implement counters. If you ever see
this for real, it's worth manually checking that account's recent activity.

## 8. Inbound SMS phone verification — optional, cost-free

Instead of paying to send an OTP, the user texts a short code from their own
phone to a fixed number; an Android device holding that SIM (any
SMS-forwarder app) POSTs the incoming message to your backend.

### Setup

1. Apply the migration:
   ```bash
   psql "$DATABASE_URL" -f ../../ledger/migrate-005-phone-verification.sql
   ```
2. Set two env vars:
   ```
   SMS_TARGET_NUMBER=+8801XXXXXXXXX
   SMS_WEBHOOK_SECRET=<a long random string, e.g. openssl rand -hex 32>
   ```
   Leaving `SMS_TARGET_NUMBER` empty disables the feature entirely — no
   partial/insecure state.
3. On the Android device with that SIM, install any SMS-forwarder app (many
   free ones on Play Store let you POST incoming SMS to a URL as JSON).
   Configure it to POST to:
   ```
   https://your-api-domain/api/v1/sms-webhook
   Header: X-Webhook-Secret: <the same SMS_WEBHOOK_SECRET>
   Body: {"from": "<sender>", "body": "<message text>"}
   ```
   (Exact config screen varies by app — look for "webhook URL" and "custom
   headers" or "custom body template" in its settings.)

### Security notes (read before relying on this)

- The webhook secret is the ONLY thing stopping a stranger from POSTing fake
  `{"from": "...", "body": "123456"}` payloads. Treat it like a password —
  long, random, not committed to git.
- A submitted code only gets **5 attempts** before it's locked out (see
  `maxMatchAttempts` in `internal/ledger/phone_verify_store.go`) — this is
  what stops someone from scripting a flood of webhook calls to brute-force
  a 6-digit code within its 5-minute window.
- This proves "this phone number can currently send SMS to our number" —
  it is a lightweight signal, not strong identity proof. It does not gate
  login or money movement in this codebase; it just sets
  `users.phone_verified`, which you can use for something later (e.g. higher
  withdrawal limits) if you choose to.

## Known limitations (be aware before real users)


- QR code library loads from a CDN (`cdnjs.cloudflare.com`) — fine for
  testing, consider self-hosting it for production so the app doesn't
  depend on a third party being up
- No "forgot PIN" flow yet — if a user forgets their PIN, there is currently
  no recovery path. Worth building before real users rely on this.
- Session tokens live in `localStorage` — fine for a wallet app (this is
  not the Claude Artifacts sandbox, it's your own real static site), but if
  you later add anything that renders arbitrary user-supplied HTML
  (comments, bios, etc.) anywhere in this app, sanitize it — an XSS
  vulnerability there could let an attacker steal a session token from
  `localStorage`.
