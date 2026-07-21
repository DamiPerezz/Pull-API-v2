package services

import (
	"html/template"
	"strings"
	"testing"
)

// Payload REAL de SendTickets (claves lowercase v1) — ambas plantillas deben
// renderizar con él sin campos vacíos.
func ticketsPayload() map[string]interface{} {
	return map[string]interface{}{
		"event_name": "511 Test Night", "event_date": "2026-07-25",
		"event_time": "21:00", "event_image": "https://x/y.jpg",
		"user_name": "Damián Pérez", "venue_name": "511 Events",
		"venue_location": "Ciudad de Guatemala", "ticket_type_name": "General",
		"quantity": 1, "order_number": "ORD-TEST", "currency": "GTQ", "total": "108.00",
		// Como lo construye SendTickets: maps con el data-URI tipado
		// template.URL (si no, html/template lo bloquea con #ZgotmplZ).
		"tickets": []map[string]interface{}{{"ID": "t1", "Type": "General",
			"OwnerName": "Damián", "QRCode": "abc",
			"QRImageDataURL": template.URL("data:image/png;base64,xxx")}},
	}
}

// La plantilla v1 principal (la que va con el PDF adjunto).
func TestTicketsWithPDFsTemplate(t *testing.T) {
	e := &EmailService{}
	html, err := e.renderTemplate("tickets_with_pdfs", ticketsPayload())
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}
	for _, want := range []string{"511 Test Night", "Damián Pérez", "511 Events", "General", "2026-07-25"} {
		if !strings.Contains(html, want) {
			t.Errorf("falta %q en tickets_with_pdfs", want)
		}
	}
}

// El fallback inline: sus claves deben coincidir con el payload (salía con
// TODOS los campos vacíos por usar claves CamelCase).
func TestInlineTicketsFallbackTemplate(t *testing.T) {
	e := &EmailService{}
	html, err := e.renderTemplate("tickets", ticketsPayload())
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}
	for _, want := range []string{"Damián Pérez", "ORD-TEST", "511 Test Night", "108.00", "data:image/png;base64,xxx"} {
		if !strings.Contains(html, want) {
			t.Errorf("falta %q en el fallback inline", want)
		}
	}
}
