package controllers

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"pull-api-v2/middleware"
	"pull-api-v2/services"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// =============================================
// LEGACY COMPATIBILITY LAYER
// =============================================
//
// PullWebApp-GL was built against the Pull-API-Go (v1) URL scheme. Rather than
// refactor every controller in the frontend, this file maps the v1 paths to
// the v2 multi-tenant logic.
//
// The handlers below resolve the venue (from slug or query param) and call
// into the venue DB exactly as the v2 endpoints do, then enrich the response
// with the legacy field aliases the frontend expects.

// resolveVenueIDFromSlug looks up the central venues table for a venue id.
func resolveVenueIDFromSlug(ctx context.Context, slug string) (string, error) {
	central := services.DB.Central()
	if central == nil {
		return "", fmt.Errorf("central db unavailable")
	}
	v, err := central.QueryOne(ctx, "venues", map[string]interface{}{
		"select": "id",
		"where": map[string]interface{}{
			"slug":       slug,
			"is_active":  true,
			"deleted_at": "is.null",
		},
	})
	if err != nil || v == nil {
		return "", fmt.Errorf("venue not found")
	}
	return services.GetString(v, "id"), nil
}

// =============================================
// VENUES
// =============================================

// LegacyGetAllVenues handles GET /api/v1/venues/get-all-venues.
// Mirrors GetVenues output but at the legacy path.
func LegacyGetAllVenues(c *gin.Context) {
	GetVenues(c)
}

// LegacyGetVenueInfo handles GET /api/v1/venues/events/get-venue-info/:slug.
// Returns the venue row directly with legacy field aliases the
// PullWebApp-GL VenueEventInfo type expects.
func LegacyGetVenueInfo(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	slug := c.Param("slug")
	central := services.DB.Central()
	v, err := central.QueryOne(ctx, "venues", map[string]interface{}{
		"select": "*",
		"where": map[string]interface{}{
			"slug":       slug,
			"is_active":  true,
			"deleted_at": "is.null",
		},
	})
	if err != nil || v == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Venue not found"})
		return
	}
	enrichVenueForLegacyFrontend(v)
	c.JSON(http.StatusOK, v)
}

// enrichVenueForLegacyFrontend adds the legacy aliases that the
// PullWebApp-GL VenueEventInfo type reads (`email`, `long_location`).
func enrichVenueForLegacyFrontend(v map[string]interface{}) {
	if v == nil {
		return
	}
	// email -> prefer email column, fall back to contact_email.
	if e := services.GetString(v, "email"); e == "" {
		if ce := services.GetString(v, "contact_email"); ce != "" {
			v["email"] = ce
		}
	}
	// long_location -> the full address (Zone + street) for display.
	if _, exists := v["long_location"]; !exists {
		addr := services.GetString(v, "address")
		loc := services.GetString(v, "location")
		city := services.GetString(v, "city")
		parts := []string{}
		if addr != "" {
			parts = append(parts, addr)
		} else if loc != "" {
			parts = append(parts, loc)
		}
		if city != "" {
			parts = append(parts, city)
		}
		long := ""
		for i, p := range parts {
			if i > 0 {
				long += ", "
			}
			long += p
		}
		v["long_location"] = long
	}
}

// LegacyGetVenueDescription handles GET /api/v1/venues/get-venue-description/:venueName.
// Returns just the venue's description text.
func LegacyGetVenueDescription(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	slug := c.Param("venueName")
	central := services.DB.Central()
	v, _ := central.QueryOne(ctx, "venues", map[string]interface{}{
		"select": "name,description,image,cover_image",
		"where":  map[string]interface{}{"slug": slug, "is_active": true, "deleted_at": "is.null"},
	})
	if v == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Venue not found"})
		return
	}
	c.JSON(http.StatusOK, v)
}

// LegacyGetEventVenueInfo handles GET /api/v1/venues/get-event-venue-info/:venueId.
func LegacyGetEventVenueInfo(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueID := c.Param("venueId")
	central := services.DB.Central()
	v, _ := central.QueryOne(ctx, "venues", map[string]interface{}{
		"select": "*",
		"where":  map[string]interface{}{"id": venueID, "deleted_at": "is.null"},
	})
	if v == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Venue not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"venue": v})
}

// LegacyGetReservationTypes handles GET /api/v1/venues/get-reservation-types/:venueId.
// Returns the configured flow flags for the venue.
func LegacyGetReservationTypes(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueID := c.Param("venueId")
	central := services.DB.Central()
	v, _ := central.QueryOne(ctx, "venues", map[string]interface{}{
		"select": "use_guest_list,use_vip_list_flow,use_group_reservations",
		"where":  map[string]interface{}{"id": venueID, "deleted_at": "is.null"},
	})
	if v == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Venue not found"})
		return
	}
	c.JSON(http.StatusOK, v)
}

// =============================================
// EVENTS
// =============================================

// LegacyGetAllVenueEvents handles GET /api/v1/venues/events/get-all-events/:slug.
// Returns the list of upcoming, published events for a venue.
func LegacyGetAllVenueEvents(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	slug := c.Param("slug")
	venueID, err := resolveVenueIDFromSlug(ctx, slug)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Venue not found"})
		return
	}
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Venue database unavailable"})
		return
	}

	events, err := venueDB.QueryCtx(ctx, "events", map[string]interface{}{
		"select": services.EventSelectColumns,
		"where": map[string]interface{}{
			"status":     services.PublishedEventStatuses,
			"deleted_at": "is.null",
		},
		"order": "start_datetime.asc",
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get events"})
		return
	}
	// Resolve venue name once and inject into every event card.
	venueRow, _ := services.DB.Central().QueryOne(ctx, "venues", map[string]interface{}{
		"select": "name,latitude,longitude,address,location",
		"where":  map[string]interface{}{"id": venueID},
	})
	venueName := ""
	if venueRow != nil {
		venueName = services.GetString(venueRow, "name")
	}

	services.EnrichEvents(events)
	for i := range events {
		events[i]["venue_id"] = venueID
		if venueName != "" {
			events[i]["venue_name"] = venueName
		}
		// EventCard uses `event.custom_location` as a venue display string in
		// some places — when the event's own location is empty, fall back to
		// the venue name so the icon row isn't blank.
		if cl := services.GetString(events[i], "custom_location"); cl == "" {
			events[i]["custom_location"] = venueName
		}
	}

	// Legacy frontend expects a plain array, not a wrapped object.
	c.JSON(http.StatusOK, events)
}

// LegacyGetEventInfo handles GET /api/v1/event/get-event-info/:slug.
// Returns a single event by slug, scanning across all venues.
func LegacyGetEventInfo(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	slugOrID := c.Param("slug")
	venueIDQuery := c.Query("venue_id")

	venueID := venueIDQuery
	if venueID == "" {
		// Fallback: assume single-venue (Aurora Hall demo). Pick the first venue.
		central := services.DB.Central()
		v, _ := central.QueryOne(ctx, "venues", map[string]interface{}{
			"select": "id",
			"where":  map[string]interface{}{"is_active": true, "deleted_at": "is.null"},
			"limit":  1,
		})
		if v == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "No venue found"})
			return
		}
		venueID = services.GetString(v, "id")
	}

	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Venue database unavailable"})
		return
	}

	// Try by slug first, then by id
	event, err := venueDB.QueryOne(ctx, "events", map[string]interface{}{
		"select": services.EventSelectColumns,
		"where":  map[string]interface{}{"slug": slugOrID, "deleted_at": "is.null"},
	})
	if event == nil {
		event, err = venueDB.QueryOne(ctx, "events", map[string]interface{}{
			"select": services.EventSelectColumns,
			"where":  map[string]interface{}{"id": slugOrID, "deleted_at": "is.null"},
		})
	}
	if err != nil || event == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Event not found"})
		return
	}
	services.EnrichEvent(event)
	event["venue_id"] = venueID
	c.JSON(http.StatusOK, gin.H{"event": event})
}

// flattenEventForLegacyDetail injects the additional aliases the
// EventDetailedInfo type expects (date / open_time / close_time / location).
func flattenEventForLegacyDetail(ev map[string]interface{}, venue map[string]interface{}) {
	if v, ok := ev["event_date"]; ok && ev["date"] == nil {
		ev["date"] = v
	}
	if v, ok := ev["start_time"]; ok && ev["open_time"] == nil {
		ev["open_time"] = v
	}
	if v, ok := ev["end_time"]; ok && ev["close_time"] == nil {
		ev["close_time"] = v
	}
	if venue != nil {
		enrichVenueForLegacyFrontend(venue)
		if v, ok := venue["name"]; ok {
			if _, exists := ev["venue_name"]; !exists {
				ev["venue_name"] = v
			}
		}
		if ev["location"] == nil {
			if v, ok := venue["location"]; ok {
				ev["location"] = v
			}
		}
		// custom_location is a VenueInfo-shaped object the EventInfoCard uses
		// to build a Google Maps deep-link, so it needs lat/long.
		ev["custom_location"] = map[string]interface{}{
			"name":          services.GetString(venue, "name"),
			"location":      services.GetString(venue, "location"),
			"address":       services.GetString(venue, "address"),
			"long_location": services.GetString(venue, "long_location"),
			"latitude":      venue["latitude"],
			"longitude":     venue["longitude"],
			"email":         services.GetString(venue, "email"),
			"image":         services.GetString(venue, "image"),
		}
	}
}

// LegacyGetDetailedEventInfo handles GET /api/v1/event/get-detailed-event-info/:slug.
// Returns the event row directly (flattened) — the legacy frontend deserialises
// the response body straight into EventDetailedInfo.
func LegacyGetDetailedEventInfo(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	slugOrID := c.Param("slug")

	central := services.DB.Central()
	venue, _ := central.QueryOne(ctx, "venues", map[string]interface{}{
		"select": "*",
		"where":  map[string]interface{}{"is_active": true, "deleted_at": "is.null"},
		"limit":  1,
	})
	if venue == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "No venue found"})
		return
	}
	venueID := services.GetString(venue, "id")
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Venue database unavailable"})
		return
	}

	event, _ := venueDB.QueryOne(ctx, "events", map[string]interface{}{
		"select": services.EventSelectColumns,
		"where":  map[string]interface{}{"slug": slugOrID, "deleted_at": "is.null"},
	})
	if event == nil {
		event, _ = venueDB.QueryOne(ctx, "events", map[string]interface{}{
			"select": services.EventSelectColumns,
			"where":  map[string]interface{}{"id": slugOrID, "deleted_at": "is.null"},
		})
	}
	if event == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Event not found"})
		return
	}
	services.EnrichEvent(event)
	eventID := services.GetString(event, "id")

	var ticketTypes, vipTypes []map[string]interface{}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ticketTypes, _ = venueDB.QueryCtx(ctx, "ticket_types", map[string]interface{}{
			"select": services.TicketTypeSelectColumns,
			"where":  map[string]interface{}{"event_id": eventID, "is_active": true},
			"order":  "sort_order.asc,price.asc",
		})
	}()
	go func() {
		defer wg.Done()
		vipTypes, _ = venueDB.QueryCtx(ctx, "event_vip_ticket_types", map[string]interface{}{
			"select": services.VIPTicketTypeSelectColumns,
			"where":  map[string]interface{}{"event_id": eventID, "is_active": true},
			"order":  "sort_order.asc",
		})
	}()
	wg.Wait()
	services.EnrichTicketTypes(ticketTypes)
	services.EnrichVIPTicketTypes(vipTypes)

	// Inject event/venue context into each ticket type so the frontend
	// can build purchase URLs without an extra round trip.
	evSlug := services.GetString(event, "slug")
	for i := range ticketTypes {
		ticketTypes[i]["slug"] = evSlug
		ticketTypes[i]["event_id"] = eventID
		ticketTypes[i]["venue_id"] = venueID
	}

	event["venue_id"] = venueID
	event["venue"] = venue
	event["ticket_types"] = ticketTypes
	event["vip_ticket_types"] = vipTypes
	flattenEventForLegacyDetail(event, venue)

	// Legacy frontend deserialises straight into EventDetailedInfo (flat shape).
	c.JSON(http.StatusOK, event)
}

// LegacyGetTicketTypes handles GET /api/v1/ticket-type/get-ticket-types/:eventSlug.
func LegacyGetTicketTypes(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	slug := c.Param("eventSlug")

	central := services.DB.Central()
	venue, _ := central.QueryOne(ctx, "venues", map[string]interface{}{
		"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
	})
	if venue == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Venue not found"})
		return
	}
	venueID := services.GetString(venue, "id")
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Venue database unavailable"})
		return
	}

	event, _ := venueDB.QueryOne(ctx, "events", map[string]interface{}{
		"select": "id,slug",
		"where":  map[string]interface{}{"slug": slug, "deleted_at": "is.null"},
	})
	if event == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Event not found"})
		return
	}
	eventID := services.GetString(event, "id")
	eventSlug := services.GetString(event, "slug")

	var ticketTypes, vipTypes []map[string]interface{}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ticketTypes, _ = venueDB.QueryCtx(ctx, "ticket_types", map[string]interface{}{
			"select": services.TicketTypeSelectColumns,
			"where":  map[string]interface{}{"event_id": eventID, "is_active": true},
			"order":  "sort_order.asc,price.asc",
		})
	}()
	go func() {
		defer wg.Done()
		vipTypes, _ = venueDB.QueryCtx(ctx, "event_vip_ticket_types", map[string]interface{}{
			"select": services.VIPTicketTypeSelectColumns,
			"where":  map[string]interface{}{"event_id": eventID, "is_active": true},
			"order":  "sort_order.asc",
		})
	}()
	wg.Wait()
	services.EnrichTicketTypes(ticketTypes)
	services.EnrichVIPTicketTypes(vipTypes)

	// The legacy frontend builds purchase URLs from `ticket.slug` (which
	// historically was the event slug embedded in each ticket type row).
	for i := range ticketTypes {
		ticketTypes[i]["slug"] = eventSlug
		ticketTypes[i]["event_id"] = eventID
		ticketTypes[i]["venue_id"] = venueID
	}

	// Inject a virtual "group ticket" (is_group=true) derived from the first
	// event_vip_ticket_type. The PullWebApp-GL group setup page calls
	// `ticketTypes.find(t => t.is_group)` to discover MEN_PRICE / WOMEN_PRICE
	// for table reservations, so we surface one synthetic row alongside the
	// regular tickets.
	if vt := pickPrimaryVIPType(vipTypes); vt != nil {
		groupTicket := map[string]interface{}{
			"ticket_type_id":     services.GetString(vt, "id"),
			"id":                 services.GetString(vt, "id"),
			"slug":               eventSlug,
			"event_id":           eventID,
			"venue_id":           venueID,
			"ticket_name":        services.GetString(vt, "name"),
			"name":               services.GetString(vt, "name"),
			"ticket_description": services.GetString(vt, "description"),
			"description":        services.GetString(vt, "description"),
			"currency":           "GTQ",
			"is_group":           true,
			"has_gender_pricing": true,
			"male_price":         services.GetFloat64(vt, "price_male"),
			"female_price":       services.GetFloat64(vt, "price_female"),
			"price_male":         services.GetFloat64(vt, "price_male"),
			"price_female":       services.GetFloat64(vt, "price_female"),
			"ticket_price":       services.GetFloat64(vt, "price_male"),
			"price":              services.GetFloat64(vt, "price_male"),
			"min_quantity":       4,
			"max_quantity":       services.GetInt(vt, "quantity_total"),
			"ticket_quantity":    services.GetInt(vt, "quantity_total") - services.GetInt(vt, "quantity_sold"),
			"available_quantity": services.GetInt(vt, "quantity_total") - services.GetInt(vt, "quantity_sold"),
			"initial_quantity":   services.GetInt(vt, "quantity_total"),
			"is_active":          true,
		}
		ticketTypes = append(ticketTypes, groupTicket)
	}

	// Legacy frontend treats the response as a flat array of TicketType.
	c.JSON(http.StatusOK, ticketTypes)
}

// pickPrimaryVIPType returns the VIP ticket type that should drive the
// virtual is_group ticket. We prefer the lowest-tier (Mesa Premium) over the
// premium one when both exist.
func pickPrimaryVIPType(vipTypes []map[string]interface{}) map[string]interface{} {
	if len(vipTypes) == 0 {
		return nil
	}
	var best map[string]interface{}
	for _, v := range vipTypes {
		if best == nil {
			best = v
			continue
		}
		if services.GetFloat64(v, "price_male") < services.GetFloat64(best, "price_male") {
			best = v
		}
	}
	return best
}

// LegacyGetTicketInfo handles GET /api/v1/ticket-type/get-ticket-info/:eventSlug/:ticketTypeId.
// Returns a single ticket type with event + venue context for the pre-purchase page.
func LegacyGetTicketInfo(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	eventSlug := c.Param("eventSlug")
	ticketTypeID := c.Param("ticketTypeId")
	if eventSlug == "" || ticketTypeID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing params"})
		return
	}

	central := services.DB.Central()
	venue, _ := central.QueryOne(ctx, "venues", map[string]interface{}{
		"select": "*", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
	})
	if venue == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Venue not found"})
		return
	}
	venueID := services.GetString(venue, "id")
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Venue database unavailable"})
		return
	}

	event, _ := venueDB.QueryOne(ctx, "events", map[string]interface{}{
		"select": services.EventSelectColumns,
		"where":  map[string]interface{}{"slug": eventSlug, "deleted_at": "is.null"},
	})
	if event == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Event not found"})
		return
	}
	services.EnrichEvent(event)

	tt, _ := venueDB.QueryOne(ctx, "ticket_types", map[string]interface{}{
		"select": services.TicketTypeSelectColumns,
		"where":  map[string]interface{}{"id": ticketTypeID, "is_active": true},
	})
	if tt == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Ticket type not found"})
		return
	}
	services.EnrichTicketType(tt)
	tt["event_id"] = services.GetString(event, "id")
	tt["venue_id"] = venueID
	tt["slug"] = eventSlug
	tt["event"] = event
	tt["venue"] = venue
	tt["event_name"] = event["event_name"]
	tt["event_date"] = event["event_date"]
	tt["event_img"] = event["event_img"]
	tt["venue_name"] = services.GetString(venue, "name")

	// Legacy frontend deserialises the response straight into TicketType.
	c.JSON(http.StatusOK, tt)
}

// LegacyGetGuestListsByEvent handles GET /api/v1/guest-lists/event/:eventSlug.
// Returns the guest_list_types for the event as a plain array; frontend does
// .filter(gl => gl.is_active).
func LegacyGetGuestListsByEvent(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	slug := c.Param("eventSlug")
	if slug == "" {
		slug = c.Param("event_id")
	}

	central := services.DB.Central()
	venue, _ := central.QueryOne(ctx, "venues", map[string]interface{}{
		"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
	})
	if venue == nil {
		c.JSON(http.StatusOK, []map[string]interface{}{})
		return
	}
	venueID := services.GetString(venue, "id")
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusOK, []map[string]interface{}{})
		return
	}

	// Find event by slug or id
	event, _ := venueDB.QueryOne(ctx, "events", map[string]interface{}{
		"select": "id", "where": map[string]interface{}{"slug": slug, "deleted_at": "is.null"},
	})
	if event == nil {
		event, _ = venueDB.QueryOne(ctx, "events", map[string]interface{}{
			"select": "id", "where": map[string]interface{}{"id": slug, "deleted_at": "is.null"},
		})
	}
	if event == nil {
		c.JSON(http.StatusOK, []map[string]interface{}{})
		return
	}
	eventID := services.GetString(event, "id")

	guestLists, _ := venueDB.QueryCtx(ctx, "guest_list_types", map[string]interface{}{
		"select": "id,event_id,name,description,max_signups,current_signups,benefits,is_active,signup_start,signup_end,created_at",
		"where":  map[string]interface{}{"event_id": eventID, "is_active": true},
	})
	if guestLists == nil {
		guestLists = []map[string]interface{}{}
	}
	c.JSON(http.StatusOK, guestLists)
}

// =============================================
// ORDERS (legacy payment flow)
// =============================================

// LegacyCreatePendingOrder handles POST /api/v1/orders/create-pending-order.
// The PullWebApp-GL payload looks like:
//
//	{
//	  event_id, ticket_type_id, total, currency,
//	  user_name, user_email,
//	  tickets_data: [{ticket_type_id, ticket_type_name, quantity, price,
//	                  owner_name, owner_last_name, owner_email, owner_phone,
//	                  owner_phone_prefix, owner_gender, owner_birthdate}]
//	}
//
// We don't get venue_id or top-level quantity; we infer venue_id from the
// configured default venue and derive quantity from len(tickets_data).
type legacyPendingOrderRequest struct {
	EventID      string                   `json:"event_id"`
	EventSlug    string                   `json:"event_slug"`
	TicketTypeID string                   `json:"ticket_type_id"`
	Quantity     int                      `json:"quantity"`
	VenueID      string                   `json:"venue_id"`
	VenueSlug    string                   `json:"venue_slug"`
	UserName     string                   `json:"user_name"`
	UserEmail    string                   `json:"user_email"`
	Gender       string                   `json:"gender"`
	Total        float64                  `json:"total"`
	Currency     string                   `json:"currency"`
	TicketsData  []map[string]interface{} `json:"tickets_data"`
}

func LegacyCreatePendingOrder(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	var req legacyPendingOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request", "details": err.Error()})
		return
	}

	// SECURITY: endpoint PÚBLICO. Los ids llegan a where-clauses de PostgREST;
	// sin validar, un valor como "not.is.null" inyecta un operador. safeLookupCode
	// rechaza cualquier cosa que no sea [A-Za-z0-9_-]. Vacío se permite (se
	// resuelve por slug o fallback más abajo).
	for _, v := range []string{req.TicketTypeID, req.EventID, req.VenueID} {
		if v != "" && !safeLookupCode(v) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Identificador inválido"})
			return
		}
	}

	// Derive missing fields from tickets_data when the frontend doesn't send them
	// at the top level.
	if req.Quantity <= 0 {
		req.Quantity = len(req.TicketsData)
	}
	if req.Quantity <= 0 {
		req.Quantity = 1
	}
	if req.UserName == "" && len(req.TicketsData) > 0 {
		fn := services.GetString(req.TicketsData[0], "owner_name")
		ln := services.GetString(req.TicketsData[0], "owner_last_name")
		req.UserName = fn
		if ln != "" {
			req.UserName += " " + ln
		}
	}
	if req.UserEmail == "" && len(req.TicketsData) > 0 {
		req.UserEmail = services.GetString(req.TicketsData[0], "owner_email")
	}
	if req.UserEmail == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_email is required"})
		return
	}

	// Resolve venue ID from slug if needed
	venueID := req.VenueID
	if venueID == "" && req.VenueSlug != "" {
		if id, err := resolveVenueIDFromSlug(ctx, req.VenueSlug); err == nil {
			venueID = id
		}
	}
	if venueID == "" {
		// Fallback to first active venue (Aurora Hall demo)
		central := services.DB.Central()
		v, _ := central.QueryOne(ctx, "venues", map[string]interface{}{
			"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
		})
		if v == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "venue_id is required"})
			return
		}
		venueID = services.GetString(v, "id")
	}

	// Resolve event ID from slug if needed
	eventID := req.EventID
	if eventID == "" && req.EventSlug != "" {
		venueDB := services.DB.ForVenue(venueID)
		if venueDB != nil {
			ev, _ := venueDB.QueryOne(ctx, "events", map[string]interface{}{
				"select": "id",
				"where":  map[string]interface{}{"slug": req.EventSlug, "deleted_at": "is.null"},
			})
			if ev != nil {
				eventID = services.GetString(ev, "id")
			}
		}
	}
	if eventID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "event_id or event_slug is required"})
		return
	}

	// Translate to v2 CreateOrder body shape and delegate.
	c.Set("__legacy_translated", true)
	c.Request.Body = nil
	body := map[string]interface{}{
		"event_id":       eventID,
		"ticket_type_id": req.TicketTypeID,
		"quantity":       req.Quantity,
		"venue_id":       venueID,
		"user_name":      req.UserName,
		"user_email":     req.UserEmail,
		"gender":         req.Gender,
	}
	c.Set("__legacy_body", body)

	// Call CreateOrder logic inline (we need a clean handler chain).
	// Simpler: replicate the minimal essential v2 CreateOrder logic here.
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	ticketType, _ := venueDB.QueryOne(ctx, "ticket_types", map[string]interface{}{
		"select": services.TicketTypeSelectColumns,
		"where":  map[string]interface{}{"id": req.TicketTypeID, "is_active": true},
	})
	if ticketType == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Ticket type not found"})
		return
	}
	services.EnrichTicketType(ticketType)

	minQty := services.GetInt(ticketType, "min_quantity")
	maxQty := services.GetInt(ticketType, "max_quantity")
	if req.Quantity < minQty || req.Quantity > maxQty {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Quantity must be between %d and %d", minQty, maxQty)})
		return
	}
	// La disponibilidad se comprueba y RESERVA de forma ATÓMICA más abajo (RPC
	// reserve_ticket_type), justo antes de crear la orden — un check aquí sería
	// TOCTOU (dos compradores simultáneos leerían el mismo "quedan N" y ambos
	// pasarían = sobreventa).

	unitPrice := services.GetFloat64(ticketType, "price")
	subtotal := unitPrice * float64(req.Quantity)

	// Server-authoritative total: subtotal + the venue's service fee
	// (venues.platform_fee_percent — the WebApp mirrors the same percent for
	// display). The client-sent total is ignored on purpose.
	total := subtotal
	if venue, err := services.DB.GetVenue(ctx, venueID); err == nil && venue.PlatformFeePercent > 0 {
		total = round2(subtotal * (1 + venue.PlatformFeePercent/100))
	}

	// Get or create user
	user, _ := venueDB.QueryOne(ctx, "public_users", map[string]interface{}{
		"select": "id",
		"where":  map[string]interface{}{"email": req.UserEmail},
	})
	var userID string
	if user == nil {
		newUser, err := venueDB.InsertCtx(ctx, "public_users", map[string]interface{}{
			"email": req.UserEmail,
			"name":  req.UserName,
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create user"})
			return
		}
		userID = services.GetString(newUser, "id")
	} else {
		userID = services.GetString(user, "id")
	}

	// RESERVA ATÓMICA del aforo ANTES de crear la orden. La RPC hace el
	// check+incremento de quantity_reserved en una sola sentencia Postgres →
	// imposible sobrevender aunque N compradores lleguen a la vez. Devuelve las
	// disponibles restantes, o -1 si no hay stock.
	rpcRes, rpcErr := venueDB.CallRPC(ctx, "reserve_ticket_type", map[string]interface{}{
		"p_id": req.TicketTypeID, "p_qty": req.Quantity,
	})
	if rpcErr != nil {
		log.Printf("[CreatePendingOrder] ALERT reserve RPC failed tt=%s qty=%d: %v", req.TicketTypeID, req.Quantity, rpcErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "No se pudo reservar el aforo. Intenta de nuevo."})
		return
	}
	if remaining, _ := rpcRes.(float64); remaining < 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "No quedan suficientes entradas disponibles.", "sold_out": true})
		return
	}
	// A partir de aquí el aforo está RESERVADO: si algo falla antes de dejar la
	// orden pendiente, hay que LIBERARLo para no bloquear plazas fantasma.
	releaseReserved := func() {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer bgCancel()
		if _, e := venueDB.CallRPC(bgCtx, "release_ticket_type", map[string]interface{}{
			"p_id": req.TicketTypeID, "p_qty": req.Quantity,
		}); e != nil {
			log.Printf("[CreatePendingOrder] ALERT release tras fallo falló tt=%s qty=%d: %v", req.TicketTypeID, req.Quantity, e)
		}
	}

	paymentLinkCode, _ := generateRandomCode(16)
	expiresAt := time.Now().Add(30 * time.Minute)

	// Persist per-attendee details (incl. instagram) in the order metadata so
	// ConfirmPayment can carry them through to the ticket rows.
	metaForOrder := map[string]interface{}{"payment_link_code": paymentLinkCode}
	if len(req.TicketsData) > 0 {
		metaForOrder["tickets_data"] = req.TicketsData
	}

	order, err := venueDB.InsertCtx(ctx, "orders", map[string]interface{}{
		"event_id":       eventID,
		"ticket_type_id": req.TicketTypeID,
		"user_id":        userID,
		"quantity":       req.Quantity,
		"unit_price":     unitPrice,
		"subtotal":       subtotal,
		"total":          total,
		"currency":       "GTQ",
		"status":         "pending",
		"user_name":      req.UserName,
		"user_email":     req.UserEmail,
		"expires_at":     expiresAt.Format(time.RFC3339),
		"metadata":       metaForOrder,
	})
	if err != nil {
		releaseReserved() // no dejar el aforo reservado sin orden que lo respalde
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create order"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"success":           true,
		"order_id":          services.GetString(order, "id"),
		"order_number":      services.GetString(order, "order_number"),
		"payment_link_code": paymentLinkCode,
		"venue_id":          venueID,
		"event_id":          eventID,
		"quantity":          req.Quantity,
		"unit_price":        unitPrice,
		"total":             total,
		"currency":          "GTQ",
		"expires_at":        expiresAt.Format(time.RFC3339),
	})
}

// LegacyCreateCheckoutSession handles POST /api/v1/orders/create-checkout-session.
// Same as v2 CreateCheckout but with legacy field names.
type legacyCheckoutRequest struct {
	OrderID    string `json:"order_id"`
	VenueID    string `json:"venue_id"`
	VenueSlug  string `json:"venue_slug"`
	ReturnURL  string `json:"return_url"`
	CancelURL  string `json:"cancel_url"`
	SuccessURL string `json:"success_url"` // alias for ReturnURL
}

func LegacyCreateCheckoutSession(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	var req legacyCheckoutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}
	venueID := req.VenueID
	if venueID == "" && req.VenueSlug != "" {
		if id, err := resolveVenueIDFromSlug(ctx, req.VenueSlug); err == nil {
			venueID = id
		}
	}
	if venueID == "" {
		// Fallback to first active venue (Aurora Hall demo)
		v, _ := services.DB.Central().QueryOne(ctx, "venues", map[string]interface{}{
			"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
		})
		if v == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "venue_id or venue_slug required"})
			return
		}
		venueID = services.GetString(v, "id")
	}
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}

	order, err := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
		"select": "id,order_number,event_id,ticket_type_id,user_id,quantity,total,currency,status,user_email,user_name",
		"where":  map[string]interface{}{"id": req.OrderID, "status": "pending"},
	})
	if err != nil || order == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found or already processed"})
		return
	}

	processor, err := services.Payments.GetProcessor(ctx, venueID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Payment gateway not configured"})
		return
	}

	ticketType, _ := venueDB.QueryOne(ctx, "ticket_types", map[string]interface{}{
		"select": "name",
		"where":  map[string]interface{}{"id": services.GetString(order, "ticket_type_id")},
	})
	ticketTypeName := "Tickets"
	if ticketType != nil {
		ticketTypeName = services.GetString(ticketType, "name")
	}

	successURL := req.ReturnURL
	if successURL == "" {
		successURL = req.SuccessURL
	}

	checkout, err := processor.CreateCheckout(ctx, models_CheckoutParams(
		services.GetFloat64(order, "total"),
		services.GetString(order, "currency"),
		services.GetString(order, "id"),
		fmt.Sprintf("%d x %s", services.GetInt(order, "quantity"), ticketTypeName),
		services.GetString(order, "user_email"),
		successURL,
		req.CancelURL,
		venueID,
		services.GetString(order, "event_id"),
		services.GetString(order, "order_number"),
	))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Mark order as processing
	go func() {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer bgCancel()
		venueDB.UpdateNoReturn(bgCtx, "orders", map[string]interface{}{
			"status":            "processing",
			"stripe_session_id": checkout.SessionID,
			"payment_gateway":   processor.GetGateway().String(),
		}, map[string]interface{}{"id": req.OrderID})
	}()

	c.JSON(http.StatusOK, gin.H{
		"checkout_url": checkout.CheckoutURL,
		"session_id":   checkout.SessionID,
		"gateway":      processor.GetGateway().String(),
	})
}

// LegacyConfirmPayment handles GET /api/v1/orders/confirm-payment.
// Same logic as v2 ConfirmPayment; just a path alias.
func LegacyConfirmPayment(c *gin.Context) {
	ConfirmPayment(c)
}

// LegacySimulatePayment handles POST /api/v1/orders/simulate-payment.
//
// The PullWebApp-GL payment flow is:
//
//	createPendingOrder()  -> POST /orders/create-pending-order  (status=pending)
//	simulateStripePayment(orderId) -> POST /orders/simulate-payment
//
// — i.e. the WebApp never calls /orders/create-checkout-session, so the order
// has no stripe_session_id yet. We bridge the gap here: stamp a mock session
// on the order, flip it to processing, then delegate to ConfirmPayment to
// emit tickets exactly like the regular checkout flow.
func LegacySimulatePayment(c *gin.Context) {
	var req struct {
		SessionID string `json:"session_id"`
		VenueID   string `json:"venue_id"`
		VenueSlug string `json:"venue_slug"`
		OrderID   string `json:"order_id"`
	}
	_ = c.ShouldBindJSON(&req)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	venueID := req.VenueID
	if venueID == "" && req.VenueSlug != "" {
		if id, err := resolveVenueIDFromSlug(ctx, req.VenueSlug); err == nil {
			venueID = id
		}
	}
	if venueID == "" {
		v, _ := services.DB.Central().QueryOne(ctx, "venues", map[string]interface{}{
			"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
		})
		if v != nil {
			venueID = services.GetString(v, "id")
		}
	}
	if venueID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "venue_id required"})
		return
	}
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Venue database unavailable"})
		return
	}

	// When only order_id was provided, ensure the order has a session_id and
	// is in processing state so ConfirmPayment can pick it up.
	if req.OrderID != "" {
		ord, _ := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
			"select": "id,stripe_session_id,status",
			"where":  map[string]interface{}{"id": req.OrderID},
		})
		if ord == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
			return
		}
		req.SessionID = services.GetString(ord, "stripe_session_id")
		status := services.GetString(ord, "status")
		if req.SessionID == "" {
			session, _ := generateRandomCode(24)
			req.SessionID = "mock_" + session
		}
		if status == "pending" || status == "" {
			venueDB.UpdateNoReturn(ctx, "orders", map[string]interface{}{
				"status":            "processing",
				"stripe_session_id": req.SessionID,
				"payment_gateway":   "stripe",
			}, map[string]interface{}{"id": req.OrderID})
		}
	}

	// Rebuild query params for ConfirmPayment
	q := c.Request.URL.Query()
	q.Set("session_id", req.SessionID)
	q.Set("venue_id", venueID)
	c.Request.URL.RawQuery = q.Encode()
	ConfirmPayment(c)
}

// =============================================
// GUEST LIST (legacy)
// =============================================

type legacyGuestSignupRequest struct {
	EventSlug       string `json:"event_slug"`
	GuestListTypeID string `json:"guest_list_type_id"`
	Name            string `json:"name"`
	LastName        string `json:"last_name"`
	Email           string `json:"email"`
	Phone           string `json:"phone"`
	PhonePrefix     string `json:"phone_prefix"`
	Gender          string `json:"gender"`
	GuestCount      int    `json:"guest_count"`
	Instagram       string `json:"instagram"`
}

// LegacyGuestListSignup handles POST /api/v1/guest-lists/signup with the
// PullWebApp-GL legacy shape (event_slug + last_name + guest_count).
func LegacyGuestListSignup(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	var req legacyGuestSignupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request", "details": err.Error()})
		return
	}

	central := services.DB.Central()
	venue, _ := central.QueryOne(ctx, "venues", map[string]interface{}{
		"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
	})
	if venue == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Venue not found"})
		return
	}
	venueID := services.GetString(venue, "id")
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Venue database unavailable"})
		return
	}

	event, _ := venueDB.QueryOne(ctx, "events", map[string]interface{}{
		"select": "id",
		"where":  map[string]interface{}{"slug": req.EventSlug, "deleted_at": "is.null"},
	})
	if event == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Event not found"})
		return
	}

	code, _ := generateRandomCode(12)
	// The schema has no dedicated instagram column, so we stash it inside the
	// notes field with a discoverable prefix so admins/scripts can extract it.
	notes := ""
	if ig := strings.TrimSpace(req.Instagram); ig != "" {
		notes = "IG:" + ig
	}
	signup, err := venueDB.InsertCtx(ctx, "guest_list_signups", map[string]interface{}{
		"guest_list_type_id": req.GuestListTypeID,
		"event_id":           services.GetString(event, "id"),
		"name":               req.Name,
		"last_name":          req.LastName,
		"email":              req.Email,
		"phone":              req.Phone,
		"gender":             req.Gender,
		"plus_one_name":      nil,
		"qr_token":           code,
		"status":             "pending",
		"notes":              notes,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create signup", "details": err.Error()})
		return
	}

	// Fire-and-forget confirmation email.
	go func() {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer bgCancel()
		if services.Email != nil {
			eventName := ""
			if ev, _ := venueDB.QueryOne(bgCtx, "events", map[string]interface{}{
				"select": "name", "where": map[string]interface{}{"slug": req.EventSlug},
			}); ev != nil {
				eventName = services.GetString(ev, "name")
			}
			services.Email.SendGuestListConfirmation(req.Email, req.Name, eventName, "Lista del evento", code)
		}
	}()

	c.JSON(http.StatusCreated, gin.H{
		"success":           true,
		"signup_id":         services.GetString(signup, "id"),
		"verification_code": code,
		"message":           "Signup created. Awaiting approval.",
	})
}

// LegacyGetGuestListStatus handles GET /api/v1/guest-lists/status/:code.
func LegacyGetGuestListStatus(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	code := c.Param("code")

	central := services.DB.Central()
	venue, _ := central.QueryOne(ctx, "venues", map[string]interface{}{
		"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
	})
	if venue == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Venue not found"})
		return
	}
	venueDB := services.DB.ForVenue(services.GetString(venue, "id"))

	signup, _ := venueDB.QueryOne(ctx, "guest_list_signups", map[string]interface{}{
		"select": "*",
		"where":  map[string]interface{}{"qr_token": code},
	})
	if signup == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Signup not found"})
		return
	}
	c.JSON(http.StatusOK, signup)
}

// =============================================
// GROUP RESERVATIONS / BOTTLES / MIXERS (legacy)
// =============================================

// LegacyGetBottles handles GET /api/v1/group-reservations/bottles/:venueSlug.
func LegacyGetBottles(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	central := services.DB.Central()
	venue, _ := central.QueryOne(ctx, "venues", map[string]interface{}{
		"select": "id",
		"where":  map[string]interface{}{"slug": c.Param("venueSlug"), "is_active": true, "deleted_at": "is.null"},
	})
	if venue == nil {
		c.JSON(http.StatusOK, []map[string]interface{}{})
		return
	}
	venueDB := services.DB.ForVenue(services.GetString(venue, "id"))
	bottles, _ := venueDB.QueryCtx(ctx, "vip_bottles", map[string]interface{}{
		"select": "id,name,brand,category,size_ml,price,currency,image,description,sort_order",
		"where":  map[string]interface{}{"is_active": true},
		"order":  "sort_order.asc,name.asc",
	})
	if bottles == nil {
		bottles = []map[string]interface{}{}
	}
	c.JSON(http.StatusOK, bottles)
}

// LegacyGetMixers handles GET /api/v1/group-reservations/mixers/:venueSlug.
func LegacyGetMixers(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	central := services.DB.Central()
	venue, _ := central.QueryOne(ctx, "venues", map[string]interface{}{
		"select": "id",
		"where":  map[string]interface{}{"slug": c.Param("venueSlug"), "is_active": true, "deleted_at": "is.null"},
	})
	if venue == nil {
		c.JSON(http.StatusOK, []map[string]interface{}{})
		return
	}
	venueDB := services.DB.ForVenue(services.GetString(venue, "id"))
	mixers, _ := venueDB.QueryCtx(ctx, "vip_mixers", map[string]interface{}{
		"select": "id,name,price,currency,sort_order",
		"where":  map[string]interface{}{"is_active": true},
		"order":  "sort_order.asc",
	})
	if mixers == nil {
		mixers = []map[string]interface{}{}
	}
	c.JSON(http.StatusOK, mixers)
}

type legacyGroupReservationRequest struct {
	EventSlug     string `json:"event_slug"`
	GuestCount    int    `json:"guest_count"`
	OrganizerData struct {
		Name        string `json:"name"`
		LastName    string `json:"last_name"`
		Email       string `json:"email"`
		Phone       string `json:"phone"`
		PhonePrefix string `json:"phone_prefix"`
		BirthDate   string `json:"birth_date"`
		Gender      string `json:"gender"`
	} `json:"organizer_data"`
	Guests []struct {
		Name      string  `json:"name"`
		LastName  string  `json:"last_name"`
		Email     string  `json:"email"`
		Instagram string  `json:"instagram"`
		Gender    string  `json:"gender"`
		AmountDue float64 `json:"amount_due"`
		HostPays  bool    `json:"host_pays"`
	} `json:"guests"`
	Bottles []struct {
		BottleID string `json:"bottle_id"`
		Quantity int    `json:"quantity"`
	} `json:"bottles"`
	Mixers []struct {
		MixerID  string `json:"mixer_id"`
		Quantity int    `json:"quantity"`
	} `json:"mixers"`
	TotalAmount            float64 `json:"total_amount"`
	SpecialRequests        string  `json:"special_requests"`
	ReservationName        string  `json:"reservation_name"`
	ReservationDescription string  `json:"reservation_description"`
}

// LegacyCreateGroupReservation handles POST /api/v1/group-reservations/create.
// Creates the reservation, organizer + guests, bottles, mixers in one shot.
func LegacyCreateGroupReservation(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	var req legacyGroupReservationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request", "details": err.Error()})
		return
	}

	central := services.DB.Central()
	venue, _ := central.QueryOne(ctx, "venues", map[string]interface{}{
		"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
	})
	if venue == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Venue not found"})
		return
	}
	venueID := services.GetString(venue, "id")
	venueDB := services.DB.ForVenue(venueID)

	event, _ := venueDB.QueryOne(ctx, "events", map[string]interface{}{
		"select": "id", "where": map[string]interface{}{"slug": req.EventSlug, "deleted_at": "is.null"},
	})
	if event == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Event not found"})
		return
	}
	eventID := services.GetString(event, "id")

	// Ensure organizer user exists
	user, _ := venueDB.QueryOne(ctx, "public_users", map[string]interface{}{
		"select": "id",
		"where":  map[string]interface{}{"email": req.OrganizerData.Email},
	})
	var userID string
	if user == nil {
		newUser, err := venueDB.InsertCtx(ctx, "public_users", map[string]interface{}{
			"email":   req.OrganizerData.Email,
			"name":    req.OrganizerData.Name,
			"surname": req.OrganizerData.LastName,
			"phone":   req.OrganizerData.Phone,
			"gender":  nullableEnum(req.OrganizerData.Gender),
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create user"})
			return
		}
		userID = services.GetString(newUser, "id")
	} else {
		userID = services.GetString(user, "id")
	}

	// Apply defaults for the optional reservation metadata.
	resvName := strings.TrimSpace(req.ReservationName)
	if resvName == "" {
		resvName = "Mesa de " + req.OrganizerData.Name
	}
	resvDesc := strings.TrimSpace(req.ReservationDescription)
	if resvDesc == "" {
		resvDesc = fmt.Sprintf(
			"Reserva grupal de %d invitados creada por %s %s para el evento.",
			req.GuestCount, req.OrganizerData.Name, req.OrganizerData.LastName,
		)
	}

	// One shared code: it functions as the management_code, the payment_link
	// tracking token, and the suffix of reservation_number. The frontend's
	// tracking page calls /group-reservations/track/:paymentLinkCode and we
	// look it up by reservation_number, so they need to align.
	sharedCode, _ := generateRandomCode(12)
	managementCode := sharedCode
	paymentLinkCode := sharedCode

	// Create the reservation (using vip_list_reservations as the table for table
	// reservations — the venue DB has this schema for "mesa VIP / lista VIP").
	resv, err := venueDB.InsertCtx(ctx, "vip_list_reservations", map[string]interface{}{
		"event_id":           eventID,
		"organizer_id":       userID,
		"organizer_name":     req.OrganizerData.Name,
		"organizer_email":    req.OrganizerData.Email,
		"organizer_phone":    req.OrganizerData.Phone,
		"max_guests":         req.GuestCount,
		"total":              req.TotalAmount,
		"reservation_number": "GRP-" + sharedCode,
		"status":             "pending",
		"notes":              resvName + " — " + resvDesc,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create reservation", "details": err.Error()})
		return
	}
	reservationID := services.GetString(resv, "id")

	// Insert guests — Instagram (if provided) goes into the guest's `phone`
	// fallback when no phone was captured, otherwise it's noop-dropped (the
	// schema doesn't currently have a dedicated instagram column).
	for _, g := range req.Guests {
		row := map[string]interface{}{
			"reservation_id": reservationID,
			"name":           g.Name,
			"last_name":      g.LastName,
			"email":          g.Email,
			"gender":         nullableEnum(g.Gender),
			"ticket_price":   g.AmountDue,
			"has_paid":       g.HostPays,
		}
		if ig := strings.TrimSpace(g.Instagram); ig != "" {
			row["phone"] = "IG:" + ig
		}
		venueDB.InsertCtx(ctx, "vip_list_guests", row)
	}
	// Insert bottles (with required unit_price / total_price lookups).
	for _, b := range req.Bottles {
		bottle, _ := venueDB.QueryOne(ctx, "vip_bottles", map[string]interface{}{
			"select": "price",
			"where":  map[string]interface{}{"id": b.BottleID},
		})
		unitPrice := services.GetFloat64(bottle, "price")
		venueDB.InsertCtx(ctx, "vip_list_bottles", map[string]interface{}{
			"reservation_id": reservationID,
			"bottle_id":      b.BottleID,
			"quantity":       b.Quantity,
			"unit_price":     unitPrice,
			"total_price":    unitPrice * float64(b.Quantity),
		})
	}

	// Fire-and-forget confirmation email for the organizer with the
	// management/tracking link.
	go func() {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer bgCancel()
		if services.Email != nil {
			services.Email.SendGroupReservationConfirmation(bgCtx, services.GroupReservationEmailData{
				To:                req.OrganizerData.Email,
				OrganizerName:     req.OrganizerData.Name,
				EventName:         services.GetString(event, "name"),
				EventDate:         services.GetString(event, "event_date"),
				ReservationNumber: "GRP-" + managementCode,
				ManagementCode:    managementCode,
				PaymentLinkCode:   paymentLinkCode,
				GuestCount:        req.GuestCount,
				TotalAmount:       req.TotalAmount,
				Currency:          "GTQ",
			})
		}
	}()

	c.JSON(http.StatusCreated, gin.H{
		"success":            true,
		"reservation_id":     reservationID,
		"reservation_number": "GRP-" + managementCode,
		"management_code":    managementCode,
		"payment_link_code":  paymentLinkCode,
		"total_amount":       req.TotalAmount,
		"guest_count":        req.GuestCount,
		"message":            "Reserva creada exitosamente. Revisa tu correo.",
	})
}

// LegacyTrackGroupReservation handles GET /api/v1/group-reservations/track/:code.
func LegacyTrackGroupReservation(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	central := services.DB.Central()
	venue, _ := central.QueryOne(ctx, "venues", map[string]interface{}{
		"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
	})
	if venue == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Venue not found"})
		return
	}
	venueDB := services.DB.ForVenue(services.GetString(venue, "id"))
	rn := "GRP-" + c.Param("code")
	resv, _ := venueDB.QueryOne(ctx, "vip_list_reservations", map[string]interface{}{
		"select": "*", "where": map[string]interface{}{"reservation_number": rn},
	})
	if resv == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Reservation not found"})
		return
	}
	resvID := services.GetString(resv, "id")

	var guests, bottles []map[string]interface{}
	guests, _ = venueDB.QueryCtx(ctx, "vip_list_guests", map[string]interface{}{
		"select": "*", "where": map[string]interface{}{"reservation_id": resvID},
	})
	bottles, _ = venueDB.QueryCtx(ctx, "vip_list_bottles", map[string]interface{}{
		"select": "*", "where": map[string]interface{}{"reservation_id": resvID},
	})

	// Look up event context for the tracking UI.
	eventImage := ""
	if eid := services.GetString(resv, "event_id"); eid != "" {
		ev, _ := venueDB.QueryOne(ctx, "events", map[string]interface{}{
			"select": "image,cover_image,name,start_datetime,end_datetime,location",
			"where":  map[string]interface{}{"id": eid},
		})
		if ev != nil {
			eventImage = services.GetString(ev, "image")
			if eventImage == "" {
				eventImage = services.GetString(ev, "cover_image")
			}
			services.EnrichEvent(ev)
			resv["event_name"] = services.GetString(ev, "name")
			resv["event_date"] = services.GetString(ev, "event_date")
			resv["venue_name"] = services.GetString(ev, "location")
		}
	}

	if guests == nil {
		guests = []map[string]interface{}{}
	}
	if bottles == nil {
		bottles = []map[string]interface{}{}
	}
	pendingAmount := 0.0
	for _, g := range guests {
		enrichGroupGuest(g)
		if !services.GetBool(g, "has_paid") {
			pendingAmount += services.GetFloat64(g, "ticket_price")
		}
	}

	// Aliases the tracking page reads on the reservation object. status_id is
	// the legacy numeric status: the page treats 7/8 as "approved" and
	// anything else as still waiting for the venue.
	resv["guest_count"] = len(guests)
	if len(guests) == 0 {
		resv["guest_count"] = services.GetInt(resv, "max_guests")
	}
	resv["total_amount"] = services.GetFloat64(resv, "total")
	resv["pending_amount"] = pendingAmount
	switch services.GetString(resv, "status") {
	case "confirmed", "closed", "completed":
		resv["status_id"] = 7
	case "cancelled":
		resv["status_id"] = 9
	default:
		resv["status_id"] = 1
	}

	c.JSON(http.StatusOK, gin.H{
		"success":     true,
		"reservation": resv,
		"guests":      guests,
		"bottles":     bottles,
		"mixers":      []map[string]interface{}{},
		"event_image": eventImage,
	})
}

// enrichGroupGuest adds the aliases the WebApp tracking / guest-complete
// pages read on top of the raw vip_list_guests row:
//
//	ticket_price            -> amount_due
//	has_paid && !paid_at    -> host_pays (paid by the organizer at creation)
//	verification_code       -> "" (schema has no such column; the guest flow
//	                          works by unguessable guest UUID instead)
func enrichGroupGuest(g map[string]interface{}) {
	if g == nil {
		return
	}
	g["amount_due"] = services.GetFloat64(g, "ticket_price")
	g["host_pays"] = services.GetBool(g, "has_paid") && services.GetString(g, "paid_at") == ""
	if _, ok := g["verification_code"]; !ok {
		g["verification_code"] = ""
	}
}

// findGroupGuest loads a vip_list_guests row by id on the demo venue DB.
func findGroupGuest(ctx context.Context, guestID string) (*services.SupabaseClient, map[string]interface{}) {
	central := services.DB.Central()
	venue, _ := central.QueryOne(ctx, "venues", map[string]interface{}{
		"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
	})
	if venue == nil {
		return nil, nil
	}
	venueDB := services.DB.ForVenue(services.GetString(venue, "id"))
	if venueDB == nil {
		return nil, nil
	}
	guest, _ := venueDB.QueryOne(ctx, "vip_list_guests", map[string]interface{}{
		"select": "*", "where": map[string]interface{}{"id": guestID},
	})
	return venueDB, guest
}

// groupGuestContext resolves the reservation + event for a guest row and the
// shared payment link code.
func groupGuestContext(ctx context.Context, venueDB *services.SupabaseClient, guest map[string]interface{}) (resv, event map[string]interface{}, code string) {
	resv, _ = venueDB.QueryOne(ctx, "vip_list_reservations", map[string]interface{}{
		"select": "*", "where": map[string]interface{}{"id": services.GetString(guest, "reservation_id")},
	})
	if resv == nil {
		return nil, nil, ""
	}
	code = strings.TrimPrefix(services.GetString(resv, "reservation_number"), "GRP-")
	event, _ = venueDB.QueryOne(ctx, "events", map[string]interface{}{
		"select": "id,name,start_datetime,end_datetime,image,cover_image,min_age",
		"where":  map[string]interface{}{"id": services.GetString(resv, "event_id")},
	})
	if event != nil {
		services.EnrichEvent(event)
	}
	return resv, event, code
}

// nextActionForGuest decides which screen the guest-complete page shows.
func nextActionForGuest(guest map[string]interface{}) string {
	hasPaid := services.GetBool(guest, "has_paid")
	hostPays := hasPaid && services.GetString(guest, "paid_at") == ""
	dataComplete := services.GetString(guest, "email") != "" && services.GetString(guest, "phone") != ""
	switch {
	case hasPaid && !hostPays && dataComplete:
		return "ticket_ready"
	case hostPays && !dataComplete:
		return "complete_data"
	case hostPays && dataComplete:
		return "ticket_ready"
	case dataComplete:
		return "pay"
	default:
		return "complete_data_then_pay"
	}
}

// LegacyGetGroupGuest handles GET /group-reservations/guest/:guestId.
// Feeds the WebApp guest-complete page (each member fills their data and
// pays their share from the shared tracking link).
func LegacyGetGroupGuest(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	venueDB, guest := findGroupGuest(ctx, c.Param("guestId"))
	if guest == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Guest not found"})
		return
	}
	resv, event, code := groupGuestContext(ctx, venueDB, guest)
	if resv == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Reservation not found"})
		return
	}
	enrichGroupGuest(guest)

	eventPayload := map[string]interface{}{}
	if event != nil {
		eventPayload = map[string]interface{}{
			"name":       services.GetString(event, "name"),
			"event_name": services.GetString(event, "name"),
			"event_date": services.GetString(event, "event_date"),
			"start_time": services.GetString(event, "start_time"),
			"image":      services.GetString(event, "image"),
			"min_age":    event["min_age"],
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"guest":             guest,
		"event":             eventPayload,
		"reservation":       resv,
		"next_action":       nextActionForGuest(guest),
		"payment_link_code": code,
		// The demo flow relies on unguessable guest UUIDs; no access code.
		"requires_access_code": false,
	})
}

// LegacyCompleteGroupGuest handles POST /group-reservations/guest/:guestId/complete.
func LegacyCompleteGroupGuest(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	var req struct {
		Name        string `json:"name"`
		LastName    string `json:"last_name"`
		Email       string `json:"email"`
		Gender      string `json:"gender"`
		Phone       string `json:"phone"`
		PhonePrefix string `json:"phone_prefix"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}
	venueDB, guest := findGroupGuest(ctx, c.Param("guestId"))
	if guest == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Guest not found"})
		return
	}

	updates := map[string]interface{}{}
	if req.Name != "" {
		updates["name"] = middleware.SanitizeName(req.Name)
	}
	if req.LastName != "" {
		updates["last_name"] = middleware.SanitizeName(req.LastName)
	}
	if req.Email != "" {
		updates["email"] = middleware.SanitizeEmail(req.Email)
	}
	if req.Gender != "" {
		updates["gender"] = req.Gender
	}
	if req.Phone != "" {
		prefix := strings.TrimSpace(req.PhonePrefix)
		updates["phone"] = strings.TrimSpace(prefix + req.Phone)
	}
	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No data supplied"})
		return
	}
	result, err := venueDB.UpdateCtx(ctx, "vip_list_guests", updates, map[string]interface{}{
		"id": services.GetString(guest, "id"),
	})
	if err != nil || len(result) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save guest data"})
		return
	}
	enrichGroupGuest(result[0])
	c.JSON(http.StatusOK, gin.H{
		"success":     true,
		"guest":       result[0],
		"next_action": nextActionForGuest(result[0]),
	})
}

// LegacyPayGroupGuest handles POST /group-reservations/guest/:guestId/pay.
// DEMO_MODE: the payment is simulated — the guest is marked paid instantly.
func LegacyPayGroupGuest(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	venueDB, guest := findGroupGuest(ctx, c.Param("guestId"))
	if guest == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Guest not found"})
		return
	}
	if services.GetBool(guest, "has_paid") && services.GetString(guest, "paid_at") != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Este invitado ya pagó su parte"})
		return
	}

	result, err := venueDB.UpdateCtx(ctx, "vip_list_guests", map[string]interface{}{
		"has_paid": true,
		"paid_at":  time.Now().Format(time.RFC3339),
	}, map[string]interface{}{"id": services.GetString(guest, "id")})
	if err != nil || len(result) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process payment"})
		return
	}
	paid := result[0]

	resv, event, code := groupGuestContext(ctx, venueDB, paid)

	// Keep the reservation's paid-guest counter in sync (best effort).
	if resv != nil {
		if all, _ := venueDB.QueryCtx(ctx, "vip_list_guests", map[string]interface{}{
			"select": "id,has_paid",
			"where":  map[string]interface{}{"reservation_id": services.GetString(resv, "id")},
		}); len(all) > 0 {
			count := 0
			for _, g := range all {
				if services.GetBool(g, "has_paid") {
					count++
				}
			}
			venueDB.UpdateNoReturn(ctx, "vip_list_reservations", map[string]interface{}{
				"confirmed_guests": count,
			}, map[string]interface{}{"id": services.GetString(resv, "id")})
		}
	}

	eventName, eventDate, eventImage := "", "", ""
	if event != nil {
		eventName = services.GetString(event, "name")
		eventDate = services.GetString(event, "event_date")
		eventImage = services.GetString(event, "image")
	}
	c.JSON(http.StatusOK, gin.H{
		"success":           true,
		"guest_email":       services.GetString(paid, "email"),
		"guest_name":        strings.TrimSpace(services.GetString(paid, "name") + " " + services.GetString(paid, "last_name")),
		"event_name":        eventName,
		"event_date":        eventDate,
		"event_image":       eventImage,
		"amount_paid":       services.GetFloat64(paid, "ticket_price"),
		"payment_link_code": code,
	})
}

// LegacyVerifyGroupGuestAccessCode handles
// POST /group-reservations/guest/:guestId/verify-access-code. The demo does
// not use access codes (requires_access_code is always false), but the page
// keeps the call path — accept anything.
func LegacyVerifyGroupGuestAccessCode(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"valid": true})
}

// nullableEnum returns nil for empty strings (the schema enums reject "").
func nullableEnum(v string) interface{} {
	if v == "" {
		return nil
	}
	return v
}

// LegacyGetOrderDetails handles GET /api/v1/orders/details/:orderId.
func LegacyGetOrderDetails(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	orderID := c.Param("orderId")
	venueIDQuery := c.Query("venue_id")
	venueID := venueIDQuery
	if venueID == "" {
		central := services.DB.Central()
		v, _ := central.QueryOne(ctx, "venues", map[string]interface{}{
			"select": "id", "where": map[string]interface{}{"is_active": true, "deleted_at": "is.null"}, "limit": 1,
		})
		if v == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Venue not found"})
			return
		}
		venueID = services.GetString(v, "id")
	}
	venueDB := services.DB.ForVenue(venueID)
	if venueDB == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid venue"})
		return
	}
	order, err := venueDB.QueryOne(ctx, "orders", map[string]interface{}{
		"select": "*",
		"where":  map[string]interface{}{"id": orderID},
	})
	if err != nil || order == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Order not found"})
		return
	}

	// The mobile ReservaDetalle screen reads nested `user` and `event`
	// objects alongside the raw order row.
	name := services.GetString(order, "user_name")
	surname := ""
	if parts := strings.SplitN(name, " ", 2); len(parts) == 2 {
		name, surname = parts[0], parts[1]
	}
	user := map[string]interface{}{
		"name":    name,
		"surname": surname,
		"email":   services.GetString(order, "user_email"),
		"phone":   services.GetString(order, "user_phone"),
	}

	event := map[string]interface{}{}
	if eid := services.GetString(order, "event_id"); eid != "" {
		if ev, _ := venueDB.QueryOne(ctx, "events", map[string]interface{}{
			"select": "id,name,start_datetime,end_datetime,image,location",
			"where":  map[string]interface{}{"id": eid},
		}); ev != nil {
			services.EnrichEvent(ev)
			event = map[string]interface{}{
				"id":         services.GetString(ev, "id"),
				"name":       services.GetString(ev, "name"),
				"event_date": services.GetString(ev, "event_date"),
				"start_time": services.GetString(ev, "start_time"),
				"image":      services.GetString(ev, "image"),
				"location":   services.GetString(ev, "location"),
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{"order": order, "user": user, "event": event, "venue_id": venueID})
}
