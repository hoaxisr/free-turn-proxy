# AWG Manager patches (based on upstream v1.8.0)

Forked for router-friendly **auto-only** VK Smart Captcha.

## Changes vs samosvalishe/free-turn-proxy v1.8.0

1. **Auto orchestrator (WDTT-inspired, no WebView)**
   - 5 auto rounds per captcha challenge (`captchaAutoRounds`)
   - Each round rotates browser persona starting from `-browser`, then the other two
   - Fresh TLS client + `browser_fp` per round
   - **Global captcha mutex** on whole process (not per VK link): `multi(4)` had 4 parallel captcha slots

2. **Stronger Go solver**
   - 4 internal attempts per round (was 2)
   - BOT and `status=ERROR` retry with new identity instead of blind backoff
   - Checkbox BOT/ERROR → fallback to slider when settings available

3. **Captcha solver**
   - Checkbox `show_type=slider` / `status=BOT` → slider without "show type mismatch" dead-end
   - `ERROR_LIMIT` → backoff 2–5s and retry within round (not instant fail)
   - `getContent status=ERROR` → session burned: fail fast, request fresh VK challenge
   - **Host captcha lock** (`/tmp/freeturn-vk-captcha.lock`): one captcha slot across freeturn **processes** on the same router

4. **No fatal on cold start**
   - Exhausted auto captcha with 0 connected streams → 60s lockout + `CAPTCHA_WAIT_REQUIRED` (retry), not process kill

5. **Manual captcha disabled by default**
   - No `:8765` HTTP server unless `-captcha-manual-fallback` or `-manual-captcha`
   - `-manual-captcha` = manual-only (legacy)
   - `-captcha-manual-fallback` = auto rounds then browser on localhost (not recommended on Keenetic)

6. **Log timestamps in router TZ (`internal/tzfix`)**
   - awg-manager launches freeturn with `TZ=<POSIX string from Keenetic /etc/TZ>`, e.g. `MSK-3` — not an IANA name.
   - Go `time.Local` does not parse POSIX-TZ from env, and the router has no zoneinfo (embedded-tzdata does not help: tzdata has no zone named `MSK-3`), so `time.Local` stays UTC and stdlib-log timestamps lag by the zone offset.
   - `tzfix.Apply()` parses the first (std) offset from the POSIX string and sets `time.Local = time.FixedZone(...)`. Called first line in `cmd/client/main.go` and `cmd/server/main.go` (before logger/goroutines — `time.Local` write is a race otherwise).
   - Limitation: DST rules ignored (first offset only); fine for DST-free router TZs (Russia).
   - Changed files: `internal/tzfix/tzfix.go`, `internal/tzfix/tzfix_test.go`, `cmd/client/main.go`, `cmd/server/main.go`.

## Build

From awg-manager repo root:

```bash
./scripts/build-freeturn-client.sh        # all arches
./scripts/build-freeturn-client.sh arm64  # router aarch64
```

Output: `prebuilt/freeturn/client-linux-*` and `server-linux-*`

Version baked in: `1.8.0-3` (`main.version` ldflag)

7. **VKCalls auth path (1.8.0-3, WDTT-inspired)**
   - Before legacy `calls.getAnonymousToken` (+ captcha), try `api.vk.me` flow:
     `auth.getAnonymToken` → `messages.getCallPreview` → `messages.getAnonymCallToken` → OK CDN → TURN
   - On failure (except terminal link errors) fall back to legacy auto-captcha
   - Env: `FREETURN_VK_AUTH_MODE=legacy` skips VKCalls
   - Works on Android/mobile too (same `vkauth.Client` path)
