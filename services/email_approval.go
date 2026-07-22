package services

import (
	"context"
	"fmt"
	"html"
)

// =============================================
// APPROVAL-FLOW EMAILS (private events)
// A private event authorizes (holds) the payment and waits for staff
// approval. The buyer does NOT get a ticket/QR yet — they get one of these
// status emails instead. The real ticket email (SendTickets) only goes out
// once staff approves and the funds are captured.
//
// Visual system: shared dark theme (see email_theme.go), same language as
// the tickets_with_pdfs.html reference template. Banner color is semantic:
// amber = pending, red = rejected/expired.
// =============================================

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

// approvalAmountCard renders the highlighted amount box (held / released),
// centered like the reference "Event Details" card values.
func approvalAmountCard(accentRGB, label, total, currency string) string {
	esc := html.EscapeString
	inner := fmt.Sprintf(
		`<div style="font-family:%s;font-size:11px;font-weight:600;letter-spacing:1.2px;line-height:1;text-align:center;text-transform:uppercase;color:#93939d;color:rgba(255, 255, 255, 0.55);margin-bottom:12px;">%s</div>%s`,
		emailFontStack, label, emailBigValue(esc(total)+" "+esc(currency)))
	return emailAccentCard(accentRGB, inner)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// approvalRejectedHeading is the single source of truth for the heading (and
// subject) of the rejected/expired approval email.
func approvalRejectedHeading(expired bool) string {
	if expired {
		return "Solicitud no procesada"
	}
	return "Solicitud rechazada"
}

// BuildApprovalPendingEmail renders the "request received, funds held" email
// HTML. Exported so preview tooling can render it without sending.
func BuildApprovalPendingEmail(d ApprovalEmailData) string {
	esc := html.EscapeString
	body := emailGreeting("Hola ", esc(d.CustomerName)) +
		emailParagraph(fmt.Sprintf(
			`Hemos recibido tu solicitud para <strong style="color:#ffffff;">%s</strong>. Es un evento privado: el equipo de %s debe aprobarla.`,
			esc(d.EventName), esc(d.VenueName))) +
		approvalAmountCard(emailAccentAmber, "IMPORTE RETENIDO — NO COBRADO", d.Total, d.Currency) +
		approvalHero(d) +
		approvalDetailsCard(emailAccentAmber, d) +
		emailFineprint(`Solo se cobrará si el equipo aprueba tu solicitud — entonces recibirás tu entrada con el código QR. Si en <strong style="color:#ffffff;">48 horas</strong> no hay respuesta, la retención se libera sola y no pagas nada.`)

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
// awaiting staff approval (funds held, not charged).
func (e *EmailService) SendApprovalPending(ctx context.Context, to string, d ApprovalEmailData) error {
	_, err := e.Send(ctx, EmailRequest{
		To:      []string{to},
		Subject: "Solicitud recibida — " + d.EventName,
		HTML:    BuildApprovalPendingEmail(d),
		Tags:    []EmailTag{{Name: "type", Value: "approval_pending"}},
	})
	return err
}

// BuildApprovalRejectedEmail renders the "declined / expired, funds released"
// email HTML. Exported so preview tooling can render it without sending.
func BuildApprovalRejectedEmail(d ApprovalEmailData, expired bool) string {
	esc := html.EscapeString
	reason := "El equipo no ha aprobado tu solicitud."
	heading := approvalRejectedHeading(expired)
	badge := "RECHAZADA"
	if expired {
		reason = "El equipo no respondió dentro del plazo de 48 horas."
		badge = "SIN RESPUESTA EN 48H"
	}

	body := emailGreeting("Hola ", esc(d.CustomerName)) +
		emailParagraph(fmt.Sprintf(
			`%s Tu solicitud para <strong style="color:#ffffff;">%s</strong> no se ha completado.`,
			reason, esc(d.EventName))) +
		approvalAmountCard(emailAccentRed, "IMPORTE LIBERADO", d.Total, d.Currency) +
		approvalHero(d) +
		approvalDetailsCard(emailAccentRed, d) +
		emailFineprint(`<strong style="color:#ffffff;">No se te ha cobrado nada.</strong> La retención sobre tu tarjeta se ha liberado; según tu banco puede tardar unos días en desaparecer del extracto.`)

	return renderEmailShell(emailShellData{
		HTMLTitle:  heading + " - Pull",
		AccentRGB:  emailAccentRed,
		BannerText: badge,
		Title:      heading,
		BodyHTML:   body,
		FooterNote: "Pull Events",
	})
}

// SendApprovalRejected tells the buyer their request was declined and the
// held funds were released. Used for both staff rejection and 48h expiry.
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
