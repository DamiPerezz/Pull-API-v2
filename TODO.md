# TODO — Pull Events / Aurora Hall demo

Priority-ordered. First bug that surfaces is likely to be in the top block.

---

## P0 — verify next time you open the mobile app

- [ ] **Reload the mobile app WITHOUT logging out.** Should hit `/auth/verify-token`,
      get `{type:"jwt", claims:{...}}`, rehydrate `user`, then load Eventos.
      If it still shows nothing, `flyctl logs` for the last request-id.
- [ ] **EventoDetalle** — after the shape unwrap, verify image, name,
      date, description, ticket list, guest list list, group section
      all render.
- [ ] **Orders tab** — default filter is `status=pending`. Aurora demo has
      3 pending orders visible last check; if you see "no orders" try
      changing the filter chip to Confirmed or All.

## P1 — pending screens I never got to touch

- [ ] **EventoNuevo (crear evento)**
  - Backend endpoint: `POST /event/create-event-with-tickets` — DEPLOYED.
  - Payload contract: `{name, description, image, event_date, start_time,
    end_time, ticket_limit, dress_code, min_age, custom_location,
    ticket_types:[{name, price, quantity, benefits}], table_capacity?}`.
  - Response: `{success, event_id, event, ticket_types}`.
  - Not tested end-to-end from UI. Likely first bug: image upload flow
    (`POST /upload/event-image` is a stub — returns a placeholder
    Unsplash URL; not persisted).
- [ ] **EventoEditar (editar evento)**
  - Endpoint: `PUT /event/update-event/:eventId`.
  - Accepts subset of the create payload (any of name/description/image/
    dress_code/custom_location/status/deleted_at/min_age/ticket_limit/
    table_capacity/event_date+start_time+end_time).
  - Not tested from UI.
- [ ] **Borrar evento** — `DELETE /event/delete-event/:eventId`. Soft delete
      (`status="cancelled", deleted_at=now`). To undelete: `PUT` with
      `{status:"published", deleted_at:null}`.
- [ ] **TicketsGestion** — presumably lists/adds/edits/deletes tickets on
      an event. Endpoints deployed:
  - `GET /ticket-types/event/:eventId`
  - `POST /ticket-types/event/:eventId`
  - `PUT /ticket-types/:ticketTypeId`
  - `DELETE /ticket-types/:ticketTypeId` (soft delete: is_active=false).
- [ ] **EmpleadoNuevo (crear staff)** — I NEVER added `POST /employees/create`
      or `PUT/DELETE`. Currently we only have `GET /employees/employees`
      and `GET /employees/employees/:id`. Add these when someone actually
      tries to create staff.
- [ ] **ReservaDetalle** (individual order detail) — endpoint exists,
      shape not verified against UI.
- [ ] **GroupReservaDetalle** — shape unverified.
- [ ] **GuestListDetalle** — likely calls `GET /guest-lists/signup/:signupId`
      which already exists in `main.go:448`. Verify shape matches.
- [ ] **VIPListDetalle**, **VIPListNuevo** — VIP list flow is the "mesa"
      variant. `POST /guest-lists/types` already exists. Verify what the
      screens actually call vs. what's registered.
- [ ] **Scanner QR** — real test needs a real ticket QR. Buy a ticket
      via WebApp, receive PDF via Brevo, open PDF, scan QR with mobile
      Scanner tab. `POST /ticket-validation/validate-ticket` returns
      `{valid, message, ticket, event}` on success.

## P2 — polish / smells

- [ ] `PullMobileApp-GL` — `app/(tabs)/EventosList/index.js.backup` slipped
      into git. Delete it and force-push with clean history, or just
      delete it in a follow-up commit.
- [ ] `PullMobileApp-GL` — `expo.log` also crept in. `.gitignore` this.
- [ ] `PullWebApp-GL` — GitHub reports 62 dependabot alerts on dev branch
      (33 high). Run `npm audit fix` and PR.
- [ ] Add `.env.production` to `.gitignore` too and use `flyctl secrets`
      / EAS env variables for release builds. Right now it's committed
      because we needed the URL for EAS Build to read.
- [ ] `Pull-API-v2` doesn't have any CI. Add a GitHub Action for `go
      build ./...` + `go vet` on every PR.
- [ ] Test coverage in `Pull-API-v2` is zero. Not going to fix now, but
      note that this is why "the build passed locally" masked the
      `UseVIPListFlow` typo — pure syntactic build wasn't enough.

## P3 — legacy cleanup

- [ ] `Pull-API-Go` — LEGACY. Add a `DEPRECATED.md` at its root pointing
      here so nobody edits it by mistake. Not deleting because the HTML
      email templates are still valuable.
- [ ] `PullClientDashboard` — points at `api.pullevents.com` which isn't
      part of the Aurora demo. Either delete or wire it to `pull-api-v2-demo`
      and add it to the demo story.

## P4 — nice-to-have

- [ ] Move `.env.example` values into a Doppler / 1Password shared vault
      so the next dev doesn't have to ping the previous one for secrets.
- [ ] Seed script (currently orders/reservations get created ad-hoc from
      the WebApp). A `cmd/seed/main.go` that creates N pending orders,
      1 pending guest-list signup, 1 pending group reservation would make
      staff-side testing much faster.
- [ ] Fly.io auto-stop / auto-start settings — the demo machine occasionally
      stops after periods of no traffic. Verify `fly.toml [http_service]`
      has `auto_stop_machines = false` if we want it warm always.

---

## When you finish something above

Delete the checkbox. This file is the source of truth for "what's left" —
if it grows without shrinking, we're not actually making progress.
