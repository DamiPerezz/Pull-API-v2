package services

import (
	"os"
	"testing"
)

// Smoke test: los acentos deben sobrevivir al render (cp1252 translator).
func TestPDFAccentsSmoke(t *testing.T) {
	p := &PDFService{}
	b, err := p.GenerateMultiTicketPDF([]TicketPDFData{{
		EventName:     "511 Test Night — Sábado",
		EventDate:     "2026-07-25",
		EventTime:     "21:00:00",
		VenueName:     "Ciudad de Guatemala",
		VenueLocation: "Zona Viva, Ciudad de Guatemala",
		TicketType:    "General",
		OwnerName:     "Damián Pérez Ñoño",
		OrderNumber:   "ORD-TEST-ACCENTS",
		TicketID:      "abc12345",
		QRCode:        "abc12345token",
	}})
	if err != nil {
		t.Fatal(err)
	}
	out := os.Getenv("PDF_SMOKE_OUT")
	if out == "" {
		t.Skip("set PDF_SMOKE_OUT to write the PDF")
	}
	if err := os.WriteFile(out, b, 0644); err != nil {
		t.Fatal(err)
	}
}
