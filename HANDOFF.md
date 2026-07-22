# HANDOFF — Pull Events / Aurora Hall demo

**Last updated:** 2026-07-08
**Purpose:** onboard a new dev (and their Claude) fast on the Aurora Hall demo state.

---

## TL;DR — where things stand

| Piece | State | URL |
|---|---|---|
| Backend `Pull-API-v2` | LIVE on Fly.io (`pull-api-v2-demo`, v47) | https://pull-api-v2-demo.fly.dev |
| WebApp customer | LIVE on Cloudflare Pages | https://aurora-hall.pages.dev |
| Mobile app staff (Expo Go) | Runs in dev, EAS build pending | `EXPO_PUBLIC_API_URL=https://aurora-hall.pages.dev/api/v1` |
| Backend Pull-API-Go (v1) | **LEGACY — DO NOT USE.** Kept only for HTML email templates | — |
| PullClientDashboard | Points to `api.pullevents.com` — **not used in demo** | — |
| Pull-Landing | Marketing site, untouched | — |

Reads all API traffic through `https://aurora-hall.pages.dev/api/v1` because:
1. Same origin as WebApp → no CORS.
2. Cloudflare Pages Function at `PullWebApp-GL/functions/api/[[path]].js`
   proxies to Fly.io — bypasses fly.dev DNS blocks on some networks.

---

## Demo credentials

**Staff mobile / dashboard**:
```
Email:    demo@aurorahall.com
Password: DemoStaff2026!
Role:     admin
Venue:    Aurora Hall
```

**Customer WebApp**: no auth — anonymous checkout.

**Fly.io / Cloudflare / Brevo / Supabase**: the actual keys live in
`Pull-API-v2/.env` (gitignored). See `Pull-API-v2/.env.example` for the
shape. To recover values in production: `flyctl secrets list --app pull-api-v2-demo`.

---

## Where each repo lives

| Path | GitHub | Branch |
|---|---|---|
| `Pull-API-v2/` | https://github.com/GreenLock-Cybersecurity/Pull-API-v2 | main |
| `PullWebApp-GL/` | https://github.com/GreenLock-Cybersecurity/PullWebApp-GL | **dev** |
| `PullMobileApp-GL/` | https://github.com/GreenLock-Cybersecurity/PullMobileApp-GL | main |
| `Pull-Landing/` | https://github.com/GreenLock-Cybersecurity/Pull-Landing | main |
| `Pull-API-Go/` | https://github.com/GreenLock-Cybersecurity/Pull-API-Go | main (**LEGACY**) |
| `PullClientDashboard/` | Not on GitHub. Not used by the demo. | — |

---

## Setup on a new machine

```powershell
# 1. Clone the 3 active repos
git clone https://github.com/GreenLock-Cybersecurity/Pull-API-v2.git
git clone -b dev https://github.com/GreenLock-Cybersecurity/PullWebApp-GL.git
git clone https://github.com/GreenLock-Cybersecurity/PullMobileApp-GL.git

# 2. Tools you'll need
#   - Go 1.21+                   (backend)
#   - Node 20+                   (frontends)
#   - flyctl                     https://fly.io/docs/hands-on/install-flyctl/
#   - gh                         (optional but handy)
#   - EAS / Expo CLI:  npm install -g eas-cli
#   - wrangler (Cloudflare):     npm install -g wrangler

# 3. Auth
flyctl auth login
gh auth login
wrangler login             # only if you'll deploy WebApp

# 4. Secrets — .env files are gitignored
#    - Copy Pull-API-v2/.env.example to Pull-API-v2/.env and fill.
#    - Values live in: flyctl secrets list --app pull-api-v2-demo
#    - Or ask another team member (they're in 1Password / your team vault).

# 5. Run backend locally (against real Supabase)
cd Pull-API-v2
go run main.go              # listens on :8080 by default

# 6. Run WebApp against the local backend
cd PullWebApp-GL
# temporarily edit .env: VITE_API_URL=http://localhost:8080/api/v1
npm install
npm run dev                 # http://localhost:5173

# 7. Run mobile app
cd PullMobileApp-GL
npm install
npx expo start --tunnel     # scan QR from Expo Go
# .env.production already points at aurora-hall.pages.dev/api/v1 (demo).
# For local dev, edit .env: EXPO_PUBLIC_API_URL=http://<your-lan-ip>:8080/api/v1
```

---

## How to redeploy

> **⚠️ 2026-07-22:** hay TRES entornos (ver `ENVIRONMENTS.md` en la raíz
> del workspace). PRODUCCIÓN 511: `bash scripts/deploy_prod.sh`. STAGING:
> `bash scripts/deploy_staging.sh`. Lo de abajo deploya la DEMO Aurora y
> ahora requiere `-c fly.demo.toml` explícito (fly.toml ya no existe).

**Backend (Fly.io) — demo Aurora**:
```bash
cd Pull-API-v2
go build ./...              # sanity check
flyctl deploy -c fly.demo.toml --remote-only --strategy immediate
```
Takes ~60–90s. If the machine ends up STOPPED after deploy, run
`flyctl machine start <machine-id> --app pull-api-v2-demo` — it happens
occasionally when the health check races the boot.

**WebApp (Cloudflare Pages)**:
Automatic on push to `dev` branch. If manual:
```bash
cd PullWebApp-GL
npm run build
wrangler pages deploy dist --project-name aurora-hall
```

**Mobile app (EAS Build → TestFlight)**:
See `PullMobileApp-GL/BUILD_INSTRUCTIONS.md` — full walkthrough.

---

## How to debug like Claude did in the last session

### Read Fly logs

```bash
# tail of last N lines, all traffic
flyctl logs --app pull-api-v2-demo | tail -60

# only 4xx / 5xx (with grep)
flyctl logs --app pull-api-v2-demo | tail -200 | grep -E "GIN.*(4[0-9]{2}|5[0-9]{2})" | grep -v 401

# specific request-id (found in error responses)
flyctl logs --app pull-api-v2-demo | grep "<request-id>"
```

### Verify a shape from the wire

```bash
API="https://aurora-hall.pages.dev/api/v1"
TOKEN=$(curl -s -X POST "$API/auth/login-staff" \
  -H 'Content-Type: application/json' \
  -d '{"email":"demo@aurorahall.com","password":"DemoStaff2026!"}' \
  | python -c "import sys,json; print(json.load(sys.stdin).get('token',''))")

# Then hit any endpoint:
curl -s -H "Authorization: Bearer $TOKEN" "$API/employees/employees" | python -m json.tool | head
```

### JWT inspect

```bash
echo "$TOKEN" | cut -d. -f2 | base64 -d 2>/dev/null | python -m json.tool
```
Should show `user_id, employee_id, email, name, venue_id, organization_id, role, type=venue_staff`.

---

## Bug hunt log (chronological)

Every finding in the last session, with root cause. If you see the same
symptom again you probably touched an adjacent handler and need the same fix.

### Backend

1. **`MobileApproveGroupReservation` 500** — sent `status="approved"` but the
   `reservation_status` enum only accepts `pending|confirmed|closed|completed|cancelled`.
   Fix at `Pull-API-v2/controllers/mobile_compat_controller.go:MobileApproveGroupReservation`.

2. **`GetVenuePendingSignups` 500** — filtered `guest_list_signups.venue_id`
   but the column doesn't exist. Fix: fetch venue's event ids first,
   `event_id in.(...)`. See `guestlist_controller.go`.

3. **`GetEventGuestLists` 404 "Event not found"** — read `c.Param("event_id")`
   but the route registers `:eventId`. Fix reads both.

4. **`MobileGetEmployees` returned null** — selected `role` (doesn't exist);
   real column is `role_id`. Also missing `deleted_at is null` filter.
   Fixed + added `role_id → role.name` lookup so the UI groups by role.

5. **JWT missing `organization_id/employee_id/email/name`** — login used
   `GenerateStaffTokenSimple` which took only ID+venue+role. Now uses the
   full `GenerateStaffToken` with a complete `Staff{}`. Also added
   `EmployeeID` as an alias of `UserID` in `JWTClaims` because the mobile
   app decodes `.employee_id`.

6. **`/event/upcoming-events` returned `{count, events}`** — the app expects
   a bare array (`response.data` mapped straight into store). Fixed.

7. **`/orders/venue` returned `{orders, count, page, limit}`** — the app
   expects `{orders, pagination: {currentPage, totalPages, totalCount, hasMore, limit}}`.
   Fixed with a separate count query for `totalCount`.

8. **`/guest-lists/types/event/:eventId` returned `{data:[...]}` wrapper** —
   the guestListService reads `response.data` as bare array. Fixed.

9. **`/event/get-event-details` returned `{event: {...}}` wrapper** — the
   eventService reads `response.data.name` etc. top-level. Fixed to bare
   event object.

10. **`/ticket-types/*` endpoints didn't exist** — GET/POST/PUT/DELETE all
    added in `mobile_compat_controller.go` (`MobileGetTicketTypesByEvent`,
    `MobileCreateTicketType`, `MobileUpdateTicketType`, `MobileDeleteTicketType`).

11. **Event CRUD endpoints didn't exist** — `MobileCreateEvent`,
    `MobileCreateEventWithTickets`, `MobileUpdateEvent`, `MobileDeleteEvent`
    added. Helpers `combineDateTime` (date + start_time + end_time → RFC3339
    with `-06:00` Guatemala; rolls over midnight when end < start) and
    `slugify` (deterministic slug).

12. **`verify-token` returned `{valid, staff, venue}`** — the mobile app
    looks for `response.data.type === 'jwt'` and reads `.claims`. Fixed
    to `{valid, type:'jwt', claims:{employee_id, email, organization_id,
    venue_id, venue_name, venue_slug, venue_currency, use_vip_list_flow,
    role, name}, staff, venue}`. Also `AuthenticateStaff` middleware now
    sets `name` in Gin context. The first deploy of this fix failed
    silently because the field is `venue.UseVipListFlow` (lowercase p),
    not `UseVIPListFlow`.

13. **EventoDetalle tickets always empty (mobile)** — `ticketTypeService.
    getTicketTypesByEvent` returned the bare array but both EventoDetalle
    and TicketsGestion did `response.data || []` on top (double unwrap →
    always `[]`). Service now wraps as `{success, data}` like
    guestListService.
    (2026-07-09, fixed in PullMobileApp-GL)

14. **`/ticket-types/event/:eventId` returned raw DB columns** — the app
    renders `available_quantity/initial_quantity/is_group/min_quantity/
    max_quantity/has_gender_pricing/male_price/female_price` and expects
    `benefits` as a string. Now the GET filters `is_active=true` (deleted
    types kept showing), lifts mobile extras persisted in `metadata`,
    runs `EnrichTicketTypes`, joins `benefits[]` to "a, b, c", and appends
    `event_vip_ticket_types` rows as `is_group=true` group tickets (the
    same Mesa Premium rows the WebApp sells). POST/PUT/DELETE now map
    `initialQuantity→quantity_total`, `minQuantity/maxQuantity→
    min_per_order/max_per_order`, benefits string→text[], gender pricing→
    `metadata`, and route `isGroup=true` writes to `event_vip_ticket_types`
    (PUT/DELETE fall through to that table when the id isn't in
    ticket_types).

15. **`create-event-with-tickets` never worked** — inserted `custom_location`,
    `organization_id`, `venue_id` (none exist in `events`; the venue DB is
    single-tenant and the column is `location`), nested tickets used the
    nonexistent `quantity` column and string `benefits`, and `status:"draft"`
    would hide the new event from the published-only lists. All fixed;
    creates as `published`. `update-event` had the same `custom_location`
    and `table_capacity` problems.

16. **`table_capacity` has no column in `events`** — it was synthesized as 0
    by `EnrichEvent`. Writes are now ignored (tables live in
    `event_vip_ticket_types`) and `get-event-details` reports the real
    count: sum of `event_vip_ticket_types.quantity_total`.

17. **Guest list types CRUD written against an imaginary schema** — real
    columns are `max_signups/current_signups/benefits/signup_start/
    signup_end` with NO `venue_id/updated_at/gender/entry_time/
    requires_approval/max_capacity/current_count`. Create 500'd
    (PGRST204), update/delete no-op'd twice over: fake columns AND
    `c.Param("id")` while the mobile route registers `:typeId` (same class
    as bug #3). Capacity checks read `current_count` (always 0) — the
    staff signup path reported "guest list is full" for every list
    (`0 >= 0`). All mapped to real columns; GET now also exposes
    `max_capacity/current_count` aliases because the mobile edit form
    reads those names.

18. **Notification poller multiplied per screen (mobile)** — every screen's
    `CustomHeader` bell mounted its own 30s `setInterval` + immediate
    fetch, and expo-router keeps stacked screens mounted → 2 requests/30s
    per stacked screen plus a burst on every navigation. Polling moved to
    a single module-level poller in `useNotificationStore`
    (`startPolling/stopPolling`, 10s throttle for mount bursts); logout
    stops it. Steady state now: 1 pair of requests per 30s total.

19. **Web (dev) reload logged staff out** — `expo-secure-store` throws on
    web, the catch swallowed it and the token was never persisted.
    `secureStorage` now falls back to AsyncStorage (localStorage) on web
    only; native keeps SecureStore.

20. **`use_vip_list_flow` inconsistent between login and reload (OPEN)** —
    the central `venues` row for Aurora Hall has `use_vip_list_flow=true`,
    so `verify-token` claims flip the staff app into VIP mode after a
    reload (individual-ticket sections hidden), while a fresh login builds
    `user` from the `employee` object (no flag → regular mode). The WebApp
    never sees the flag (legacy `custom_location` doesn't include it) and
    sells individual tickets, so `true` looks like stale config. Proposed
    fix (needs a human to run it — prod data):
    `PATCH <central>/rest/v1/venues?id=eq.8450e956-... {"use_vip_list_flow": false}`.

21. **Approving an order didn't create tickets or notify the buyer** —
    `MobileApproveOrder` just flipped status to confirmed. Now it runs the
    same pipeline as customer payment (ConfirmPayment): tickets created,
    email with QRs/PDF sent. `MobileRejectOrder` 500'd on the nonexistent
    `rejected_by` column — the acting staff id now goes into
    `cancellation_reason`.
    (2026-07-09)

22. **Approving a group reservation sent no payment-link email** — added
    `SendGroupReservationApproved` (embedded `group_reservation_approved.html`
    template) fired from `MobileApproveGroupReservation`, with the
    `/es/group/track/<code>` link, guest count and host-paid count. Also
    persists `approved_at`/`approved_by` now.

23. **The per-guest complete/pay endpoints didn't exist** — the WebApp
    guest-complete page calls `GET /group-reservations/guest/:guestId`,
    `POST .../complete`, `POST .../pay`, `POST .../verify-access-code`;
    none were registered (the whole "each member fills their data and pays"
    flow 404'd). Implemented in legacy_compat (mock payment in DEMO_MODE,
    updates `confirmed_guests`, next_action state machine). Note:
    `vip_list_guests` has NO verification_code column — the flow relies on
    unguessable guest UUIDs and `requires_access_code:false`.

24. **Tracking page showed Q0.00/NaN and never "approved"** — the page reads
    flat aliases on the reservation (`event_name`, `event_date`, `venue_name`,
    `guest_count`, `total_amount`, `pending_amount`, numeric `status_id`
    where 7/8 = approved) that the raw `vip_list_reservations` row doesn't
    have. `LegacyTrackGroupReservation` now enriches all of them plus
    per-guest aliases (`amount_due`, `host_pays`, `verification_code`).

25. **Resend fallback 422'd on every send** — tags with spaces/accents
    (e.g. event names) violate Resend's charset. Tags are now sanitized in
    the Resend path. (Brevo remains primary; note Brevo rejects sends from
    non-authorized IPs, so local-dev emails only work if you whitelist your
    IP at app.brevo.com/security/authorised_ips.)

26. **VIP list flow retired in the mobile app** — `use_vip_list_flow` is
    forced to `false` in authService (both login and verify-token paths) so
    stale venue config can't flip the staff UI into the old VIP-only mode.
    The central venues row still says `true` — see #20 for the pending
    data fix.

27. **Mobile UI sweep (2026-07-09, second pass)** — full staff-app flow now
    works end-to-end in the browser preview. Fixes found on the way:
    - `Alert.alert` is a NO-OP on RN-web → every approve/reject/delete
      confirm silently did nothing. Added `utils/webAlert.js` shim
      (window.confirm/alert on web only), imported from `app/_layout.js`.
    - Rehydrated sessions lost the staff name (avatar showed "U"):
      authService's verify-token mapping didn't copy `claims.name`.
    - `/orders/details/:orderId` now returns nested `user` and `event`
      objects (ReservaDetalle showed "Event Name/No email" placeholders).
    - Bookings → Group filter was ALWAYS empty: it reads
      `staff-notifications?type=group_reservation` but (a) the response key
      is `data` while the app reads `.notifications`, and (b) nothing ever
      inserted group_reservation notification rows. The endpoint now serves
      live `vip_list_reservations` rows shaped for the app
      (reservation_id/user_name/quantity/total + organizer aliases), and the
      generic path returns both `data` and `notifications` keys.
    - `/group-reservations/details/:id` returns a flat object under `data`
      (status_name, total_amount, pending_amount, guest_count, event/venue
      names, guests embedded) — GroupReservaDetalle showed "not found".
    - Guest List filter crashed (`data.filter is not a function`):
      getPendingSignups double-wraps the `{success,data,pagination}` body.
      Normalized in the mobile service.
    - GetVenuePendingSignups' enrichment silently 42703'd (selected
      `event_date` on events and `entry_benefit` on guest_list_types);
      fixed + added flat `guest_list_name`/`event_name` aliases.
    - Signup approve/reject/get/check-in/undo/batch: all filtered by the
      nonexistent `guest_list_signups.venue_id`, wrote nonexistent columns
      (`updated_at`, `checked_in`, `plus_ones_used`, `rejected_by/at`,
      `reject_reason`), and the mobile routes register `:signupId` while
      handlers read `c.Param("id")` (bug #3 pattern — third occurrence).
      All fixed; reject reason goes into `notes`. Approve → status
      `approved` + QR + confirmation email verified.

### WebApp (customer)

- Instagram optional field in all 3 flows (individual/group/list) with
  matching form-field styles (previously only styled on group).
- Group reservation name+description INSIDE "Configuración del Grupo"
  section, with sensible defaults.
- Group ticket prices Q400 M / Q250 F (was showing wrong single price).
- Removed duplicate "Reservas de Grupo" golden crown card in event
  detail — only the blue MESA PREMIUM stays.
- Group reservation email uses v1 `group_reservation_pending.html`
  template (was inline HTML, "se veía horrible").
- PDF attachment now delivered via Brevo — fixed `attachment` (singular)
  key on the Brevo payload and QR generation (8-bit NRGBA re-drawing
  because gofpdf doesn't accept 16-bit boombuler PNGs).
- Tracking link works — unified `management_code` and `payment_link_code`
  to a single `sharedCode` (12 chars) inserted as `GRP-<code>` into
  `reservation_number`.

---

## What's NOT tested yet (mobile)

Priority order for the next session — probably where the next bugs live:

- [ ] **EventoDetalle full render** with all 3 sections (ticket types,
      guest lists, group reservations) after the shape fixes.
- [ ] **Crear evento** from `+` in Eventos tab — end-to-end round trip
      into DB. `MobileCreateEventWithTickets` is deployed but no UI test.
- [ ] **Editar evento** — same.
- [ ] **Borrar evento** — I *did* soft-delete Aurora Friday Nights by
      accident during a smoke test and restored it via PUT
      `{status:"published", deleted_at:null}`. The flow works, but nobody
      has clicked the delete button from the UI yet.
- [ ] **EmpleadoNuevo / EmpleadoEditar** — routes might not exist yet on
      backend (`POST /employees/create`, `PUT /employees/:id`, `DELETE`).
- [ ] **ReservaDetalle** (individual order detail from mobile) —
      `MobileGetOrderDetails` exists but shape might not match.
- [ ] **GroupReservaDetalle** — same, exists but untested from UI.
- [ ] **GuestListDetalle**, **VIPListDetalle**, **VIPListNuevo** — probably
      hit endpoints that aren't wired yet. First failed request will tell
      us which.
- [ ] **Scanner QR** — need a real ticket QR to validate. Buy a ticket
      from the WebApp, get the PDF from email, then scan the QR from the
      mobile Scanner tab.
- [ ] **Push notifications** — cannot be tested in Expo Go by design.
      Need an EAS dev-client build to test. `POST /notifications/register-token`
      works but delivery requires the FCM/APNS chain.

---

## Anti-patterns / things to NOT do again

- Don't run `DELETE /event/delete-event/:id` on Aurora Friday Nights just
  to smoke test. Test against a throwaway event.
- Don't add a route to `setupMobileRoutes` without checking whether the
  same path is already registered elsewhere. Duplicate `GET
  /guest-lists/venue/:venueId/pending` panicked the boot and the machine
  ended stopped.
- Don't add ambitious columns to `select` on `events` (`select: "*"`
  works, but selecting fields like `entry_benefit` that don't exist on
  `guest_list_types` returns 42703 from PostgREST).
- When adding fields to the JWT, always regenerate a token and inspect
  it with `base64 -d | jq` — the first `verify-token` fix looked "ok"
  in logs but the JWT struct field name typo made deploy fail silently.

---

## Ownership / how to keep this doc alive

- **When you fix a bug**, add a numbered entry to the "Bug hunt log"
  section with the root cause. The value of this doc is that pattern —
  if we start writing "fixed misc bugs" nobody can navigate it.
- **When you finish a "not tested" item above**, delete the bullet from
  the checklist. The list is meant to shrink.
- **When you add a new frontend/backend/service**, add a row to the top
  "where things stand" table.
