# CLAUDE.md

Guidance for Claude Code when working in this workspace.

---

## READ THESE FIRST (in this order)

Before doing anything, load the state:

1. **[HANDOFF.md](HANDOFF.md)** — current state of the Aurora Hall demo,
   URLs, credentials, and every bug caught so far with root cause. Read
   this whole file at the start of a new session.
2. **[TODO.md](TODO.md)** — prioritized backlog. If the user says "keep
   going" or "next bug", pick from the top of the P0/P1 lists.
3. **[ARCHITECTURE.md](ARCHITECTURE.md)** — system map + multi-tenant
   model + which frontends hit which backend. Reference when a request
   surprises you.

If those docs feel stale or contradict the code, TRUST THE CODE and update
the docs after you resolve the confusion.

---

## Project inventory (as of 2026-07-08)

| Project | Type | Stack | Location | State |
|---|---|---|---|---|
| **Pull-API-v2** | Backend (ACTIVE) | Go 1.21, Gin | `/Pull-API-v2` | Deployed on Fly.io as `pull-api-v2-demo` |
| **PullWebApp-GL** | Customer web (ACTIVE) | React 19, Vite, TS | `/PullWebApp-GL` | Cloudflare Pages at `aurora-hall.pages.dev`. Branch: `dev`. |
| **PullMobileApp-GL** | Staff mobile (ACTIVE) | Expo 54, RN 0.81 | `/PullMobileApp-GL` | Runs in Expo Go; EAS build pending |
| Pull-API-Go | **LEGACY** — do not use | Go 1.24, Gin | `/Pull-API-Go` | Kept only for HTML email templates (v2 embeds them) |
| PullClientDashboard | **Not used in demo** | React 19 | `/PullClientDashboard` | Points at `api.pullevents.com` which is not part of Aurora Hall |
| Pull-Landing | Marketing (untouched) | React 19, Tailwind | `/Pull-Landing` | Clean repo |

**Cheat sheet — all three active pieces communicate through**:
```
https://aurora-hall.pages.dev/api/v1     (CF Pages Function proxy)
       ↓
https://pull-api-v2-demo.fly.dev/api/v1  (Fly.io backend)
```

---

## Build and run

### Backend (Go)

```bash
cd Pull-API-v2
go build ./...              # sanity check
go run main.go              # local dev on :8080
flyctl deploy --remote-only --strategy immediate   # deploy
flyctl logs --app pull-api-v2-demo | tail -60      # debug live
```

### Frontends

```bash
# Customer WebApp
cd PullWebApp-GL
npm install
npm run dev          # http://localhost:5173
npm run build

# Staff Mobile (Expo)
cd PullMobileApp-GL
npm install
npx expo start --tunnel  # scan QR from Expo Go
npx expo export --platform ios   # sanity build

# Landing (untouched, don't need to run for demo)
cd Pull-Landing && npm install && npm run dev
```

---

## Demo credentials

**Staff mobile / any staff endpoint**:
```
Email:    demo@aurorahall.com
Password: DemoStaff2026!
Role:     admin
```

**Customer WebApp**: no login required.

**Real secrets** (Fly.io, Cloudflare, Brevo, Supabase service keys): they
live in `Pull-API-v2/.env` (gitignored). Retrieve via
`flyctl secrets list --app pull-api-v2-demo` or ask a team member.

---

## Multi-tenant model (quick recap)

- **Central Supabase**: `venues`, encrypted venue DB credentials,
  platform-level rows.
- **Per-venue Supabase**: events, orders, tickets, staff, guest signups.
- **Routing**: `services.DB.Central()` vs `services.DB.ForVenue(venueID)`.
- The Aurora Hall demo uses the venue at
  `https://oqqhffxwiizukkevzkvz.supabase.co` (see `DEFAULT_SUPABASE_URL`).

More detail in [ARCHITECTURE.md](ARCHITECTURE.md).

---

## Path compatibility layers

Both frontends call **v1-shaped paths**; v2 translates them:

- `controllers/legacy_compat_controller.go` — WebApp v1 paths → v2 logic.
- `controllers/mobile_compat_controller.go` — Mobile v1 paths → v2 logic.

Whenever a request 404s, first check whether the path is registered in
`main.go` and mapped through one of these two files.

Common gotchas — see the full log in [HANDOFF.md](HANDOFF.md):
- App expects **bare arrays** for list endpoints (`/event/upcoming-events`,
  `/guest-lists/types/event/:eventId`). Wrapping in `{data:[...]}` or
  `{events:[...]}` breaks silently — the store receives an object and
  the UI shows empty.
- `/orders/venue` expects `{orders, pagination: {currentPage, totalPages,
  totalCount, hasMore, limit}}`. Don't return `{orders, count, page, limit}`.
- `/auth/verify-token` expects `{type:"jwt", claims:{employee_id,
  organization_id, venue_id, email, name, role, venue_name, venue_slug,
  venue_currency, use_vip_list_flow}}` for the mobile app to rehydrate.

---

## When you hit an error

1. `flyctl logs --app pull-api-v2-demo | tail -80` — find the 4xx/5xx.
2. Grep for the request-id in the error body to correlate.
3. If it's a Supabase 42703, a column doesn't exist. Use narrower
   `select` or fix the schema call.
4. If it's a 22P02 on an enum, you sent a value not in the enum
   (`reservation_status`: `pending|confirmed|closed|completed|cancelled`;
   `order_status`: `pending|processing|confirmed|failed|cancelled`).
5. After fixing, `go build ./...` locally, THEN
   `flyctl deploy --remote-only --strategy immediate`. Local `go build`
   catches most typos but Fly's remote builder catches everything (see
   the `UseVIPListFlow` vs `UseVipListFlow` incident in HANDOFF).

---

## Anti-patterns

- Don't touch `Pull-API-Go` files unless you're editing the HTML email
  templates. It's not deployed anywhere; changes go nowhere.
- Don't touch `PullClientDashboard` for the Aurora demo. Different flow.
- Don't `git add -A` in `Pull-API-v2` without confirming `.env` isn't
  staged. `.gitignore` covers it but be paranoid.
- Don't run `DELETE /event/delete-event/:id` on live demo events for
  smoke tests — I did it on Aurora Friday Nights once and had to `PUT`
  it back to life.

---

## Contact / ownership

Repo owner: `GreenLock-Cybersecurity` org on GitHub. Primary dev:
`diego.rodriguez@greenlock.tech`.
