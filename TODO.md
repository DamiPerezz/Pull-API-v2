# TODO — Pull Events / Aurora Hall demo

Priority-ordered. First bug that surfaces is likely to be in the top block.

---

## P0 — verify next time you open the mobile app

- [x] **Reload the mobile app WITHOUT logging out.** VERIFIED on expo-web
      (2026-07-09): rehydrates via verify-token and loads Eventos. BUT it
      surfaced an open issue: claims carry `use_vip_list_flow=true` (stale
      central config?) so the app flips to VIP mode after reload while a
      fresh login stays in regular mode. See HANDOFF bug #20 — decide and
      PATCH the central venues row.
- [x] **EventoDetalle** — VERIFIED (2026-07-09): image, name, date,
      description, tickets with availability, groups (Mesa Premium/VIP
      Lounge with M/F prices from event_vip_ticket_types) and guest lists
      all render. Required HANDOFF fixes #13-14.
- [ ] **Orders tab** — default filter is `status=pending`. Aurora demo has
      3 pending orders visible last check; if you see "no orders" try
      changing the filter chip to Confirmed or All.

## P1 — pending screens I never got to touch

- [~] **EventoNuevo (crear evento)** — backend contract VERIFIED via API
      (2026-07-09) after HANDOFF fix #15 (the endpoint had never worked:
      phantom columns). Creates as `published` now. UI wizard still not
      driven end-to-end: the required image picker opens a native file
      dialog (untestable from web preview).
      **CORRECTED (2026-08-18): `POST /upload/event-image` is NOT a stub** —
      that note was stale. It really uploads to the venue's `event-images`
      bucket via `SupabaseClient.UploadPublicObject` and returns the public
      URL. The only Unsplash URL left in the repo is the no-image fallback
      in `services/email.go`, which is unrelated.
      Twin endpoint for the venue photo: `POST /upload/venue-image`
      (`MobileUploadVenueImage`, admin|manager only, central `venue-images`
      bucket). ⚠️ Both hand back an `/object/public/` URL, so the bucket must
      be PUBLIC in that environment — prod is (the email logo lives there),
      but `storage_controller.go` treats the same bucket as private and signs
      URLs. If staging/demo has it private the upload still answers 200 and
      the photo renders broken: open the returned URL once per environment.
- [x] **EventoEditar (editar evento)** — VERIFIED from UI (2026-07-09) on a
      throwaway event: name/description/dress_code persisted via
      `PUT /event/update-event/:eventId`. Needed HANDOFF fixes #15-16
      (`custom_location→location`, `table_capacity` not a column). Note:
      in VIP mode the form requires tableCount > 0 and web's silent
      Alert.alert made validation failures invisible.
- [x] **Borrar evento** — VERIFIED via API (2026-07-09) on the throwaway
      event: soft delete works, list stays clean. Undelete: `PUT` with
      `{status:"published", deleted_at:null}`.
- [x] **TicketsGestion** — VERIFIED (2026-07-09): UI lists tickets with
      availability and the VIP pricing form saves; full CRUD (regular +
      group/VIP-table) exercised via API. Needed HANDOFF fixes #13-14.
- [ ] **EmpleadoNuevo (crear staff)** — I NEVER added `POST /employees/create`
      or `PUT/DELETE`. Currently we only have `GET /employees/employees`
      and `GET /employees/employees/:id`. Add these when someone actually
      tries to create staff.
- [ ] **ReservaDetalle** (individual order detail) — endpoint exists,
      shape not verified against UI.
- [ ] **GroupReservaDetalle** — shape unverified.
- [ ] **GuestListDetalle** — likely calls `GET /guest-lists/signup/:signupId`
      which already exists in `main.go:448`. Verify shape matches.
- [x] **Guest list types CRUD (Lists modal/screen)** — VERIFIED via the
      mobile endpoints (2026-07-09): POST/PUT/DELETE + GET aliases. The
      handlers were written against an imaginary schema and PUT/DELETE
      also read the wrong route param — see HANDOFF bug #17. GET rendering
      verified in UI (Latin Vibes shows both lists).
- [x] **Flujos de aprobación (2026-07-09)** — verificados end-to-end en local:
      order approve ahora corre el pipeline completo (tickets + email QR/PDF),
      order reject arreglado (columna rejected_by no existe), group approve
      envía email con el link de pago (`group_reservation_approved.html`),
      y los endpoints por-invitado (`/group-reservations/guest/:id`,
      `/complete`, `/pay`) se implementaron — no existían y la página web de
      "completar datos y pagar" estaba 404. Tracking page enriquecida
      (status_id 7, totales, aliases). Ver HANDOFF #21-25. **PENDIENTE:
      deploy a Fly + push de los repos.**
- [ ] **P0 dato central**: `venues.use_vip_list_flow=true` para Aurora es
      config stale — el producto retiró ese flujo. La app ya lo ignora
      (forzado false en authService), pero hay que corregir el dato:
      `PATCH <central>/rest/v1/venues?id=eq.8450e956-... {"use_vip_list_flow": false}`.
- [ ] ~~**VIPListDetalle**, **VIPListNuevo**~~ — RETIRADO (2026-07-09): el
      flujo VIP list ya no se usa como producto. Las pantallas VIPList* del
      móvil quedan muertas; candidatas a borrarse en una limpieza futura.
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
