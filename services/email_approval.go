package services

import (
	"context"
	"fmt"
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
	CustomerName string
	EventName    string
	VenueName    string
	OrderNumber  string
	Total        string
	Currency     string
}

func approvalShell(title, accent, heading, bodyHTML string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"></head>
<body style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;margin:0;padding:20px;background:#f5f5f5;">
  <div style="max-width:520px;margin:0 auto;background:#fff;border-radius:12px;overflow:hidden;box-shadow:0 2px 8px rgba(0,0,0,.08);">
    <div style="background:%s;padding:28px 40px;">
      <h1 style="color:#fff;margin:0;font-size:22px;">%s</h1>
    </div>
    <div style="padding:32px 40px;color:#333;font-size:15px;line-height:1.6;">%s</div>
  </div>
</body></html>`, accent, heading, bodyHTML)
}

// SendApprovalPending tells the buyer their request was received and is
// awaiting staff approval (funds held, not charged).
func (e *EmailService) SendApprovalPending(ctx context.Context, to string, d ApprovalEmailData) error {
	body := fmt.Sprintf(`
    <p>Hola %s,</p>
    <p>Hemos recibido tu solicitud de entrada para <strong>%s</strong>. Es un evento privado, así que el equipo de <strong>%s</strong> debe aprobarla.</p>
    <div style="background:#f8f9fa;border-radius:8px;padding:16px;margin:20px 0;">
      <p style="margin:0 0 6px;"><strong>Solicitud:</strong> %s</p>
      <p style="margin:0;"><strong>Importe retenido:</strong> %s %s</p>
    </div>
    <p><strong>Importante:</strong> el importe está <u>retenido, no cobrado</u>. Solo se cobrará cuando el equipo apruebe tu solicitud, y recibirás tu entrada con el código QR.</p>
    <p>El equipo tiene <strong>48 horas</strong> para responder. Si no se aprueba en ese plazo, la retención se libera automáticamente y no se te cobrará nada.</p>`,
		d.CustomerName, d.EventName, d.VenueName, d.OrderNumber, d.Total, d.Currency)

	_, err := e.Send(ctx, EmailRequest{
		To:      []string{to},
		Subject: "Solicitud recibida — " + d.EventName,
		HTML:    approvalShell("Solicitud recibida", "#6d28d9", "Solicitud recibida", body),
		Tags:    []EmailTag{{Name: "type", Value: "approval_pending"}},
	})
	return err
}

// SendApprovalRejected tells the buyer their request was declined and the
// held funds were released. Used for both staff rejection and 48h expiry.
func (e *EmailService) SendApprovalRejected(ctx context.Context, to string, d ApprovalEmailData, expired bool) error {
	reason := "El equipo no ha aprobado tu solicitud."
	heading := "Solicitud rechazada"
	if expired {
		reason = "El equipo no respondió dentro del plazo de 48 horas."
		heading = "Solicitud no procesada"
	}
	body := fmt.Sprintf(`
    <p>Hola %s,</p>
    <p>%s Tu solicitud de entrada para <strong>%s</strong> no se ha completado.</p>
    <div style="background:#f8f9fa;border-radius:8px;padding:16px;margin:20px 0;">
      <p style="margin:0 0 6px;"><strong>Solicitud:</strong> %s</p>
      <p style="margin:0;"><strong>Importe liberado:</strong> %s %s</p>
    </div>
    <p><strong>No se te ha cobrado nada.</strong> La retención sobre tu tarjeta se ha liberado; según tu banco puede tardar unos días en desaparecer del todo.</p>`,
		d.CustomerName, reason, d.EventName, d.OrderNumber, d.Total, d.Currency)

	_, err := e.Send(ctx, EmailRequest{
		To:      []string{to},
		Subject: heading + " — " + d.EventName,
		HTML:    approvalShell(heading, "#b91c1c", heading, body),
		Tags:    []EmailTag{{Name: "type", Value: "approval_rejected"}},
	})
	return err
}
