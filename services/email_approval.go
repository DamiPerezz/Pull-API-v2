package services

import (
	"context"
	"fmt"
	"html"
)

// =============================================
// APPROVAL-FLOW EMAILS (private events)
//
// Flow (dLocal Go, "solicitar → aprobar → pagar"; see DLOCAL-FLUJO-PRIVADO.md):
//
//	1. The customer requests a spot WITHOUT paying — no card is asked for and
//	   no money moves. Order goes to `awaiting_approval`  → SendApprovalPending.
//	2. Staff approves → `approved_unpaid`. The customer gets a PAYMENT LINK
//	   and a deadline (24h by default)                    → SendApprovalApproved.
//	3. Staff rejects → nothing to refund, nothing was ever
//	   charged                                            → SendApprovalRejected(expired=false).
//	4. Nobody decides within 48h → the request expires    → SendApprovalRejected(expired=true).
//	5. Approved but not paid in time → the spot is freed  → SendApprovalPaymentExpired.
//
// IMPORTANT for the copy: there is NO hold/authorization/capture anymore. The
// customer is never charged until they pay themselves from the link, so no
// email may talk about "retención", "autorización" or "importe liberado".
//
// Visual system: shared dark theme (see email_theme.go), same language as the
// tickets_with_pdfs.html reference template. Banner color is semantic:
// amber = pending, green = approved, red = rejected/expired.
// =============================================

// Default windows for the copy when the caller does not pass one.
const (
	defaultApprovalDecisionHours = 48 // staff has this long to decide
	defaultApprovalPaymentHours  = 24 // approved customer has this long to pay
)

// ApprovalEmailData carries the venue/event context for status emails.
type ApprovalEmailData struct {
	CustomerName  string
	EventName     string
	EventImage    string
	EventDate     string
	EventTime     string
	VenueName     string
	VenueLocation string
	OrderNumber   string
	Total         string
	Currency      string
}

// approvalHero renders the event image (rounded, like the reference) when we
// have one.
func approvalHero(d ApprovalEmailData) string {
	if d.EventImage == "" {
		return ""
	}
	return fmt.Sprintf(
		`<table width="100%%" cellpadding="0" cellspacing="0" border="0" role="presentation" style="border-collapse:collapse;margin:0 0 24px;"><tr><td><img src="%s" alt="%s" width="504" style="border:0;border-radius:12px;display:block;outline:none;text-decoration:none;height:auto;width:100%%;max-width:100%%;font-size:13px;" /></td></tr></table>`,
		html.EscapeString(d.EventImage), html.EscapeString(d.EventName))
}

// approvalDetailsCard renders the accent-tinted "Event Details"-style card
// with the same fields the old table showed (Evento, Fecha, Hora, Lugar,
// Solicitud); empty fields are skipped.
func approvalDetailsCard(accentRGB string, d ApprovalEmailData) string {
	esc := html.EscapeString
	field := func(label, value string) string {
		if value == "" {
			return ""
		}
		return emailDetailField(label, esc(value))
	}
	inner := field("Evento", d.EventName) +
		field("Fecha", d.EventDate) +
		field("Hora", d.EventTime) +
		field("Lugar", firstNonEmpty(d.VenueLocation, d.VenueName)) +
		field("Solicitud", d.OrderNumber)
	return emailAccentCard(accentRGB, inner)
}

// approvalNoticeCard renders the highlighted box: small uppercase label, one
// big centered value, and an optional explanatory note underneath. bigValue
// and note are inline HTML (escape before calling).
func approvalNoticeCard(accentRGB, label, bigValue, note string) string {
	inner := fmt.Sprintf(
		`<div style="font-family:%s;font-size:11px;font-weight:600;letter-spacing:1.2px;line-height:1;text-align:center;text-transform:uppercase;color:#93939d;color:rgba(255, 255, 255, 0.55);margin-bottom:12px;">%s</div>%s`,
		emailFontStack, label, emailBigValue(bigValue))
	if note != "" {
		inner += fmt.Sprintf(
			`<div style="font-family:%s;font-size:13px;font-weight:400;line-height:1.6;text-align:center;color:#a8a8b2;color:rgba(255, 255, 255, 0.65);margin:12px 0 0;">%s</div>`,
			emailFontStack, note)
	}
	return emailAccentCard(accentRGB, inner)
}

// approvalAmountCard is approvalNoticeCard for a money value ("400.00 GTQ").
func approvalAmountCard(accentRGB, label, total, currency, note string) string {
	esc := html.EscapeString
	value := esc(total)
	if currency != "" {
		value += " " + esc(currency)
	}
	return approvalNoticeCard(accentRGB, label, value, note)
}

// approvalSteps renders a numbered "what happens next" list, table-based so it
// survives Outlook. Items are inline HTML (escape before calling).
func approvalSteps(steps ...string) string {
	rows := ""
	for i, s := range steps {
		rows += fmt.Sprintf(
			`<tr><td width="24" valign="top" style="padding:0 10px 10px 0;font-family:%s;font-size:14px;font-weight:700;line-height:1.6;color:#a78bfa;">%d.</td><td valign="top" style="padding:0 0 10px;font-family:%s;font-size:14px;font-weight:400;line-height:1.6;color:#bfbfc6;color:rgba(255, 255, 255, 0.75);">%s</td></tr>`,
			emailFontStack, i+1, emailFontStack, s)
	}
	return `<table width="100%" cellpadding="0" cellspacing="0" border="0" role="presentation" style="border-collapse:collapse;">` + rows + `</table>`
}

// approvalPayFallbackLink renders the "the button didn't work?" plain link.
func approvalPayFallbackLink(paymentURL string) string {
	esc := html.EscapeString(paymentURL)
	return emailFineprint(fmt.Sprintf(
		`¿No te funciona el botón? Copia y pega este enlace en tu navegador:<br /><a href="%s" style="color:#a78bfa;color:rgb(167, 139, 250);text-decoration:underline;word-break:break-all;">%s</a>`,
		esc, esc))
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// approvalHoursPhrase turns an hour count into copy ("24 horas"), falling back
// to the documented default when the caller passes 0.
func approvalHoursPhrase(hours, fallback int) string {
	if hours <= 0 {
		hours = fallback
	}
	if hours == 1 {
		return "1 hora"
	}
	return fmt.Sprintf("%d horas", hours)
}

// approvalRejectedHeading is the single source of truth for the heading (and
// subject) of the rejected / not-decided-in-time approval email.
func approvalRejectedHeading(expired bool) string {
	if expired {
		return "Solicitud caducada"
	}
	return "Solicitud no aceptada"
}

// approvalPaymentExpiredHeading is the heading (and subject) for the third
// negative case: approved, but the payment window ran out.
func approvalPaymentExpiredHeading() string {
	return "Se agotó el plazo para pagar"
}

// noChargeNote is the reassurance line shared by every negative email: in this
// flow the customer never handed over card details.
const noChargeNote = `Nunca te pedimos los datos de tu tarjeta, así que no hay nada que devolver.`

// BuildApprovalPendingEmail renders the "request received, nothing charged"
// email HTML. Exported so preview tooling can render it without sending.
func BuildApprovalPendingEmail(d ApprovalEmailData) string {
	esc := html.EscapeString
	decision := approvalHoursPhrase(defaultApprovalDecisionHours, defaultApprovalDecisionHours)

	body := emailGreeting("Hola ", esc(d.CustomerName)) +
		emailParagraph(fmt.Sprintf(
			`Hemos recibido tu solicitud para <strong style="color:#ffffff;">%s</strong>. Es un evento privado, así que el equipo de %s tiene que darle el visto bueno antes de nada.`,
			esc(d.EventName), esc(d.VenueName))) +
		approvalAmountCard(emailAccentAmber, "TOTAL SI TE APRUEBAN", d.Total, d.Currency,
			`Todavía <strong style="color:#ffffff;">no se te ha cobrado nada</strong>.`) +
		approvalHero(d) +
		approvalDetailsCard(emailAccentAmber, d) +
		emailInfoCard(
			emailCardLabel("Cómo sigue")+
				approvalSteps(
					fmt.Sprintf(`El equipo de %s revisa tu solicitud.`, esc(d.VenueName)),
					`Si la aceptan, te mandamos un correo con un <strong style="color:#ffffff;">enlace para pagar</strong>.`,
					`En cuanto pagues, recibes tus entradas con el código QR.`,
				)) +
		emailFineprint(fmt.Sprintf(
			`No te hemos pedido ningún dato de pago y no se te cobrará nada por enviar esta solicitud. Si en <strong style="color:#ffffff;">%s</strong> no hay respuesta, la solicitud caduca sola y te avisamos.`,
			decision))

	return renderEmailShell(emailShellData{
		HTMLTitle:  "Solicitud recibida - Pull",
		AccentRGB:  emailAccentAmber,
		BannerText: "PENDIENTE DE APROBACIÓN",
		Title:      "Solicitud recibida",
		BodyHTML:   body,
		FooterNote: "Pull Events",
	})
}

// SendApprovalPending tells the buyer their request was received and is
// awaiting staff approval. Nothing has been charged.
func (e *EmailService) SendApprovalPending(ctx context.Context, to string, d ApprovalEmailData) error {
	_, err := e.Send(ctx, EmailRequest{
		To:      []string{to},
		Subject: "Solicitud recibida — " + d.EventName,
		HTML:    BuildApprovalPendingEmail(d),
		Tags:    []EmailTag{{Name: "type", Value: "approval_pending"}},
	})
	return err
}

// BuildApprovalApprovedEmail renders the "approved — now pay" email HTML.
// paymentURL is the link that reopens the existing order on the checkout page;
// hoursToPay is the payment window in hours (0 → defaultApprovalPaymentHours).
// Exported so preview tooling can render it without sending.
func BuildApprovalApprovedEmail(d ApprovalEmailData, paymentURL string, hoursToPay int) string {
	esc := html.EscapeString
	window := approvalHoursPhrase(hoursToPay, defaultApprovalPaymentHours)

	cta := ""
	if paymentURL != "" {
		cta = emailButton(esc(paymentURL), "Pagar mis entradas") + approvalPayFallbackLink(paymentURL)
	}

	body := emailGreeting("Hola ", esc(d.CustomerName)) +
		emailParagraph(fmt.Sprintf(
			`Buenas noticias: el equipo de %s ha aceptado tu solicitud para <strong style="color:#ffffff;">%s</strong>. Solo falta que pagues y las entradas son tuyas.`,
			esc(d.VenueName), esc(d.EventName))) +
		approvalAmountCard(emailAccentGreen, "TOTAL A PAGAR", d.Total, d.Currency,
			fmt.Sprintf(`Tienes <strong style="color:#ffffff;">%s</strong> para completar el pago.`, window)) +
		cta +
		approvalHero(d) +
		approvalDetailsCard(emailAccentGreen, d) +
		emailInfoCard(
			emailCardLabel("Qué pasa después")+
				approvalSteps(
					`Pagas desde el botón de arriba, en una página segura.`,
					`Te llega otro correo con tus entradas y su código QR.`,
					`Enseñas el QR en la puerta el día del evento.`,
				)) +
		emailFineprint(fmt.Sprintf(
			`Tu sitio está guardado durante <strong style="color:#ffffff;">%s</strong>. Si para entonces no has pagado, se libera para otra persona y tendrás que solicitarlo de nuevo.`,
			window))

	return renderEmailShell(emailShellData{
		HTMLTitle:  "Solicitud aprobada - Pull",
		AccentRGB:  emailAccentGreen,
		BannerText: "APROBADA — PENDIENTE DE PAGO",
		Title:      "¡Tu solicitud ha sido aprobada!",
		BodyHTML:   body,
		FooterNote: "Pull Events",
	})
}

// SendApprovalApproved tells the buyer staff accepted their request and hands
// them the payment link plus the deadline. Pass hoursToPay = 0 to use the
// documented 24h default.
func (e *EmailService) SendApprovalApproved(ctx context.Context, to string, d ApprovalEmailData, paymentURL string, hoursToPay int) error {
	_, err := e.Send(ctx, EmailRequest{
		To:      []string{to},
		Subject: "¡Aprobada! Ya puedes pagar — " + d.EventName,
		HTML:    BuildApprovalApprovedEmail(d, paymentURL, hoursToPay),
		Tags:    []EmailTag{{Name: "type", Value: "approval_approved"}},
	})
	return err
}

// BuildApprovalRejectedEmail renders the "declined" (expired=false) or "nobody
// answered in time" (expired=true) email HTML. Neither case involves money:
// the buyer never paid. Exported so preview tooling can render it.
func BuildApprovalRejectedEmail(d ApprovalEmailData, expired bool) string {
	esc := html.EscapeString
	heading := approvalRejectedHeading(expired)

	badge := "NO ACEPTADA"
	intro := fmt.Sprintf(
		`El equipo de %s no ha podido aceptar tu solicitud para <strong style="color:#ffffff;">%s</strong>. Sentimos no darte mejores noticias.`,
		esc(d.VenueName), esc(d.EventName))
	closing := `Si crees que ha sido un error o quieres preguntar, responde a este correo y te echamos una mano.`

	if expired {
		badge = "SIN RESPUESTA A TIEMPO"
		intro = fmt.Sprintf(
			`Tu solicitud para <strong style="color:#ffffff;">%s</strong> ha caducado: el equipo de %s no llegó a responder dentro del plazo de %s.`,
			esc(d.EventName), esc(d.VenueName),
			approvalHoursPhrase(defaultApprovalDecisionHours, defaultApprovalDecisionHours))
		closing = `Si todavía quieres ir, puedes enviar una solicitud nueva — si quedan plazas, el equipo la revisará.`
	}

	body := emailGreeting("Hola ", esc(d.CustomerName)) +
		emailParagraph(intro) +
		approvalAmountCard(emailAccentRed, "TE HEMOS COBRADO", "0.00", d.Currency, noChargeNote) +
		approvalHero(d) +
		approvalDetailsCard(emailAccentRed, d) +
		emailFineprint(closing)

	return renderEmailShell(emailShellData{
		HTMLTitle:  heading + " - Pull",
		AccentRGB:  emailAccentRed,
		BannerText: badge,
		Title:      heading,
		BodyHTML:   body,
		FooterNote: "Pull Events",
	})
}

// SendApprovalRejected tells the buyer their request did not go through, with
// no money involved. Used for staff rejection (expired=false) and for the 48h
// no-decision expiry (expired=true).
func (e *EmailService) SendApprovalRejected(ctx context.Context, to string, d ApprovalEmailData, expired bool) error {
	heading := approvalRejectedHeading(expired)
	_, err := e.Send(ctx, EmailRequest{
		To:      []string{to},
		Subject: heading + " — " + d.EventName,
		HTML:    BuildApprovalRejectedEmail(d, expired),
		Tags:    []EmailTag{{Name: "type", Value: "approval_rejected"}},
	})
	return err
}

// BuildApprovalPaymentExpiredEmail renders the third negative case: the
// request WAS approved, but the buyer did not pay within the window, so the
// spot was released. hoursToPay is the window that just ran out (0 → default).
// Exported so preview tooling can render it without sending.
func BuildApprovalPaymentExpiredEmail(d ApprovalEmailData, hoursToPay int) string {
	esc := html.EscapeString
	heading := approvalPaymentExpiredHeading()
	window := approvalHoursPhrase(hoursToPay, defaultApprovalPaymentHours)

	body := emailGreeting("Hola ", esc(d.CustomerName)) +
		emailParagraph(fmt.Sprintf(
			`Tu solicitud para <strong style="color:#ffffff;">%s</strong> estaba aprobada, pero no llegó el pago dentro de las %s que tenías. Hemos liberado tu sitio para que otra persona pueda usarlo.`,
			esc(d.EventName), window)) +
		approvalAmountCard(emailAccentRed, "TE HEMOS COBRADO", "0.00", d.Currency,
			`No se te ha cobrado nada y el enlace de pago que te mandamos ya no funciona.`) +
		approvalHero(d) +
		approvalDetailsCard(emailAccentRed, d) +
		emailFineprint(fmt.Sprintf(
			`Si todavía quieres ir, envía una solicitud nueva y el equipo de %s la revisará otra vez — siempre que queden plazas.`,
			esc(d.VenueName)))

	return renderEmailShell(emailShellData{
		HTMLTitle:  heading + " - Pull",
		AccentRGB:  emailAccentRed,
		BannerText: "PLAZO DE PAGO AGOTADO",
		Title:      heading,
		BodyHTML:   body,
		FooterNote: "Pull Events",
	})
}

// SendApprovalPaymentExpired tells the buyer their approved request expired
// because they did not pay in time. Pass hoursToPay = 0 for the 24h default.
func (e *EmailService) SendApprovalPaymentExpired(ctx context.Context, to string, d ApprovalEmailData, hoursToPay int) error {
	_, err := e.Send(ctx, EmailRequest{
		To:      []string{to},
		Subject: approvalPaymentExpiredHeading() + " — " + d.EventName,
		HTML:    BuildApprovalPaymentExpiredEmail(d, hoursToPay),
		Tags:    []EmailTag{{Name: "type", Value: "approval_payment_expired"}},
	})
	return err
}
