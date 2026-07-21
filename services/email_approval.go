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

// approvalShell — mismo lenguaje visual oscuro que el email de tickets:
// tarjeta oscura, marca PULL EVENTS, foto del evento y tabla de detalles.
func approvalShell(accentRGB, badge, badgeColor string, d ApprovalEmailData, bodyHTML string) string {
	esc := html.EscapeString
	hero := ""
	if d.EventImage != "" {
		hero = fmt.Sprintf(`<img src="%s" alt="" width="100%%" style="display:block;width:100%%;max-height:220px;object-fit:cover;border-radius:12px;margin:0 0 24px;" />`, esc(d.EventImage))
	}
	row := func(label, value string) string {
		if value == "" {
			return ""
		}
		return fmt.Sprintf(`<tr><td style="padding:9px 0;color:#a0a0b0;border-top:1px solid #2a2a3a;">%s</td><td style="padding:9px 0;text-align:right;color:#fff;font-weight:500;border-top:1px solid #2a2a3a;">%s</td></tr>`, label, esc(value))
	}
	details := fmt.Sprintf(`
    <table style="width:100%%;border-collapse:collapse;font-size:14px;margin:20px 0 4px;">
      <tr><td style="padding:9px 0;color:#a0a0b0;">Evento</td><td style="padding:9px 0;text-align:right;color:#fff;font-weight:600;">%s</td></tr>
      %s%s%s%s
    </table>`,
		esc(d.EventName),
		row("Fecha", d.EventDate), row("Hora", d.EventTime),
		row("Lugar", firstNonEmpty(d.VenueLocation, d.VenueName)),
		row("Solicitud", d.OrderNumber))

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="es"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"></head>
<body style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;margin:0;padding:24px;background:#0a0a0f;color:#fff;">
  <div style="max-width:560px;margin:0 auto;background:#15151f;border:1px solid #2a2a3a;border-radius:16px;padding:32px;">
    <div style="font-size:12px;letter-spacing:3px;color:#8b5cf6;font-weight:700;margin-bottom:16px;">PULL EVENTS</div>
    %s
    <div style="display:inline-block;background:rgba(%s,0.14);border:1px solid rgba(%s,0.4);color:%s;font-size:12px;font-weight:700;letter-spacing:1px;padding:6px 14px;border-radius:999px;margin-bottom:14px;">%s</div>
    %s
    %s
  </div>
  <p style="color:#6b6b7b;font-size:11px;text-align:center;margin:18px 0 0;">Pull Events</p>
</body></html>`, hero, accentRGB, accentRGB, badgeColor, badge, bodyHTML, details)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// SendApprovalPending tells the buyer their request was received and is
// awaiting staff approval (funds held, not charged).
func (e *EmailService) SendApprovalPending(ctx context.Context, to string, d ApprovalEmailData) error {
	esc := html.EscapeString
	body := fmt.Sprintf(`
    <h1 style="font-size:24px;margin:4px 0 10px;color:#fff;">Solicitud recibida</h1>
    <p style="color:#a0a0b0;margin:0 0 18px;font-size:15px;line-height:1.6;">Hola %s, hemos recibido tu solicitud para <strong style="color:#fff;">%s</strong>. Es un evento privado: el equipo de %s debe aprobarla.</p>
    <div style="background:rgba(139,92,246,0.08);border-left:3px solid #8b5cf6;padding:14px 16px;border-radius:10px;margin-bottom:6px;">
      <div style="font-size:11px;color:#8b8b9b;letter-spacing:1.2px;margin-bottom:2px;">IMPORTE RETENIDO — NO COBRADO</div>
      <div style="font-size:22px;font-weight:800;color:#fff;">%s %s</div>
    </div>
    <p style="color:#a0a0b0;font-size:13px;line-height:1.6;margin:14px 0 0;">Solo se cobrará si el equipo aprueba tu solicitud — entonces recibirás tu entrada con el código QR. Si en <strong style="color:#fff;">48 horas</strong> no hay respuesta, la retención se libera sola y no pagas nada.</p>`,
		esc(d.CustomerName), esc(d.EventName), esc(d.VenueName), esc(d.Total), esc(d.Currency))

	_, err := e.Send(ctx, EmailRequest{
		To:      []string{to},
		Subject: "Solicitud recibida — " + d.EventName,
		HTML:    approvalShell("251,191,36", "PENDIENTE DE APROBACIÓN", "#fbbf24", d, body),
		Tags:    []EmailTag{{Name: "type", Value: "approval_pending"}},
	})
	return err
}

// SendApprovalRejected tells the buyer their request was declined and the
// held funds were released. Used for both staff rejection and 48h expiry.
func (e *EmailService) SendApprovalRejected(ctx context.Context, to string, d ApprovalEmailData, expired bool) error {
	esc := html.EscapeString
	reason := "El equipo no ha aprobado tu solicitud."
	heading := "Solicitud rechazada"
	badge := "RECHAZADA"
	if expired {
		reason = "El equipo no respondió dentro del plazo de 48 horas."
		heading = "Solicitud no procesada"
		badge = "SIN RESPUESTA EN 48H"
	}
	body := fmt.Sprintf(`
    <h1 style="font-size:24px;margin:4px 0 10px;color:#fff;">%s</h1>
    <p style="color:#a0a0b0;margin:0 0 18px;font-size:15px;line-height:1.6;">Hola %s, %s Tu solicitud para <strong style="color:#fff;">%s</strong> no se ha completado.</p>
    <div style="background:rgba(248,113,113,0.08);border-left:3px solid #f87171;padding:14px 16px;border-radius:10px;margin-bottom:6px;">
      <div style="font-size:11px;color:#8b8b9b;letter-spacing:1.2px;margin-bottom:2px;">IMPORTE LIBERADO</div>
      <div style="font-size:22px;font-weight:800;color:#fff;">%s %s</div>
    </div>
    <p style="color:#a0a0b0;font-size:13px;line-height:1.6;margin:14px 0 0;"><strong style="color:#fff;">No se te ha cobrado nada.</strong> La retención sobre tu tarjeta se ha liberado; según tu banco puede tardar unos días en desaparecer del extracto.</p>`,
		heading, esc(d.CustomerName), reason, esc(d.EventName), esc(d.Total), esc(d.Currency))

	_, err := e.Send(ctx, EmailRequest{
		To:      []string{to},
		Subject: heading + " — " + d.EventName,
		HTML:    approvalShell("248,113,113", badge, "#f87171", d, body),
		Tags:    []EmailTag{{Name: "type", Value: "approval_rejected"}},
	})
	return err
}
