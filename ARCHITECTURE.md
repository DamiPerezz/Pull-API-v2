# ARCHITECTURE — Pull Events

## System map

```
┌─────────────────────────────────────────────────────────────────────┐
│                            USERS                                    │
├──────────────┬────────────────────────┬─────────────────────────────┤
│ Customers    │ Venue staff (mobile)   │ Platform admins (unused)    │
└──────┬───────┴────────────┬───────────┴──────────────┬──────────────┘
       │                    │                          │
       ▼                    ▼                          ▼
┌──────────────┐    ┌──────────────────┐      ┌──────────────────────┐
│ PullWebApp   │    │ PullMobileApp-GL │      │ PullClientDashboard  │
│ -GL          │    │ (Expo/RN)        │      │ (React SPA)          │
│ (React SPA)  │    │                  │      │ NOT USED IN DEMO     │
│  Cloudflare  │    │  Expo Go / EAS   │      │                      │
│  Pages       │    │  build           │      │  Points at           │
│              │    │                  │      │  api.pullevents.com  │
└──────┬───────┘    └──────────┬───────┘      └──────────────────────┘
       │                       │
       │   /api/v1/*           │  /api/v1/*
       │                       │
       ▼                       ▼
┌──────────────────────────────────────────┐
│  Cloudflare Pages Function (proxy)       │
│  aurora-hall.pages.dev/api/[[path]]     │
│  PullWebApp-GL/functions/api/[[path]].js│
│  - Bypasses fly.dev DNS blocks           │
│  - Same origin as WebApp → no CORS       │
└──────────────┬───────────────────────────┘
               │
               ▼
┌──────────────────────────────────────────┐
│  Pull-API-v2  (Go/Gin)                   │
│  Fly.io app: pull-api-v2-demo            │
│  https://pull-api-v2-demo.fly.dev        │
│                                          │
│  - main.go: route wiring                 │
│  - controllers/                          │
│    - auth_controller.go                  │
│    - event_controller.go                 │
│    - order_controller.go                 │
│    - guestlist_controller.go             │
│    - viplist_controller.go               │
│    - legacy_compat_controller.go  ← webApp v1 paths
│    - mobile_compat_controller.go  ← mobile v1 paths
│    - ...                                 │
│  - services/                             │
│    - database_router.go (multi-tenant)   │
│    - jwt.go, email*.go, pdf.go, ...      │
└──────────────┬───────────────────────────┘
               │
       ┌───────┴───────┐
       ▼               ▼
┌──────────────┐   ┌──────────────┐
│ Central DB   │   │ Venue DB(s)  │
│ Supabase     │   │ Supabase     │
│              │   │              │
│ - venues     │   │ - events     │
│ - venue_db_  │   │ - orders     │
│   configs    │   │ - tickets    │
│   (encrypted │   │ - guest_list_│
│    creds)    │   │   signups    │
│ - platform_  │   │ - vip_list_  │
│   transactions │  │   reservations
│ - pull_staff │   │ - organization_│
│              │   │   workers    │
│              │   │              │
│ One instance │   │ ONE INSTANCE │
│              │   │ PER VENUE    │
└──────────────┘   └──────────────┘
```

Additionally:
- **Brevo** — transactional email (300/day free, no domain verification).
  Delivers order confirmations, PDFs with QR, group reservation
  confirmations, guest-list approvals.
- **Mock payment processor** — `services/mock_processor.go`, activated
  with `DEMO_MODE=true`. Simulates Stripe: `/orders/demo-checkout` UI +
  auto-confirm. No real card processing in the Aurora Hall demo.

---

## Multi-tenant explained

- **Central DB** owns `venues`, `venue_database_configs` (with AES-256-GCM
  encrypted Supabase credentials for each venue), and platform-level rows.
- **Each venue** has its own Supabase project. Events, orders, tickets,
  staff (`organization_workers`), guest list signups, etc. all live in
  the venue DB — never in the central one.
- Router: `services.DB.Central()` gets the central connection.
  `services.DB.ForVenue(venueID)` looks up the venue's config, decrypts,
  and returns a cached `*SupabaseClient` for that venue.
- Aurora Hall's venue DB is at `https://oqqhffxwiizukkevzkvz.supabase.co`
  (see `Pull-API-v2/.env`: `DEFAULT_SUPABASE_URL`).

---

## Which frontends use which API — CHEAT SHEET

| Frontend | Base URL | Backend it hits | Notes |
|---|---|---|---|
| PullWebApp-GL (`.env.production`) | `/api/v1` | Pull-API-v2 via CF Pages Function proxy | Same-origin. Prod. |
| PullMobileApp-GL (`.env.production`) | `https://aurora-hall.pages.dev/api/v1` | Same proxy | Prod for EAS builds. |
| PullMobileApp-GL (`.env` dev) | Whatever you set | Anywhere | Default in repo also points at demo. |
| PullClientDashboard | `https://api.pullevents.com/api/v1` | **Nothing — that URL doesn't answer** | NOT USED IN DEMO. Ignore. |
| Pull-Landing | n/a | n/a | Marketing site, no API. |

The takeaway: **only Pull-API-v2 is a live backend in the Aurora demo.**
`Pull-API-Go` (v1) and any `api.pullevents.com` endpoint are historical.

---

## Path compatibility layers

Pull-API-v2 speaks the "v2 REST" style natively but the two frontends were
built against **v1 paths** and never migrated. So we translate:

- `controllers/legacy_compat_controller.go` — maps every `Pull-API-Go` path
  that PullWebApp-GL still calls to v2 logic. Examples:
  - `POST /orders/create-pending-order`
  - `POST /orders/simulate-payment`
  - `POST /group-reservations/create`
  - `POST /guest-lists/signup`
  - `GET  /event/get-detailed-event-info/:slug`
- `controllers/mobile_compat_controller.go` — same but for
  `PullMobileApp-GL`. Adds staff-flow endpoints the WebApp doesn't need
  (approve/reject orders, group reservations, guest signups; CRUD for
  events, tickets, employees; push token register/unregister).

Both compat layers set claims from JWTs, call into `services.DB.ForVenue`,
enrich rows via `services/schema_compat.go`
(`EnrichEvent`/`EnrichTicketType`) so the frontends see the legacy field
names they expect (`event_date`, `start_time`, `end_time` vs. the DB's
`start_datetime`; `event_id`, `venue_id` populated at every level; etc.).

---

## JWT claims

Signed with `JWT_SECRET`. Encoded fields (as of v47):

```json
{
  "iss": "pull-api-v2",
  "exp": <unix>,
  "iat": <unix>,
  "user_id":         "5032b851-...",  // staff.id
  "employee_id":     "5032b851-...",  // alias for mobile app
  "email":           "demo@aurorahall.com",
  "name":            "Demo Admin",
  "venue_id":        "8450e956-...",
  "organization_id": "74f2fa79-...",
  "role":            "admin",
  "type":            "venue_staff"
}
```

Middleware sets on `gin.Context`: `user_id`, `staff_id`, `venue_id`,
`organization_id`, `role`, `email`, `name` + typed `staff` and `claims`.

`GET /auth/verify-token` echoes these back to the mobile app in the
`{type:"jwt", claims:{...}}` shape it expects for rehydration.

---

## Files that surprise people

- `Pull-API-v2/services/templates/*.html` — HTML email templates copied
  from `Pull-API-Go/templates/` and embedded via `//go:embed`. Kept in
  the v2 tree so the binary is self-contained. If you edit them, they
  ship with the next Fly deploy.
- `Pull-API-v2/cmd/hashpwd/` — throwaway CLI that generates a bcrypt
  hash. Used once to create the demo staff row; kept around for the
  next time.
- `Pull-API-v2/cmd/encrypt/` — same for AES-256-GCM (encrypts venue
  Supabase credentials before you INSERT them into
  `venue_database_configs.service_key_encrypted`).
- `PullWebApp-GL/functions/api/[[path]].js` — the Cloudflare Pages
  Function proxy. Deployed automatically with every WebApp build.
