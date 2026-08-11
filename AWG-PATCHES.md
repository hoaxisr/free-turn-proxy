# AWG Manager patches (based on upstream v2.1.1)

Forked for router-friendly **auto-only** VK Smart Captcha.

## Changes vs samosvalishe/free-turn-proxy v2.1.1

> Перенос 2.0.1 → 2.1.1 (2026-08-11). Апстрим вынес запуск клиента в
> `internal/session` и сам инжектит ручной решатель параметром `Deps.Solver`
> из `cmd/client/main.go` — наш пункт 5 сжался до «Solver=nil, если нет
> `-manual-captcha`». Флаг `-captcha-manual-fallback` убран: awg-manager его
> никогда не передавал (`buildClientArgs`: только авто-капча), а механизм
> «auto-раунды, потом браузер» жив и включается самим фактом переданного
> решателя (`manualFallback := c.manualSolve != nil && !c.manualOnly`).
> Остальные патчи легли на 2.1.1 без изменений.

> Перенос 1.8.0 → 2.0.1 (2026-07-29). Апстрим сам реализовал часть наших
> доработок, причём аккуратнее, — они убраны из форка:
> * ротация браузерных семейств (`-browser`, `browserprofile.Kind`) — апстрим
>   перешёл на персоны по `-platform` (desktop/mobile), семейство всегда Chrome;
> * checkbox → slider при show-type mismatch — апстримовый `escalate()`
>   доигрывает слайдер в той же сессии;
> * `status=BOT` / `ERROR_LIMIT` — разведены на `errCaptchaBot` и
>   `errCaptchaRateLimit` с fail-fast.
>
> Осталось нашим: пункты 1 (5 auto-раундов, глобальный мьютекс), 3 (host
> captcha lock, сгоревшая сессия), 4, 5, 6, 7.

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
   - `cmd/client/main.go` передаёт `Deps.Solver = nil`, если не задан `-manual-captcha`: без решателя `:8765` не поднимается
   - `-manual-captcha` = manual-only (legacy)

6. **Log timestamps in router TZ (`internal/tzfix`)**
   - awg-manager launches freeturn with `TZ=<POSIX string from Keenetic /etc/TZ>`, e.g. `MSK-3` — not an IANA name.
   - Go `time.Local` does not parse POSIX-TZ from env, and the router has no zoneinfo (embedded-tzdata does not help: tzdata has no zone named `MSK-3`), so `time.Local` stays UTC and stdlib-log timestamps lag by the zone offset.
   - `tzfix.Apply()` parses the first (std) offset from the POSIX string and sets `time.Local = time.FixedZone(...)`. Called first line in `cmd/client/main.go` and `cmd/server/main.go` (before logger/goroutines — `time.Local` write is a race otherwise).
   - Limitation: DST rules ignored (first offset only); fine for DST-free router TZs (Russia).
   - Changed files: `internal/tzfix/tzfix.go`, `internal/tzfix/tzfix_test.go`, `cmd/client/main.go`, `cmd/server/main.go`.

## Build

Роутерные бинари собирает `.github/workflows/release.yml` по тегу `vX.Y.Z-N`
(arm64 / mipsle-softfloat / mips-softfloat, `-trimpath`, `main.version=X.Y.Z-N`)
и кладёт в релиз вместе с `checksums.txt`. Эти же артефакты идут на зеркало
`repo.hoaxisr.ru/ft/<версия>/`, а их SHA256 — в `internal/freeturn/install.go`
awg-manager.

7. **VKCalls auth path (1.8.0-3, WDTT-inspired)**
   - Before legacy `calls.getAnonymousToken` (+ captcha), try `api.vk.me` flow:
     `auth.getAnonymToken` → `messages.getCallPreview` → `messages.getAnonymCallToken` → OK CDN → TURN
   - On failure (except terminal link errors) fall back to legacy auto-captcha
   - Env: `FREETURN_VK_AUTH_MODE=legacy` skips VKCalls
   - Works on Android/mobile too (same `vkauth.Client` path)
