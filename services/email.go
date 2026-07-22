package services

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"pull-api-v2/config"
	"sync"
	"time"
)

// emailTemplatesFS embeds the legacy Pull-API-Go email templates into the v2
// binary so we don't have to ship the templates/ directory alongside the
// Docker image. The names match the v1 template filenames (e.g.
// "tickets_with_pdfs.html").
//
//go:embed templates/*.html
var emailTemplatesFS embed.FS

// =============================================
// EMAIL SERVICE
// Uses Resend API for transactional emails
// =============================================

// EmailService handles email sending
type EmailService struct {
	apiKey    string
	fromEmail string
	fromName  string
	baseURL   string
	client    *http.Client
}

// Global email service instance
var Email *EmailService

// Buffer pool for email JSON encoding (reduces allocations)
var emailBufferPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

// InitEmailService initializes the email service
func InitEmailService() error {
	if config.App.ResendAPIKey == "" {
		log.Println("Email Service: No API key configured, emails will be logged only")
	}

	// Parse from email (format: "Name <email>" or just "email")
	fromEmail := config.App.ResendFromEmail
	fromName := "Pull Events"

	Email = &EmailService{
		apiKey:    config.App.ResendAPIKey,
		fromEmail: fromEmail,
		fromName:  fromName,
		baseURL:   "https://api.resend.com",
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}

	log.Println("Email Service: Initialized")
	return nil
}

// =============================================
// EMAIL TYPES
// =============================================

// EmailRequest represents an email to send
type EmailRequest struct {
	To          []string          `json:"to"`
	Subject     string            `json:"subject"`
	HTML        string            `json:"html,omitempty"`
	Text        string            `json:"text,omitempty"`
	ReplyTo     string            `json:"reply_to,omitempty"`
	CC          []string          `json:"cc,omitempty"`
	BCC         []string          `json:"bcc,omitempty"`
	Attachments []EmailAttachment `json:"attachments,omitempty"`
	Tags        []EmailTag        `json:"tags,omitempty"`
}

// EmailAttachment represents an email attachment
type EmailAttachment struct {
	Filename    string `json:"filename"`
	Content     string `json:"content"` // Base64 encoded
	ContentType string `json:"content_type,omitempty"`
}

// EmailTag for tracking
type EmailTag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// EmailResponse from Resend API
type EmailResponse struct {
	ID string `json:"id"`
}

// =============================================
// SEND METHODS
// =============================================

// Send sends an email. When BREVO_API_KEY is configured we route via Brevo
// (free 300/day without domain verification); otherwise we fall back to the
// Resend transactional API.
func (e *EmailService) Send(ctx context.Context, req EmailRequest) (string, error) {
	// Brevo first (preferred for the demo deployment).
	brevoAttempted := false
	if config.App.BrevoAPIKey != "" {
		brevoAttempted = true
		id, err := e.sendViaBrevo(ctx, req)
		if err == nil {
			return id, nil
		}
		// Fall through to Resend if Brevo errored.
		log.Printf("[Email] Brevo failed, falling back to Resend: %v", err)
	}

	// Sin key de Resend: solo se puede "mockear" en desarrollo real (ningún
	// proveedor configurado). Si Brevo ESTABA configurado y falló, esto es un
	// fallo de entrega REAL — NUNCA devolver éxito falso, o el caller marca el
	// ticket como entregado cuando el comprador se quedó sin su QR.
	if e.apiKey == "" {
		if brevoAttempted {
			return "", fmt.Errorf("email NO enviado: Brevo falló y no hay RESEND_API_KEY de fallback")
		}
		log.Printf("[Email] Would send to %v: %s (sin proveedor de email configurado)", req.To, req.Subject)
		return "mock-email-id", nil
	}

	// Build request body
	body := map[string]interface{}{
		"from":    e.fromEmail, // Already in "Name <email>" format from config
		"to":      req.To,
		"subject": req.Subject,
	}

	if req.HTML != "" {
		body["html"] = req.HTML
	}
	if req.Text != "" {
		body["text"] = req.Text
	}
	if req.ReplyTo != "" {
		body["reply_to"] = req.ReplyTo
	}
	if len(req.CC) > 0 {
		body["cc"] = req.CC
	}
	if len(req.BCC) > 0 {
		body["bcc"] = req.BCC
	}
	if len(req.Attachments) > 0 {
		body["attachments"] = req.Attachments
	}
	if len(req.Tags) > 0 {
		// Resend only accepts ASCII letters, numbers, underscores and dashes
		// in tag names/values — anything else 422s the whole send.
		body["tags"] = sanitizeResendTags(req.Tags)
	}

	// OPTIMIZED: Use buffer pool to reduce allocations
	buf := emailBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer emailBufferPool.Put(buf)

	if err := json.NewEncoder(buf).Encode(body); err != nil {
		return "", fmt.Errorf("failed to encode email: %w", err)
	}

	// Create request
	httpReq, err := http.NewRequestWithContext(ctx, "POST", e.baseURL+"/emails", buf)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Authorization", "Bearer "+e.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	// Send request
	resp, err := e.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("failed to send email: %w", err)
	}
	defer resp.Body.Close()

	// OPTIMIZED: Handle errors without full body read for success case
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		// Log the rejection so it's visible in Fly logs, but don't surface it
		// to callers (most call sites fire-and-forget; we don't want a quiet
		// email failure to abort the surrounding business flow).
		log.Printf("[Email] REJECTED to=%v subject=%q status=%d body=%s",
			req.To, req.Subject, resp.StatusCode, string(errBody))
		return "", fmt.Errorf("email API error %d: %s", resp.StatusCode, string(errBody))
	}

	// OPTIMIZED: Stream decode directly from response body
	var emailResp EmailResponse
	if err := json.NewDecoder(resp.Body).Decode(&emailResp); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	log.Printf("[Email] Sent to %v: %s (ID: %s)", req.To, req.Subject, emailResp.ID)
	return emailResp.ID, nil
}

// sanitizeResendTags maps arbitrary tag values to Resend's allowed charset
// (ASCII letters, numbers, underscore, dash) by replacing anything else with
// a dash.
func sanitizeResendTags(tags []EmailTag) []EmailTag {
	clean := func(s string) string {
		out := make([]rune, 0, len(s))
		for _, r := range s {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
				out = append(out, r)
			default:
				out = append(out, '-')
			}
		}
		return string(out)
	}
	result := make([]EmailTag, len(tags))
	for i, t := range tags {
		result[i] = EmailTag{Name: clean(t.Name), Value: clean(t.Value)}
	}
	return result
}

// =============================================
// TEMPLATE EMAILS
// =============================================

// BuildVerificationCodeEmail renders the login-code email HTML (shared dark
// theme). Exported so preview tooling can render it without sending.
func BuildVerificationCodeEmail(venueName, code string) string {
	esc := html.EscapeString
	body := emailParagraph(fmt.Sprintf("Usa este código para iniciar sesión en %s:", esc(venueName))) +
		emailAccentCard(emailAccentPurple, emailCode(esc(code))) +
		emailFineprint("Este código expira en 10 minutos.") +
		emailFineprint("Si no solicitaste este código, puedes ignorar este correo.")

	return renderEmailShell(emailShellData{
		HTMLTitle: "Tu código de verificación - Pull",
		AccentRGB: emailAccentPurple,
		Title:     "Tu código de verificación",
		BodyHTML:  body,
	})
}

// SendVerificationCode sends verification code email
func (e *EmailService) SendVerificationCode(ctx context.Context, to, code, venueName string) error {
	_, err := e.Send(ctx, EmailRequest{
		To:      []string{to},
		Subject: fmt.Sprintf("Tu código de verificación: %s", code),
		HTML:    BuildVerificationCodeEmail(venueName, code),
		Tags: []EmailTag{
			{Name: "type", Value: "verification"},
			{Name: "venue", Value: venueName},
		},
	})
	return err
}

// SendOrderConfirmation sends order confirmation email
func (e *EmailService) SendOrderConfirmation(ctx context.Context, to string, order OrderEmailData) error {
	html, err := e.renderTemplate("order_confirmation", order)
	if err != nil {
		return err
	}

	_, err = e.Send(ctx, EmailRequest{
		To:      []string{to},
		Subject: fmt.Sprintf("Confirmación de orden #%s", order.OrderNumber),
		HTML:    html,
		Tags: []EmailTag{
			{Name: "type", Value: "order_confirmation"},
			{Name: "order", Value: order.OrderNumber},
		},
	})
	return err
}

// SendTickets sends tickets email with PDF attachment.
// Uses the embedded v1 template "tickets_with_pdfs.html" which expects
// lowercase/snake_case variables (event_name, user_name, ticket_type_name…).
func (e *EmailService) SendTickets(ctx context.Context, to string, data TicketEmailData, pdfContent []byte) error {
	// Adapt the struct into the lowercase keys that the v1 template uses.
	eventImage := data.EventImage
	if eventImage == "" {
		// Cheap default so the <img> in the template doesn't 404.
		eventImage = "https://images.unsplash.com/photo-1492684223066-81342ee5ff30?w=1200&q=80"
	}
	// Los QR inline van como template.URL: html/template bloquea data-URIs
	// "no confiables" en src (los sustituye por #ZgotmplZ) y los QR nunca se
	// pintaban. Son PNGs generados por nosotros — confiables.
	ticketRows := make([]map[string]interface{}, 0, len(data.Tickets))
	for _, tk := range data.Tickets {
		ticketRows = append(ticketRows, map[string]interface{}{
			"ID": tk.ID, "Type": tk.Type, "OwnerName": tk.OwnerName,
			"QRCode": tk.QRCode, "QRImageDataURL": template.URL(tk.QRImageDataURL),
		})
	}
	payload := map[string]interface{}{
		"event_name":       data.EventName,
		"event_date":       data.EventDate,
		"event_time":       data.EventTime,
		"event_image":      eventImage,
		"user_name":        data.CustomerName,
		"venue_name":       data.VenueName,
		"venue_location":   data.EventLocation,
		"ticket_type_name": data.TicketType,
		"quantity":         len(data.Tickets),
		"order_number":     data.OrderNumber,
		"currency":         data.Currency,
		"total":            data.Total,
		"tickets":          ticketRows,
	}

	// Prefer the v1 template; fall back to the inline "tickets" template if
	// the embed didn't pick it up for some reason.
	// IMPORTANTE: compilar ANTES de consultar el mapa — se compila lazy en el
	// primer render, así que sin esto el PRIMER email tras cada arranque veía
	// el mapa vacío y caía al fallback (que además salía con campos vacíos).
	compileTemplatesOnce.Do(compileAllTemplates)
	tmplName := "tickets_with_pdfs"
	if _, ok := compiledTemplates[tmplName]; !ok {
		tmplName = "tickets"
	}
	html, err := e.renderTemplate(tmplName, payload)
	if err != nil {
		return err
	}

	attachments := []EmailAttachment{}
	if len(pdfContent) > 0 {
		attachments = append(attachments, EmailAttachment{
			Filename:    fmt.Sprintf("tickets_%s.pdf", data.OrderNumber),
			Content:     encodeBase64(pdfContent),
			ContentType: "application/pdf",
		})
	}

	_, err = e.Send(ctx, EmailRequest{
		To:          []string{to},
		Subject:     fmt.Sprintf("Tus entradas para %s", data.EventName),
		HTML:        html,
		Attachments: attachments,
		Tags: []EmailTag{
			{Name: "type", Value: "tickets"},
			{Name: "event", Value: data.EventName},
		},
	})
	return err
}

// =============================================
// EMAIL DATA TYPES
// =============================================

// OrderEmailData for order confirmation emails
type OrderEmailData struct {
	OrderNumber   string
	CustomerName  string
	EventName     string
	EventDate     string
	EventLocation string
	VenueName     string
	TicketType    string
	Quantity      int
	Total         string
	Currency      string
}

// TicketEmailData for ticket emails
type TicketEmailData struct {
	OrderNumber   string
	CustomerName  string
	EventName     string
	EventDate     string
	EventTime     string
	EventImage    string
	EventLocation string
	VenueName     string
	TicketType    string
	Currency      string
	Total         string
	Tickets       []TicketData
}

// TicketData individual ticket info. QRImageDataURL is a base64-encoded PNG
// data URL (data:image/png;base64,...) that the email HTML can embed inline.
type TicketData struct {
	ID             string
	Type           string
	OwnerName      string
	QRCode         string
	QRImageDataURL string
}

// =============================================
// TEMPLATE RENDERING (Pre-compiled for performance)
// =============================================

// Compiled templates cache
var (
	compiledTemplates    map[string]*template.Template
	compileTemplatesOnce sync.Once
)

// Email templates (raw strings)
var emailTemplates = map[string]string{
	"order_confirmation": `
<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
</head>
<body style="font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; margin: 0; padding: 20px; background-color: #f5f5f5;">
    <div style="max-width: 600px; margin: 0 auto; background: white; border-radius: 12px; padding: 40px; box-shadow: 0 2px 8px rgba(0,0,0,0.1);">
        <h1 style="color: #1a1a1a; margin: 0 0 10px; font-size: 24px;">¡Orden confirmada!</h1>
        <p style="color: #666; margin: 0 0 30px; font-size: 16px;">Gracias por tu compra, {{.CustomerName}}</p>

        <div style="background: #f8f9fa; border-radius: 8px; padding: 20px; margin-bottom: 20px;">
            <p style="margin: 0 0 5px; color: #999; font-size: 12px;">NÚMERO DE ORDEN</p>
            <p style="margin: 0; color: #1a1a1a; font-size: 18px; font-weight: bold;">{{.OrderNumber}}</p>
        </div>

        <h2 style="color: #1a1a1a; margin: 30px 0 15px; font-size: 18px;">Detalles del evento</h2>
        <table style="width: 100%; border-collapse: collapse;">
            <tr>
                <td style="padding: 10px 0; color: #666;">Evento</td>
                <td style="padding: 10px 0; color: #1a1a1a; text-align: right; font-weight: 500;">{{.EventName}}</td>
            </tr>
            <tr>
                <td style="padding: 10px 0; color: #666; border-top: 1px solid #eee;">Fecha</td>
                <td style="padding: 10px 0; color: #1a1a1a; text-align: right; border-top: 1px solid #eee;">{{.EventDate}}</td>
            </tr>
            <tr>
                <td style="padding: 10px 0; color: #666; border-top: 1px solid #eee;">Ubicación</td>
                <td style="padding: 10px 0; color: #1a1a1a; text-align: right; border-top: 1px solid #eee;">{{.EventLocation}}</td>
            </tr>
            <tr>
                <td style="padding: 10px 0; color: #666; border-top: 1px solid #eee;">Tipo</td>
                <td style="padding: 10px 0; color: #1a1a1a; text-align: right; border-top: 1px solid #eee;">{{.TicketType}}</td>
            </tr>
            <tr>
                <td style="padding: 10px 0; color: #666; border-top: 1px solid #eee;">Cantidad</td>
                <td style="padding: 10px 0; color: #1a1a1a; text-align: right; border-top: 1px solid #eee;">{{.Quantity}}</td>
            </tr>
            <tr>
                <td style="padding: 15px 0; color: #1a1a1a; font-weight: bold; border-top: 2px solid #1a1a1a;">Total</td>
                <td style="padding: 15px 0; color: #1a1a1a; text-align: right; font-weight: bold; font-size: 18px; border-top: 2px solid #1a1a1a;">{{.Currency}} {{.Total}}</td>
            </tr>
        </table>

        <p style="color: #999; margin: 30px 0 0; font-size: 14px; text-align: center;">Recibirás tus entradas en un correo separado.</p>
    </div>
</body>
</html>
`,
	"tickets": `
<!DOCTYPE html>
<html lang="es">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
</head>
<body style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;margin:0;padding:24px;background:#0a0a0f;color:#fff;">
  <div style="max-width:600px;margin:0 auto;background:#15151f;border:1px solid #2a2a3a;border-radius:16px;padding:36px;">

    <div style="font-size:12px;letter-spacing:3px;color:#8b5cf6;font-weight:700;">PULL EVENTS</div>
    <h1 style="font-size:26px;margin:8px 0 8px;color:#fff;">Tus entradas están listas</h1>
    <p style="color:#a0a0b0;margin:0 0 28px;font-size:15px;">Hola {{.user_name}}, gracias por tu compra. Aquí tienes los detalles de tu orden.</p>

    <div style="background:linear-gradient(135deg,rgba(99,102,241,0.12),rgba(139,92,246,0.06));border-left:3px solid #6366f1;padding:16px 18px;border-radius:10px;margin-bottom:24px;">
      <div style="font-size:11px;color:#8b8b9b;letter-spacing:1.2px;margin-bottom:4px;">ORDEN</div>
      <div style="font-size:18px;font-weight:700;color:#fff;">{{.order_number}}</div>
    </div>

    <table style="width:100%;border-collapse:collapse;font-size:14px;margin-bottom:28px;">
      <tr>
        <td style="padding:10px 0;color:#a0a0b0;">Evento</td>
        <td style="padding:10px 0;text-align:right;color:#fff;font-weight:500;">{{.event_name}}</td>
      </tr>
      <tr>
        <td style="padding:10px 0;color:#a0a0b0;border-top:1px solid #2a2a3a;">Fecha</td>
        <td style="padding:10px 0;text-align:right;color:#fff;border-top:1px solid #2a2a3a;">{{.event_date}}</td>
      </tr>
      <tr>
        <td style="padding:10px 0;color:#a0a0b0;border-top:1px solid #2a2a3a;">Hora</td>
        <td style="padding:10px 0;text-align:right;color:#fff;border-top:1px solid #2a2a3a;">{{.event_time}}</td>
      </tr>
      <tr>
        <td style="padding:10px 0;color:#a0a0b0;border-top:1px solid #2a2a3a;">Lugar</td>
        <td style="padding:10px 0;text-align:right;color:#fff;border-top:1px solid #2a2a3a;">{{.venue_name}}</td>
      </tr>
      <tr>
        <td style="padding:10px 0;color:#a0a0b0;border-top:1px solid #2a2a3a;">Tipo</td>
        <td style="padding:10px 0;text-align:right;color:#fff;border-top:1px solid #2a2a3a;">{{.ticket_type_name}}</td>
      </tr>
      <tr>
        <td style="padding:14px 0;font-weight:700;color:#fff;border-top:2px solid #6366f1;">Total</td>
        <td style="padding:14px 0;text-align:right;font-weight:700;font-size:18px;color:#fff;border-top:2px solid #6366f1;">{{.currency}} {{.total}}</td>
      </tr>
    </table>

    <h2 style="font-size:16px;color:#fff;margin:8px 0 16px;">Códigos QR de tus entradas</h2>
    {{range $i, $t := .tickets}}
    <div style="background:#1a1a24;border:1px solid #2a2a3a;border-radius:14px;padding:18px;margin-bottom:14px;display:flex;align-items:center;gap:18px;">
      <div style="background:#fff;padding:6px;border-radius:8px;flex-shrink:0;">
        <img src="{{$t.QRImageDataURL}}" alt="QR" width="120" height="120" style="display:block;width:120px;height:120px;"/>
      </div>
      <div style="flex:1;">
        <div style="font-size:11px;color:#8b8b9b;letter-spacing:1px;">ENTRADA {{$t.Type}}</div>
        <div style="font-size:15px;color:#fff;font-weight:600;margin-top:4px;">{{$t.OwnerName}}</div>
        <div style="font-family:monospace;font-size:11px;color:#6b6b7b;margin-top:6px;word-break:break-all;">{{$t.QRCode}}</div>
      </div>
    </div>
    {{end}}

    <p style="color:#a0a0b0;margin:24px 0 4px;font-size:13px;">
      Adjuntamos también un PDF con tus entradas listas para imprimir. Presenta cada código QR en la puerta del evento.
    </p>
    <p style="color:#6b6b7b;font-size:11px;margin:18px 0 0;text-align:center;">
      Pull Events
    </p>
  </div>
</body>
</html>
`,
}

// compileAllTemplates pre-compiles every embedded *.html template plus the
// inline string templates (kept as fallback so legacy code paths keep
// working). File-based templates take precedence on name collision.
func compileAllTemplates() {
	compiledTemplates = make(map[string]*template.Template)

	// 1) Inline string templates (legacy v2 fallback).
	for name, tmplStr := range emailTemplates {
		if tmpl, err := template.New(name).Parse(tmplStr); err == nil {
			compiledTemplates[name] = tmpl
		} else {
			log.Printf("Warning: failed to compile inline email template %s: %v", name, err)
		}
	}

	// 2) Embedded *.html files override the inline versions when present.
	entries, err := fs.ReadDir(emailTemplatesFS, "templates")
	if err != nil {
		log.Printf("Warning: failed to list embedded templates: %v", err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fileName := entry.Name()
		raw, err := emailTemplatesFS.ReadFile("templates/" + fileName)
		if err != nil {
			log.Printf("Warning: failed to read embedded template %s: %v", fileName, err)
			continue
		}
		// Register under both the filename and the stem so callers can use
		// either "tickets_with_pdfs" or "tickets_with_pdfs.html".
		stem := fileName
		if len(stem) > 5 && stem[len(stem)-5:] == ".html" {
			stem = stem[:len(stem)-5]
		}
		tmpl, err := template.New(fileName).Parse(string(raw))
		if err != nil {
			log.Printf("Warning: failed to compile embedded template %s: %v", fileName, err)
			continue
		}
		compiledTemplates[fileName] = tmpl
		compiledTemplates[stem] = tmpl
	}
}

// renderTemplate renders an email template using pre-compiled templates
func (e *EmailService) renderTemplate(name string, data interface{}) (string, error) {
	// Compile templates once on first use
	compileTemplatesOnce.Do(compileAllTemplates)

	tmpl, ok := compiledTemplates[name]
	if !ok {
		return "", fmt.Errorf("template not found: %s", name)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	return buf.String(), nil
}

// encodeBase64 encodes bytes to base64
func encodeBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// =============================================
// VIP LIST EMAILS
// =============================================

// BuildVIPListInvitationEmail renders the VIP list invitation HTML (shared
// dark theme). Exported so preview tooling can render it without sending.
func BuildVIPListInvitationEmail(guestName, organizerName, listName, confirmURL string) string {
	esc := html.EscapeString
	body := emailGreeting("Hola ", esc(guestName)) +
		emailParagraph(fmt.Sprintf(
			`<strong style="color:#ffffff;">%s</strong> te ha invitado a unirte a su VIP list <strong style="color:#ffffff;">"%s"</strong>.`,
			esc(organizerName), esc(listName))) +
		emailButton(confirmURL, "Confirmar asistencia") +
		emailFineprint("Si no conoces a esta persona, puedes ignorar este correo.")

	return renderEmailShell(emailShellData{
		HTMLTitle: "Invitación VIP - Pull",
		AccentRGB: emailAccentPurple,
		Title:     "🎉 Invitación VIP",
		BodyHTML:  body,
	})
}

// SendVIPListInvitation sends invitation to join a VIP list
func (e *EmailService) SendVIPListInvitation(to, guestName, organizerName, listName, qrToken string) error {
	confirmURL := fmt.Sprintf("%s/vip-list/confirm/%s", config.App.FrontendURL, qrToken)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := e.Send(ctx, EmailRequest{
		To:      []string{to},
		Subject: fmt.Sprintf("%s te invita a su VIP list", organizerName),
		HTML:    BuildVIPListInvitationEmail(guestName, organizerName, listName, confirmURL),
		Tags: []EmailTag{
			{Name: "type", Value: "vip_invitation"},
		},
	})
	return err
}

// SendGuestConfirmation notifies organizer when a guest confirms
func (e *EmailService) SendGuestConfirmation(to, organizerName, guestName, listName string) error {
	html := fmt.Sprintf(`
<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
</head>
<body style="font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; margin: 0; padding: 20px; background-color: #f5f5f5;">
    <div style="max-width: 500px; margin: 0 auto; background: white; border-radius: 12px; padding: 40px; box-shadow: 0 2px 8px rgba(0,0,0,0.1);">
        <h1 style="color: #1a1a1a; margin: 0 0 20px; font-size: 24px;">✅ Invitado confirmado</h1>
        <p style="color: #666; margin: 0 0 20px; font-size: 16px;">Hola %s,</p>
        <p style="color: #666; margin: 0 0 30px; font-size: 16px;"><strong>%s</strong> ha confirmado su asistencia a tu VIP list <strong>"%s"</strong>.</p>
    </div>
</body>
</html>
`, organizerName, guestName, listName)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := e.Send(ctx, EmailRequest{
		To:      []string{to},
		Subject: fmt.Sprintf("%s confirmó su asistencia", guestName),
		HTML:    html,
		Tags: []EmailTag{
			{Name: "type", Value: "guest_confirmed"},
		},
	})
	return err
}

// BuildVIPListApprovedEmail renders the VIP list approved HTML (shared dark
// theme, green banner). Exported so preview tooling can render it without
// sending.
func BuildVIPListApprovedEmail(organizerName, listName, listURL string) string {
	esc := html.EscapeString
	body := emailGreeting("Hola ", esc(organizerName)) +
		emailParagraph(fmt.Sprintf(
			`Tu VIP list <strong style="color:#ffffff;">"%s"</strong> ha sido aprobada. Ya puedes invitar a tus amigos.`,
			esc(listName))) +
		emailButton(listURL, "Ver mi VIP list")

	return renderEmailShell(emailShellData{
		HTMLTitle: "VIP List aprobada - Pull",
		AccentRGB: emailAccentGreen,
		Title:     "🎉 VIP List aprobada",
		BodyHTML:  body,
	})
}

// SendVIPListApproved notifies organizer their VIP list was approved
func (e *EmailService) SendVIPListApproved(to, organizerName, listName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := e.Send(ctx, EmailRequest{
		To:      []string{to},
		Subject: fmt.Sprintf("Tu VIP list \"%s\" fue aprobada", listName),
		HTML:    BuildVIPListApprovedEmail(organizerName, listName, config.App.FrontendURL+"/my-vip-lists"),
		Tags: []EmailTag{
			{Name: "type", Value: "vip_approved"},
		},
	})
	return err
}

// BuildVIPListRejectedEmail renders the VIP list rejected HTML (shared dark
// theme, red banner). Exported so preview tooling can render it without
// sending.
func BuildVIPListRejectedEmail(organizerName, listName, reason string) string {
	esc := html.EscapeString
	reasonText := ""
	if reason != "" {
		reasonText = emailParagraph(fmt.Sprintf("Motivo: %s", esc(reason)))
	}

	body := emailGreeting("Hola ", esc(organizerName)) +
		emailParagraph(fmt.Sprintf(
			`Lamentamos informarte que tu VIP list <strong style="color:#ffffff;">"%s"</strong> no fue aprobada.`,
			esc(listName))) +
		reasonText +
		emailFineprint("Si tienes dudas, contacta al venue.")

	return renderEmailShell(emailShellData{
		HTMLTitle: "VIP List no aprobada - Pull",
		AccentRGB: emailAccentRed,
		Title:     "VIP List no aprobada",
		BodyHTML:  body,
	})
}

// SendVIPListRejected notifies organizer their VIP list was rejected
func (e *EmailService) SendVIPListRejected(to, organizerName, listName, reason string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := e.Send(ctx, EmailRequest{
		To:      []string{to},
		Subject: fmt.Sprintf("Tu VIP list \"%s\" no fue aprobada", listName),
		HTML:    BuildVIPListRejectedEmail(organizerName, listName, reason),
		Tags: []EmailTag{
			{Name: "type", Value: "vip_rejected"},
		},
	})
	return err
}

// BuildGuestListConfirmationEmail renders the guest list signup confirmation
// HTML (shared dark theme, green banner). Exported so preview tooling can
// render it without sending.
func BuildGuestListConfirmationEmail(guestName, listName, eventName, qrURL string) string {
	esc := html.EscapeString
	body := emailGreeting("Hola ", esc(guestName)) +
		emailParagraph(fmt.Sprintf(
			`Tu registro en la lista <strong style="color:#ffffff;">"%s"</strong> para <strong style="color:#ffffff;">%s</strong> ha sido confirmado.`,
			esc(listName), esc(eventName))) +
		emailButton(qrURL, "Ver mi QR") +
		emailFineprint("Presenta este QR en la entrada del evento.")

	return renderEmailShell(emailShellData{
		HTMLTitle: "Registro confirmado - Pull",
		AccentRGB: emailAccentGreen,
		Title:     "✅ Registro confirmado",
		BodyHTML:  body,
	})
}

// SendGuestListConfirmation sends confirmation for guest list signup
func (e *EmailService) SendGuestListConfirmation(to, guestName, eventName, listName, qrToken string) error {
	qrURL := fmt.Sprintf("%s/guest-list/qr/%s", config.App.FrontendURL, qrToken)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := e.Send(ctx, EmailRequest{
		To:      []string{to},
		Subject: fmt.Sprintf("Confirmación: %s - %s", listName, eventName),
		HTML:    BuildGuestListConfirmationEmail(guestName, listName, eventName, qrURL),
		Tags: []EmailTag{
			{Name: "type", Value: "guest_list_confirmation"},
		},
	})
	return err
}
