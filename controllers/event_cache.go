package controllers

import (
	"context"
	"sync"
	"time"

	"pull-api-v2/services"
)

// =============================================
// Caches ligeros para el pico del evento — evitan re-consultar Supabase
// (free tier, pool limitado) en caminos calientes: push por compra y el
// panel Control refrescando cada 30s en varios móviles.
// =============================================

var eventNameCache = struct {
	sync.RWMutex
	m map[string]string
}{m: map[string]string{}}

// cachedEventName resuelve el nombre del evento una vez y lo cachea (los
// nombres no cambian durante el evento). Evita una query por cada push.
func cachedEventName(ctx context.Context, venueDB *services.SupabaseClient, eventID string) string {
	if eventID == "" || venueDB == nil {
		return ""
	}
	eventNameCache.RLock()
	if n, ok := eventNameCache.m[eventID]; ok {
		eventNameCache.RUnlock()
		return n
	}
	eventNameCache.RUnlock()

	name := ""
	if ev, _ := venueDB.QueryOne(ctx, "events", map[string]interface{}{
		"select": "name", "where": map[string]interface{}{"id": eventID},
	}); ev != nil {
		name = services.GetString(ev, "name")
	}
	eventNameCache.Lock()
	eventNameCache.m[eventID] = name
	eventNameCache.Unlock()
	return name
}

// statsCacheEntry es un snapshot del panel Control con su instante.
type statsCacheEntry struct {
	at   time.Time
	data map[string]interface{}
}

var statsCache = struct {
	sync.Mutex
	m map[string]statsCacheEntry
}{m: map[string]statsCacheEntry{}}

const statsCacheTTL = 20 * time.Second

// getCachedStats devuelve el snapshot cacheado si es fresco (<20s). El panel
// refresca cada 30s en varios móviles; sin cache eran 6 queries pesadas por
// móvil cada vez, compitiendo con el checkout por el pool de Supabase.
func getCachedStats(key string) (map[string]interface{}, bool) {
	statsCache.Lock()
	defer statsCache.Unlock()
	if e, ok := statsCache.m[key]; ok && time.Since(e.at) < statsCacheTTL {
		return e.data, true
	}
	return nil, false
}

func setCachedStats(key string, data map[string]interface{}) {
	statsCache.Lock()
	statsCache.m[key] = statsCacheEntry{at: time.Now(), data: data}
	statsCache.Unlock()
}
