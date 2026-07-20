package services

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// =============================================
// OPTIMIZED SUPABASE CLIENT
// Ultra-fast HTTP with connection pooling
// =============================================

// Buffer pool for JSON encoding (reduces GC pressure)
var bufferPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

// Compiled regex for table name validation (security)
var tableNameRegex = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// SupabaseClient handles Supabase REST API operations
type SupabaseClient struct {
	baseURL    string
	serviceKey string
	anonKey    string
	client     *http.Client
}

// =============================================
// SHARED HTTP CLIENT FOR CONNECTION POOLING
// CRITICAL: All Supabase clients MUST share this transport
// to enable TCP connection reuse across venues
// =============================================

var (
	// Shared transport for ALL Supabase clients (set by database_router)
	sharedSupabaseTransport *http.Transport
	sharedSupabaseClient    *http.Client
	sharedTransportOnce     sync.Once
)

// SetSharedTransport allows database_router to inject the shared transport
// This MUST be called before creating any Supabase clients
func SetSharedTransport(transport *http.Transport, client *http.Client) {
	sharedSupabaseTransport = transport
	sharedSupabaseClient = client
}

// getSharedClient returns the shared HTTP client or creates a default one
func getSharedClient() *http.Client {
	if sharedSupabaseClient != nil {
		return sharedSupabaseClient
	}

	// Fallback: create optimized client if shared not set
	sharedTransportOnce.Do(func() {
		sharedSupabaseTransport = &http.Transport{
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 64,
			MaxConnsPerHost:     128,
			IdleConnTimeout:     90 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 60 * time.Second,
			}).DialContext,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			DisableCompression:    false,
			ForceAttemptHTTP2:     true,
		}
		sharedSupabaseClient = &http.Client{
			Transport: sharedSupabaseTransport,
			Timeout:   30 * time.Second,
		}
	})

	return sharedSupabaseClient
}

// NewSupabaseClient creates a new Supabase client using the SHARED transport
// IMPORTANT: Uses shared connection pool for optimal performance
func NewSupabaseClient(baseURL, serviceKey, anonKey string) *SupabaseClient {
	return &SupabaseClient{
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		serviceKey: serviceKey,
		anonKey:    anonKey,
		client:     getSharedClient(), // Use shared client!
	}
}

// GetBaseURL returns the Supabase base URL
func (s *SupabaseClient) GetBaseURL() string {
	return s.baseURL
}

// GetServiceKey returns the service key (for storage operations)
func (s *SupabaseClient) GetServiceKey() string {
	return s.serviceKey
}

// =============================================
// QUERY METHODS (with context support)
// =============================================

// Query executes a SELECT query with default context
func (s *SupabaseClient) Query(table string, params map[string]interface{}) ([]map[string]interface{}, error) {
	return s.QueryCtx(context.Background(), table, params)
}

// QueryCtx executes a SELECT query with context for cancellation/timeout
func (s *SupabaseClient) QueryCtx(ctx context.Context, table string, params map[string]interface{}) ([]map[string]interface{}, error) {
	// Validate table name (SQL injection prevention)
	if !tableNameRegex.MatchString(table) {
		return nil, fmt.Errorf("invalid table name")
	}

	// Build URL
	reqURL := fmt.Sprintf("%s/rest/v1/%s", s.baseURL, table)

	// Build query parameters efficiently
	queryParams := s.buildQueryParams(params)
	if len(queryParams) > 0 {
		reqURL += "?" + queryParams.Encode()
	}

	// Create request with context
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, err
	}

	s.setHeaders(req)

	// Execute request
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Read response efficiently
	return s.parseResponse(resp)
}

// CountCtx returns the exact number of rows matching the filters WITHOUT
// fetching them: HEAD + "Prefer: count=exact" makes PostgREST answer with a
// Content-Range header ("0-0/123") and an empty body.
func (s *SupabaseClient) CountCtx(ctx context.Context, table string, where map[string]interface{}) (int, error) {
	if !tableNameRegex.MatchString(table) {
		return 0, fmt.Errorf("invalid table name")
	}
	reqURL := fmt.Sprintf("%s/rest/v1/%s", s.baseURL, table)
	queryParams := s.buildQueryParams(map[string]interface{}{
		"select": "id",
		"where":  where,
		"limit":  1,
	})
	if len(queryParams) > 0 {
		reqURL += "?" + queryParams.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, "HEAD", reqURL, nil)
	if err != nil {
		return 0, err
	}
	s.setHeaders(req)
	req.Header.Set("Prefer", "count=exact")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("count error %d", resp.StatusCode)
	}
	cr := resp.Header.Get("Content-Range") // "0-0/123" or "*/123"
	if i := strings.LastIndex(cr, "/"); i >= 0 && i+1 < len(cr) {
		return strconv.Atoi(cr[i+1:])
	}
	return 0, fmt.Errorf("missing content-range header")
}

// QueryOne returns a single result or nil
func (s *SupabaseClient) QueryOne(ctx context.Context, table string, params map[string]interface{}) (map[string]interface{}, error) {
	// Force limit 1
	params["limit"] = 1

	result, err := s.QueryCtx(ctx, table, params)
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, nil
	}
	return result[0], nil
}

// =============================================
// INSERT METHODS
// =============================================

// Insert inserts data and returns the created row
func (s *SupabaseClient) Insert(table string, data map[string]interface{}) (map[string]interface{}, error) {
	return s.InsertCtx(context.Background(), table, data)
}

// InsertCtx inserts with context support
func (s *SupabaseClient) InsertCtx(ctx context.Context, table string, data map[string]interface{}) (map[string]interface{}, error) {
	if !tableNameRegex.MatchString(table) {
		return nil, fmt.Errorf("invalid table name")
	}

	reqURL := fmt.Sprintf("%s/rest/v1/%s", s.baseURL, table)

	// Get buffer from pool
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	// Encode JSON
	if err := json.NewEncoder(buf).Encode(data); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", reqURL, buf)
	if err != nil {
		return nil, err
	}

	s.setHeaders(req)
	req.Header.Set("Prefer", "return=representation")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	result, err := s.parseResponse(resp)
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, nil
	}
	return result[0], nil
}

// InsertBatch inserts multiple rows efficiently (no return)
func (s *SupabaseClient) InsertBatch(ctx context.Context, table string, dataList []map[string]interface{}) error {
	if len(dataList) == 0 {
		return nil
	}
	if !tableNameRegex.MatchString(table) {
		return fmt.Errorf("invalid table name")
	}

	reqURL := fmt.Sprintf("%s/rest/v1/%s", s.baseURL, table)

	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	if err := json.NewEncoder(buf).Encode(dataList); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", reqURL, buf)
	if err != nil {
		return err
	}

	s.setHeaders(req)
	req.Header.Set("Prefer", "return=minimal") // No return = faster

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("insert error %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// =============================================
// UPDATE METHODS
// =============================================

// Update updates data matching the where clause
func (s *SupabaseClient) Update(table string, data map[string]interface{}, where map[string]interface{}) ([]map[string]interface{}, error) {
	return s.UpdateCtx(context.Background(), table, data, where)
}

// UpdateCtx updates with context support
func (s *SupabaseClient) UpdateCtx(ctx context.Context, table string, data map[string]interface{}, where map[string]interface{}) ([]map[string]interface{}, error) {
	if !tableNameRegex.MatchString(table) {
		return nil, fmt.Errorf("invalid table name")
	}

	queryParams := url.Values{}
	for key, value := range where {
		if !isValidColumnName(key) {
			return nil, fmt.Errorf("invalid column name: %s", key)
		}
		queryParams.Set(key, fmt.Sprintf("eq.%v", value))
	}

	reqURL := fmt.Sprintf("%s/rest/v1/%s?%s", s.baseURL, table, queryParams.Encode())

	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	if err := json.NewEncoder(buf).Encode(data); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "PATCH", reqURL, buf)
	if err != nil {
		return nil, err
	}

	s.setHeaders(req)
	req.Header.Set("Prefer", "return=representation")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return s.parseResponse(resp)
}

// UpdateNoReturn updates without returning data (faster)
func (s *SupabaseClient) UpdateNoReturn(ctx context.Context, table string, data map[string]interface{}, where map[string]interface{}) error {
	if !tableNameRegex.MatchString(table) {
		return fmt.Errorf("invalid table name")
	}

	queryParams := url.Values{}
	for key, value := range where {
		if !isValidColumnName(key) {
			return fmt.Errorf("invalid column name: %s", key)
		}
		queryParams.Set(key, fmt.Sprintf("eq.%v", value))
	}

	reqURL := fmt.Sprintf("%s/rest/v1/%s?%s", s.baseURL, table, queryParams.Encode())

	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	if err := json.NewEncoder(buf).Encode(data); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "PATCH", reqURL, buf)
	if err != nil {
		return err
	}

	s.setHeaders(req)
	req.Header.Set("Prefer", "return=minimal")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("update error %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// =============================================
// DELETE METHODS
// =============================================

// Delete deletes data matching the where clause
func (s *SupabaseClient) Delete(table string, where map[string]interface{}) error {
	return s.DeleteCtx(context.Background(), table, where)
}

// DeleteCtx deletes with context support
func (s *SupabaseClient) DeleteCtx(ctx context.Context, table string, where map[string]interface{}) error {
	if !tableNameRegex.MatchString(table) {
		return fmt.Errorf("invalid table name")
	}

	queryParams := url.Values{}
	for key, value := range where {
		if !isValidColumnName(key) {
			return fmt.Errorf("invalid column name: %s", key)
		}
		queryParams.Set(key, fmt.Sprintf("eq.%v", value))
	}

	reqURL := fmt.Sprintf("%s/rest/v1/%s?%s", s.baseURL, table, queryParams.Encode())

	req, err := http.NewRequestWithContext(ctx, "DELETE", reqURL, nil)
	if err != nil {
		return err
	}

	s.setHeaders(req)

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete error %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// =============================================
// RPC METHODS
// =============================================

// CallRPC calls a Supabase RPC function
func (s *SupabaseClient) CallRPC(ctx context.Context, functionName string, params map[string]interface{}) (interface{}, error) {
	// Validate function name (alphanumeric and underscores only)
	if !tableNameRegex.MatchString(functionName) {
		return nil, fmt.Errorf("invalid function name")
	}

	reqURL := fmt.Sprintf("%s/rest/v1/rpc/%s", s.baseURL, functionName)

	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	if err := json.NewEncoder(buf).Encode(params); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", reqURL, buf)
	if err != nil {
		return nil, err
	}

	s.setHeaders(req)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// OPTIMIZED: Handle errors without full body read for success case
	if resp.StatusCode >= 400 {
		// Only read body for error message
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024)) // Limit error body
		return nil, fmt.Errorf("RPC error %d: %s", resp.StatusCode, string(body))
	}

	// OPTIMIZED: Stream decode directly from response body
	var result interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result, nil
}

// =============================================
// INTERNAL HELPERS
// =============================================

// setHeaders sets optimized headers
func (s *SupabaseClient) setHeaders(req *http.Request) {
	req.Header.Set("apikey", s.serviceKey)
	req.Header.Set("Authorization", "Bearer "+s.serviceKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// Note: gzip disabled - Go's http.Client handles it automatically via Transport
}

// buildQueryParams builds URL query parameters (optimized)
func (s *SupabaseClient) buildQueryParams(params map[string]interface{}) url.Values {
	// Pre-allocate with estimated capacity
	queryParams := make(url.Values, len(params)+1)

	// Select fields
	if sel, ok := params["select"].(string); ok {
		queryParams.Set("select", sel)
	} else {
		queryParams.Set("select", "*")
	}

	// Where conditions
	if where, ok := params["where"].(map[string]interface{}); ok {
		for key, value := range where {
			if !isValidColumnName(key) {
				continue
			}
			if value == nil {
				queryParams.Set(key, "is.null")
				continue
			}
			switch v := value.(type) {
			case string:
				if isPostgrestOperator(v) {
					queryParams.Set(key, v)
				} else {
					queryParams.Set(key, "eq."+v)
				}
			case bool:
				if v {
					queryParams.Set(key, "eq.true")
				} else {
					queryParams.Set(key, "eq.false")
				}
			case int:
				queryParams.Set(key, "eq."+strconv.Itoa(v))
			case int64:
				queryParams.Set(key, "eq."+strconv.FormatInt(v, 10))
			case float64:
				queryParams.Set(key, "eq."+strconv.FormatFloat(v, 'f', -1, 64))
			default:
				queryParams.Set(key, fmt.Sprintf("eq.%v", value))
			}
		}
	}

	// Order
	if order, ok := params["order"].(string); ok {
		queryParams.Set("order", order)
	}

	// Limit (support int and string)
	switch v := params["limit"].(type) {
	case int:
		queryParams.Set("limit", strconv.Itoa(v))
	case string:
		queryParams.Set("limit", v)
	}

	// Offset (support int and string)
	switch v := params["offset"].(type) {
	case int:
		queryParams.Set("offset", strconv.Itoa(v))
	case string:
		queryParams.Set("offset", v)
	}

	return queryParams
}

// parseResponse parses HTTP response efficiently
func (s *SupabaseClient) parseResponse(resp *http.Response) ([]map[string]interface{}, error) {
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("supabase error %d: %s", resp.StatusCode, string(body))
	}

	var data []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	return data, nil
}

// isValidColumnName validates column names
func isValidColumnName(name string) bool {
	return tableNameRegex.MatchString(name)
}

// postgrestOperators is a precomputed set for O(1) lookup
var postgrestOperators = map[string]struct{}{
	"is.": {}, "in.": {}, "eq.": {}, "neq.": {}, "gt.": {}, "gte.": {},
	"lt.": {}, "lte.": {}, "like.": {}, "ilike.": {}, "cs.": {}, "cd.": {},
	"not.": {}, "or.": {}, "and.": {}, "fts.": {}, "plfts.": {}, "phfts.": {},
	"wfts.": {}, "adj.": {}, "ov.": {}, "sl.": {}, "sr.": {}, "nxl.": {}, "nxr.": {},
}

// isPostgrestOperator checks if a string is a PostgREST operator (O(1) lookup)
func isPostgrestOperator(v string) bool {
	if len(v) < 3 {
		return false
	}
	// Find first dot position (most operators are 2-5 chars)
	dotIdx := strings.IndexByte(v, '.')
	if dotIdx < 2 || dotIdx > 6 {
		return false
	}
	_, ok := postgrestOperators[v[:dotIdx+1]]
	return ok
}

// =============================================
// TYPE-SAFE HELPERS
// =============================================

// GetString safely gets a string from a map
func GetString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// GetInt safely gets an int from a map
func GetInt(m map[string]interface{}, key string) int {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case int64:
			return int(n)
		}
	}
	return 0
}

// GetFloat64 safely gets a float64 from a map
func GetFloat64(m map[string]interface{}, key string) float64 {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case int:
			return float64(n)
		case int64:
			return float64(n)
		}
	}
	return 0
}

// GetBool safely gets a bool from a map
func GetBool(m map[string]interface{}, key string) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// GetTime safely gets a time from a map (RFC3339 format)
func GetTime(m map[string]interface{}, key string) *time.Time {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return &t
			}
		}
	}
	return nil
}

// GetQueryInt gets an int from gin query params with default value
func GetQueryInt(c interface{ Query(string) string }, key string, defaultVal int) int {
	str := c.Query(key)
	if str == "" {
		return defaultVal
	}
	val, err := strconv.Atoi(str)
	if err != nil {
		return defaultVal
	}
	return val
}
