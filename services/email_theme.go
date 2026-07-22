package services

// =============================================
// SHARED DARK EMAIL THEME
// =============================================
//
// Every transactional email reuses the visual system of the ticket-delivery
// template (services/templates/tickets_with_pdfs.html): dark page (#0a0a12),
// purple-tinted logo header, a semantic status banner, a big white title,
// bordered accent cards for the details, and a discreet dark footer.
//
// Email-client constraints respected here: table layout only (no flexbox or
// grid), all styles inline, absolute image URLs, system font stack.

import (
	"fmt"
	"time"
)

// Semantic accent colors (RGB triplets, ready for rgba()/rgb() wrapping).
//
//	green  → confirmations / tickets delivered
//	amber  → pending approval
//	red    → rejection / expiry
//	purple → neutral / informational (brand color)
const (
	emailAccentGreen  = "52, 211, 153"
	emailAccentAmber  = "251, 191, 36"
	emailAccentRed    = "248, 113, 113"
	emailAccentPurple = "139, 92, 246"

	emailFontStack = "-apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, 'Helvetica Neue', Arial, sans-serif"
	emailLogoURL   = "https://mwuppgpmlynfxyghkpzv.supabase.co/storage/v1/object/public/venue-images/pull/logo-email.png"
)

// emailShellData describes one themed email.
type emailShellData struct {
	// HTMLTitle goes in <title> (inbox metadata only). Usually the heading.
	HTMLTitle string
	// AccentRGB is the semantic color triplet (emailAccent* consts).
	AccentRGB string
	// BannerText, when non-empty, renders the colored status banner under the
	// logo header (like "Your Tickets Are Ready!" in the reference template).
	// When empty a slim accent bar is rendered instead so the email still
	// carries its semantic color without inventing new copy.
	BannerText string
	// Title is the big centered white heading. Optional.
	Title string
	// BodyHTML is the stack of content sections (paragraphs/cards) rendered
	// on the #141418 content area. Left-aligned by default.
	BodyHTML string
	// FooterNote is an optional second, dimmer footer line (kept from each
	// email's original sign-off text).
	FooterNote string
}

// renderEmailShell wraps the body sections with the shared dark chrome.
func renderEmailShell(d emailShellData) string {
	banner := fmt.Sprintf(
		`<tr><td style="height:6px;line-height:6px;font-size:0;background-color:rgba(%s, 0.5);">&nbsp;</td></tr>`,
		d.AccentRGB)
	if d.BannerText != "" {
		banner = fmt.Sprintf(
			`<tr><td style="padding:24px 48px;text-align:center;background-color:rgba(%s, 0.15);"><div style="font-family:%s;font-size:16px;font-weight:600;line-height:1.3;color:rgb(%s);">%s</div></td></tr>`,
			d.AccentRGB, emailFontStack, d.AccentRGB, d.BannerText)
	}

	title := ""
	if d.Title != "" {
		title = fmt.Sprintf(
			`<tr><td style="padding:40px 48px 8px;text-align:center;background-color:#141418;"><div style="font-family:%s;font-size:28px;font-weight:700;letter-spacing:-0.5px;line-height:1.25;color:#ffffff;">%s</div></td></tr>`,
			emailFontStack, d.Title)
	}

	footerNote := ""
	if d.FooterNote != "" {
		footerNote = fmt.Sprintf(
			`<div style="font-family:%s;font-size:11px;line-height:1;color:#5c5c66;color:rgba(255, 255, 255, 0.3);">%s</div>`,
			emailFontStack, d.FooterNote)
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="es">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta http-equiv="X-UA-Compatible" content="IE=edge">
  <title>%s</title>
</head>
<body style="margin:0;padding:0;background-color:#0a0a12;word-spacing:normal;-webkit-text-size-adjust:100%%;-ms-text-size-adjust:100%%;">
  <table width="100%%" cellpadding="0" cellspacing="0" border="0" role="presentation" style="border-collapse:collapse;background-color:#0a0a12;">
    <tr>
      <td align="center" style="padding:32px 12px;">
        <table width="600" cellpadding="0" cellspacing="0" border="0" role="presentation" style="border-collapse:collapse;max-width:600px;width:100%%;">
          <!-- Logo header -->
          <tr>
            <td style="padding:48px 48px 40px;text-align:center;background-color:rgba(139, 92, 246, 0.08);">
              <img src="%s" alt="PULL EVENTS" width="200" style="border:0;display:inline-block;outline:none;text-decoration:none;height:auto;width:200px;max-width:200px;color:#a78bfa;font-size:20px;font-weight:bold;" />
            </td>
          </tr>
          <!-- Status banner -->
          %s
          <!-- Title -->
          %s
          <!-- Body -->
          <tr>
            <td style="padding:24px 48px 40px;background-color:#141418;">
              %s
            </td>
          </tr>
          <!-- Footer -->
          <tr>
            <td style="padding:24px 48px;text-align:center;background-color:rgba(0, 0, 0, 0.4);">
              <div style="font-family:%s;font-size:12px;font-weight:500;line-height:1;color:#77777f;color:rgba(255, 255, 255, 0.4);margin-bottom:8px;">&copy; %d Pull. Todos los derechos reservados.</div>
              %s
            </td>
          </tr>
        </table>
      </td>
    </tr>
  </table>
</body>
</html>`, d.HTMLTitle, emailLogoURL, banner, title, d.BodyHTML, emailFontStack, time.Now().Year(), footerNote)
}

// emailParagraph renders a left-aligned body paragraph. innerHTML may contain
// inline markup (already escaped by the caller where needed).
func emailParagraph(innerHTML string) string {
	return fmt.Sprintf(
		`<p style="margin:0 0 16px;font-family:%s;font-size:15px;font-weight:400;line-height:1.7;color:#bfbfc6;color:rgba(255, 255, 255, 0.75);text-align:left;">%s</p>`,
		emailFontStack, innerHTML)
}

// emailGreeting renders the "Hola <name>," line with the highlighted name
// (purple #a78bfa, like the reference greeting).
func emailGreeting(prefixHTML, name string) string {
	return fmt.Sprintf(
		`<p style="margin:0 0 16px;font-family:%s;font-size:16px;line-height:1.6;color:#ffffff;text-align:left;">%s<span style="color:rgb(167, 139, 250);font-weight:600;">%s</span>,</p>`,
		emailFontStack, prefixHTML, name)
}

// emailFineprint renders a small dim note.
func emailFineprint(innerHTML string) string {
	return fmt.Sprintf(
		`<p style="margin:16px 0 0;font-family:%s;font-size:13px;font-weight:400;line-height:1.6;color:#8a8a94;color:rgba(255, 255, 255, 0.5);text-align:left;">%s</p>`,
		emailFontStack, innerHTML)
}

// emailAccentCard renders a centered, accent-tinted bordered card (the
// "Event Details" card treatment from the reference template).
func emailAccentCard(accentRGB, innerHTML string) string {
	return fmt.Sprintf(
		`<table width="100%%" cellpadding="0" cellspacing="0" border="0" role="presentation" style="border-collapse:separate;margin:0 0 24px;"><tr><td style="background-color:rgba(%s, 0.08);border:1px solid rgba(%s, 0.2);border-radius:16px;padding:24px;">%s</td></tr></table>`,
		accentRGB, accentRGB, innerHTML)
}

// emailInfoCard renders the purple-tinted "Important Information" card.
func emailInfoCard(innerHTML string) string {
	return fmt.Sprintf(
		`<table width="100%%" cellpadding="0" cellspacing="0" border="0" role="presentation" style="border-collapse:separate;margin:0 0 24px;"><tr><td style="background-color:rgba(139, 92, 246, 0.05);border:1px solid rgba(139, 92, 246, 0.15);border-radius:14px;padding:24px;">%s</td></tr></table>`,
		innerHTML)
}

// emailCardLabel renders the small uppercase label centered at the top of a
// card ("EVENT DETAILS" style).
func emailCardLabel(text string) string {
	return fmt.Sprintf(
		`<div style="font-family:%s;font-size:11px;font-weight:600;letter-spacing:1.2px;line-height:1;text-align:center;text-transform:uppercase;color:#93939d;color:rgba(255, 255, 255, 0.55);margin-bottom:16px;">%s</div>`,
		emailFontStack, text)
}

// emailDetailField renders one label-above-value field inside a details card.
func emailDetailField(label, value string) string {
	return fmt.Sprintf(
		`<div style="font-family:%s;font-size:13px;line-height:1;text-align:left;color:#8a8a94;color:rgba(255, 255, 255, 0.5);padding:0 0 4px;">%s</div><div style="font-family:%s;font-size:15px;font-weight:500;line-height:1.4;text-align:left;color:#ffffff;padding:0 0 16px;">%s</div>`,
		emailFontStack, label, emailFontStack, value)
}

// emailBigValue renders a large highlighted value (amount, code) centered.
func emailBigValue(value string) string {
	return fmt.Sprintf(
		`<div style="font-family:%s;font-size:28px;font-weight:700;line-height:1.2;text-align:center;color:#ffffff;">%s</div>`,
		emailFontStack, value)
}

// emailCode renders a big monospace one-time code, centered.
func emailCode(code string) string {
	return fmt.Sprintf(
		`<div style="font-family:'Courier New', Courier, monospace;font-size:32px;font-weight:700;letter-spacing:8px;line-height:1.2;text-align:center;color:#ffffff;">%s</div>`,
		code)
}

// emailButton renders the brand call-to-action button (purple gradient, like
// the buttons on the group reservation emails), centered.
func emailButton(href, label string) string {
	return fmt.Sprintf(
		`<table width="100%%" cellpadding="0" cellspacing="0" border="0" role="presentation" style="border-collapse:collapse;margin:8px 0 24px;"><tr><td align="center"><a href="%s" style="display:inline-block;background-color:rgb(139, 92, 246);background-image:linear-gradient(135deg, rgb(139, 92, 246), rgb(124, 58, 237));color:#ffffff;text-decoration:none;padding:14px 32px;border-radius:10px;font-family:%s;font-weight:600;font-size:15px;">%s</a></td></tr></table>`,
		href, emailFontStack, label)
}
