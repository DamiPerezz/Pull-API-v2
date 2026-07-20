package services

import (
	"context"
	"fmt"
	"pull-api-v2/config"
	"time"
)

// GroupReservationEmailData carries the fields for the group reservation
// confirmation email sent to the organizer right after creating the booking.
type GroupReservationEmailData struct {
	To                string
	OrganizerName     string
	EventName         string
	EventDate         string
	ReservationNumber string
	ManagementCode    string
	PaymentLinkCode   string
	GuestCount        int
	TotalAmount       float64
	Currency          string
}

// SendGroupReservationConfirmation sends the organizer their reservation
// summary + tracking link. Uses the embedded v1 template
// "group_reservation_pending.html" so the design stays consistent with the
// rest of the Pull emails.
func (e *EmailService) SendGroupReservationConfirmation(ctx context.Context, data GroupReservationEmailData) error {
	if e == nil {
		return nil
	}
	if data.Currency == "" {
		data.Currency = "GTQ"
	}

	payload := map[string]interface{}{
		"name":              data.OrganizerName,
		"event_name":        data.EventName,
		"event_date":        data.EventDate,
		"guest_count":       data.GuestCount,
		"total_amount":      fmt.Sprintf("%s %.2f", data.Currency, data.TotalAmount),
		"management_code":   data.ManagementCode,
		"payment_link_code": data.PaymentLinkCode,
		"track_url":         fmt.Sprintf("%s/es/group/track/%s", config.App.FrontendURL, data.PaymentLinkCode),
	}

	html, err := e.renderTemplate("group_reservation_pending", payload)
	if err != nil {
		return err
	}

	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err = e.Send(c, EmailRequest{
		To:      []string{data.To},
		Subject: fmt.Sprintf("Reserva %s — Pull Events", data.ReservationNumber),
		HTML:    html,
		Tags: []EmailTag{
			{Name: "type", Value: "group_reservation"},
			{Name: "reservation", Value: data.ReservationNumber},
		},
	})
	return err
}

// GroupReservationApprovedData carries the fields for the approval email —
// the one that gives the organizer the shared group payment link.
type GroupReservationApprovedData struct {
	To                  string
	OrganizerName       string
	EventName           string
	EventDate           string
	ReservationNumber   string
	PaymentLinkCode     string
	GuestCount          int
	HostPaidGuestsCount int
}

// SendGroupReservationApproved notifies the organizer their table was
// approved and hands them the payment/tracking link to share with the group
// so each member fills in their data and pays their part.
func (e *EmailService) SendGroupReservationApproved(ctx context.Context, data GroupReservationApprovedData) error {
	if e == nil {
		return nil
	}

	payload := map[string]interface{}{
		"organizer_name":         data.OrganizerName,
		"event_name":             data.EventName,
		"event_date":             data.EventDate,
		"guest_count":            data.GuestCount,
		"payment_link":           fmt.Sprintf("%s/es/group/track/%s", config.App.FrontendURL, data.PaymentLinkCode),
		"has_host_paid_guests":   data.HostPaidGuestsCount > 0,
		"host_paid_guests_count": data.HostPaidGuestsCount,
		// The access-code flow is not enabled in the demo; guests complete
		// their data directly from the tracking link.
		"host_paid_access_code": data.PaymentLinkCode,
	}

	html, err := e.renderTemplate("group_reservation_approved", payload)
	if err != nil {
		return err
	}

	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err = e.Send(c, EmailRequest{
		To:      []string{data.To},
		Subject: fmt.Sprintf("¡Reserva %s aprobada! — Link de pago del grupo", data.ReservationNumber),
		HTML:    html,
		Tags: []EmailTag{
			{Name: "type", Value: "group_reservation_approved"},
			{Name: "reservation", Value: data.ReservationNumber},
		},
	})
	return err
}

// SendOrderTicketsEmail sends the buyer a single email with both the order
// summary and the ticket QR codes (rendered as URLs the customer can scan).
// It runs synchronously but is intended to be called in a goroutine from the
// confirm handler so the API response isn't blocked.
type OrderTicketsEmailData struct {
	To           string
	CustomerName string
	OrderNumber  string
	EventName    string
	EventDate    string
	EventTime    string
	VenueName    string
	VenueAddress string
	TicketType   string
	Quantity     int
	Total        float64
	Currency     string
	QRTokens     []string
}

func (e *EmailService) SendOrderTicketsEmail(ctx context.Context, data OrderTicketsEmailData) error {
	if e == nil {
		return nil
	}
	if data.Currency == "" {
		data.Currency = "GTQ"
	}

	// Build ticket rows. Each row shows the QR token (which the customer can
	// re-scan from the frontend's /tickets page).
	ticketsHTML := ""
	for i, q := range data.QRTokens {
		ticketsHTML += fmt.Sprintf(`
      <div style="background:#1a1a24;border:1px solid #2a2a3a;border-radius:10px;padding:14px 18px;margin-bottom:10px;">
        <div style="font-size:11px;color:#8b8b9b;letter-spacing:1px;">ENTRADA #%d</div>
        <div style="font-family:monospace;font-size:13px;color:#fff;margin-top:6px;word-break:break-all;">%s</div>
      </div>`, i+1, q)
	}

	html := fmt.Sprintf(`
<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="font-family:-apple-system,BlinkMacSystemFont,system-ui,sans-serif;margin:0;padding:24px;background:#0a0a0f;color:#fff;">
  <div style="max-width:560px;margin:0 auto;background:#15151f;border:1px solid #2a2a3a;border-radius:14px;padding:32px;">
    <div style="font-size:12px;letter-spacing:2px;color:#8b5cf6;font-weight:600;">PULL EVENTS</div>
    <h1 style="font-size:24px;margin:8px 0 24px;">¡Pago confirmado!</h1>
    <p style="color:#a0a0b0;margin:0 0 24px;">Hola %s, tu compra para <strong>%s</strong> ha sido confirmada.</p>

    <div style="background:rgba(99,102,241,0.08);border-left:3px solid #6366f1;padding:16px 18px;border-radius:8px;margin-bottom:20px;">
      <div style="font-size:11px;color:#8b8b9b;letter-spacing:1px;">ORDEN</div>
      <div style="font-size:18px;font-weight:700;margin-top:4px;">%s</div>
    </div>

    <table style="width:100%%;border-collapse:collapse;font-size:14px;margin-bottom:24px;">
      <tr><td style="padding:8px 0;color:#a0a0b0;">Evento</td><td style="padding:8px 0;text-align:right;font-weight:500;">%s</td></tr>
      <tr><td style="padding:8px 0;color:#a0a0b0;border-top:1px solid #2a2a3a;">Fecha</td><td style="padding:8px 0;text-align:right;border-top:1px solid #2a2a3a;">%s</td></tr>
      <tr><td style="padding:8px 0;color:#a0a0b0;border-top:1px solid #2a2a3a;">Hora</td><td style="padding:8px 0;text-align:right;border-top:1px solid #2a2a3a;">%s</td></tr>
      <tr><td style="padding:8px 0;color:#a0a0b0;border-top:1px solid #2a2a3a;">Lugar</td><td style="padding:8px 0;text-align:right;border-top:1px solid #2a2a3a;">%s</td></tr>
      <tr><td style="padding:8px 0;color:#a0a0b0;border-top:1px solid #2a2a3a;">Tipo</td><td style="padding:8px 0;text-align:right;border-top:1px solid #2a2a3a;">%s × %d</td></tr>
      <tr><td style="padding:12px 0;font-weight:700;border-top:2px solid #6366f1;">Total</td><td style="padding:12px 0;text-align:right;font-weight:700;font-size:18px;border-top:2px solid #6366f1;">%s %.2f</td></tr>
    </table>

    <h2 style="font-size:16px;color:#fff;margin:24px 0 12px;">Tus entradas</h2>
    %s

    <p style="color:#6b6b7b;font-size:12px;margin:24px 0 0;text-align:center;line-height:1.5;">
      Presenta cada código QR en la puerta · Pull Events
    </p>
  </div>
</body>
</html>`,
		data.CustomerName, data.EventName, data.OrderNumber,
		data.EventName, data.EventDate, data.EventTime, data.VenueName,
		data.TicketType, data.Quantity, data.Currency, data.Total,
		ticketsHTML,
	)

	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := e.Send(c, EmailRequest{
		To:      []string{data.To},
		Subject: fmt.Sprintf("Entradas confirmadas: %s — Pull Events", data.EventName),
		HTML:    html,
		Tags: []EmailTag{
			{Name: "type", Value: "order_tickets"},
			{Name: "order", Value: data.OrderNumber},
		},
	})
	return err
}
