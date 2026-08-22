package usagestats

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// -----------------------------------------------------------------------------
// Public API for the workbuddy main plugin
// -----------------------------------------------------------------------------

// Open opens (or re-opens) the local token statistics database. apiKeySecret
// is intentionally ignored: the workbuddy plugin authenticates with OAuth
// accounts (auth_index / UID), never API keys, so key encryption is disabled.
func Open(config Config) (*Store, error) {
	config.APIKeySecret = ""
	return openStore(config)
}

// RecordUsageRecord decodes one host-delivered UsageRecord (the CPA
// UsagePlugin broadcast that captures api-provider / third-party requests
// outside the workbuddy feed path) and records it into the store. The record
// shape is pluginapi.UsageRecord JSON; the decode reuses the same canonical
// normalization as the original cap-token-usage-tracker plugin so api-provider
// requests show up in the dashboard alongside workbuddy feed records.
func (s *Store) RecordUsageRecord(raw []byte) error {
	if s == nil {
		return fmt.Errorf("store is not initialized")
	}
	usage, err := decodeUsage(raw, time.Now().UTC())
	if err != nil {
		return err
	}
	// The host UsagePlugin broadcast carries the raw upstream API key in
	// Dimensions.APIKey, but never the matching APIKeyHash / APIKeyGeneration
	// (those are produced by the tracker's own crypto layer, which is disabled
	// here — see Open). The store enforces a ciphertext-consistency guard that
	// requires all three to be recorded together, so zero the whole envelope
	// before persisting. This mirrors the original cap-token-usage-tracker's
	// "source missing" branch: no API-key identity dimension is retained, but
	// provider / model / alias accounting still lands.
	usage.Dimensions.APIKey = ""
	usage.Dimensions.APIKeyHash = ""
	usage.Dimensions.APIKeyGeneration = 0
	usage.Dimensions.APIKeyStatus = apiKeyStatusSourceMissing
	return s.Record(usage)
}

// DashboardHTML returns the standalone token usage dashboard page (HTML).
func DashboardHTML() []byte {
	return []byte(dashboardHTML)
}

// DefaultDataPath resolves the default statistics database location: the
// "data" directory next to the discovered CLIProxyAPI root, falling back to
// the working directory. fileName is the database file name.
func DefaultDataPath(fileName string) string {
	if strings.TrimSpace(fileName) == "" {
		fileName = "usage-stats.db"
	}
	modulePath, _ := loadedPluginPath()
	executablePath, _ := os.Executable()
	workingDir, _ := os.Getwd()
	if root, ok := cliProxyRootFromPluginPath(modulePath, workingDir); ok {
		return filepath.Join(root, "data", fileName)
	}
	if root, ok := cliProxyRootBesideExecutable(executablePath); ok {
		return filepath.Join(root, "data", fileName)
	}
	if root, ok := cliProxyRootAtWorkingDir(workingDir); ok {
		return filepath.Join(root, "data", fileName)
	}
	return filepath.Join("data", fileName)
}

// NewCounters builds a Counters record for one request. total=0 falls back to
// input+output+reasoning (matching the decode path).
func NewCounters(input, output, reasoning, cached, cacheRead, cacheCreation, total int64) Counters {
	if total <= 0 {
		total = saturatingInt64Sum(saturatingInt64Sum(input, output), reasoning)
	}
	return Counters{
		Requests:            1,
		InputTokens:         positiveUint(input),
		OutputTokens:        positiveUint(output),
		ReasoningTokens:     positiveUint(reasoning),
		CachedTokens:        positiveUint(cached),
		CacheReadTokens:     positiveUint(cacheRead),
		CacheCreationTokens: positiveUint(cacheCreation),
		TotalTokens:         positiveUint(total),
	}
}

// NewUsage builds a normalizedUsage record for the local statistics store.
// source should be a URL or stable provider identifier (never a credential).
func NewUsage(provider, executorType, model, alias, source, authType, authIndex string, requestedAt time.Time, latencyNS uint64, failed bool, statusCode int, counters Counters) normalizedUsage {
	counters.FailedRequests = 0
	if failed {
		counters.FailedRequests = 1
	}
	return normalizedUsage{
		Dimensions: Dimensions{
			Provider:      normalizeDimension(provider),
			ExecutorType:  normalizeDimension(executorType),
			Model:         normalizeDimension(model),
			Alias:         normalizeDimension(alias),
			Source:        normalizeDimension(source),
			AuthType:      normalizeDimension(authType),
			Failed:        failed,
			FailureStatus: clampStatus(int64(statusCode)),
		},
		RequestedAt: requestedAt.UTC(),
		LatencyNS:   latencyNS,
		Counters:    counters,
		authIndex:   strings.TrimSpace(authIndex),
	}
}

// QueryResult is the result of a statistics query, ready to be wrapped into a
// ManagementResponse by the caller.
type QueryResult struct {
	Status  int
	Headers http.Header
	Body    []byte
}

func (q QueryResult) isJSON() bool {
	return q.Headers.Get("Content-Type") == "application/json; charset=utf-8"
}

// HandleQuery dispatches a statistics query by relative path. body carries the
// request payload for write endpoints (PUT /prices, POST /prices/sync,
// POST /reset, POST /restore); headers carries the original request headers.
// Supported paths:
//
//	GET  /stats /stats/initial /stats/trends /stats/groups
//	GET  /requests /costs /exchange-rate /prices /preferences
//	PUT  /prices              (save price book)
//	POST /prices/sync         (models.dev sync)
//	POST /reset
//	GET  /backup   POST /restore
func (s *Store) HandleQuery(method, path string, query url.Values, body []byte, headers http.Header) QueryResult {
	if s == nil {
		return jsonQueryResult(http.StatusServiceUnavailable, map[string]any{"error": "storage is not initialized"})
	}
	switch path {
	case "/stats":
		if method != http.MethodGet {
			return methodNotAllowedResult(http.MethodGet)
		}
		return s.statsResult(query)
	case "/stats/initial":
		if method != http.MethodGet {
			return methodNotAllowedResult(http.MethodGet)
		}
		return s.initialStatsResult(query)
	case "/stats/trends":
		if method != http.MethodGet {
			return methodNotAllowedResult(http.MethodGet)
		}
		return s.statsTrendResult(query)
	case "/stats/groups":
		if method != http.MethodGet {
			return methodNotAllowedResult(http.MethodGet)
		}
		return s.groupsStatsResult(query)
	case "/requests":
		if method != http.MethodGet {
			return methodNotAllowedResult(http.MethodGet)
		}
		return s.requestsResult(query)
	case "/costs":
		if method != http.MethodGet {
			return methodNotAllowedResult(http.MethodGet)
		}
		return s.costsResult(query)
	case "/exchange-rate":
		if method != http.MethodGet {
			return methodNotAllowedResult(http.MethodGet)
		}
		return exchangeRateResult()
	case "/prices":
		if method == http.MethodGet {
			return s.pricesResult()
		}
		if method == http.MethodPut {
			return s.savePricesResult(body, headers)
		}
		return methodNotAllowedResult("GET, PUT")
	case "/prices/sync":
		if method != http.MethodPost {
			return methodNotAllowedResult(http.MethodPost)
		}
		return s.syncPricesResult(body, headers)
	case "/preferences":
		if method != http.MethodGet {
			return methodNotAllowedResult(http.MethodGet)
		}
		return s.preferencesResult(query)
	case "/reset":
		if method != http.MethodPost {
			return methodNotAllowedResult(http.MethodPost)
		}
		return s.resetResult(body)
	case "/backup":
		if method != http.MethodGet {
			return methodNotAllowedResult(http.MethodGet)
		}
		return s.backupResult()
	case "/restore":
		if method != http.MethodPost {
			return methodNotAllowedResult(http.MethodPost)
		}
		return s.restoreResult(body, headers)
	default:
		return jsonQueryResult(http.StatusNotFound, map[string]any{"error": "route not found"})
	}
}

func (s *Store) statsResult(query url.Values) QueryResult {
	queryRange, err := usageRangeFromQuery(query.Get("range"), query.Get("start"), query.Get("end"), time.Now().UTC())
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	stats, err := s.queryStatsByFilter(queryRange, newUsageFilterFromIdentities(query.Get("source"), nil))
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	stats.Redact()
	return jsonQueryResult(http.StatusOK, &stats)
}

func (s *Store) initialStatsResult(query url.Values) QueryResult {
	queryRange, err := usageRangeFromQuery(query.Get("range"), query.Get("start"), query.Get("end"), time.Now().UTC())
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	stats, err := s.queryInitialStatsByFilter(queryRange, newUsageFilterFromIdentities(query.Get("source"), nil))
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	return jsonQueryResult(http.StatusOK, &stats)
}

func (s *Store) statsTrendResult(query url.Values) QueryResult {
	queryRange, err := usageRangeFromQuery(query.Get("range"), query.Get("start"), query.Get("end"), time.Now().UTC())
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	stats, err := s.queryStatsTrendByFilter(queryRange, newUsageFilterFromIdentities(query.Get("source"), nil))
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	return jsonQueryResult(http.StatusOK, stats)
}

func (s *Store) groupsStatsResult(query url.Values) QueryResult {
	queryRange, err := usageRangeFromQuery(query.Get("range"), query.Get("start"), query.Get("end"), time.Now().UTC())
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	filter := newUsageFilterFromIdentities(query.Get("source"), nil)
	filter.Model = normalizeDimension(query.Get("model"))
	excludedModels := make(map[string]struct{}, len(query["exclude_model"]))
	for _, model := range query["exclude_model"] {
		if model = normalizeDimension(model); model != "" {
			excludedModels[model] = struct{}{}
		}
	}
	stats, err := s.queryGroupsByFilter(queryRange, filter)
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	if len(excludedModels) > 0 {
		items := stats.Items[:0]
		for _, item := range stats.Items {
			if _, excluded := excludedModels[compactModelName(item.Model)]; !excluded {
				items = append(items, item)
			}
		}
		stats.Items = items
		stats.Total = len(items)
	}
	if err := sortGroupStats(stats.Items, query.Get("sort"), query.Get("direction")); err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	offset, err := parseNonNegativeQueryInt(query.Get("offset"), 0, "offset")
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	limit, err := parseNonNegativeQueryInt(query.Get("limit"), defaultRequestPageSize, "limit")
	if err != nil || limit < 1 || limit > maxDashboardPageSize {
		return jsonQueryResult(http.StatusBadRequest, map[string]any{"error": "limit must be an integer between 1 and 500"})
	}
	if offset > stats.Total {
		offset = stats.Total
	}
	end := offset + limit
	if end > stats.Total {
		end = stats.Total
	}
	stats.Items = stats.Items[offset:end]
	stats.Redact()
	return jsonQueryResult(http.StatusOK, &stats)
}

func (s *Store) requestsResult(query url.Values) QueryResult {
	queryRange, err := usageRangeFromQuery(query.Get("range"), query.Get("start"), query.Get("end"), time.Now().UTC())
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	offset, err := parseNonNegativeQueryInt(query.Get("offset"), 0, "offset")
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	limit, err := parseNonNegativeQueryInt(query.Get("limit"), defaultRequestPageSize, "limit")
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	page, err := s.queryRequestPageByFilter(queryRange, offset, limit, query.Get("model"), newUsageFilterFromIdentities(query.Get("source"), nil), query.Get("result"))
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	page.Redact()
	return jsonQueryResult(http.StatusOK, &page)
}

func (s *Store) costsResult(query url.Values) QueryResult {
	queryRange, err := usageRangeFromQuery(query.Get("range"), query.Get("start"), query.Get("end"), time.Now().UTC())
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	costs, err := s.queryCostsByFilter(queryRange, newUsageFilterFromIdentities(query.Get("source"), nil))
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	costs.Redact()
	return jsonQueryResult(http.StatusOK, &costs)
}

func exchangeRateResult() QueryResult {
	rate, err := newExchangeRateService().latest()
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	return jsonQueryResult(http.StatusOK, rate)
}

func (s *Store) pricesResult() QueryResult {
	priceBook, err := s.QueryPriceBook()
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	return jsonQueryResult(http.StatusOK, priceBook)
}

func (s *Store) savePricesResult(body []byte, headers http.Header) QueryResult {
	contentType, _, err := mime.ParseMediaType(headers.Get("Content-Type"))
	if err != nil || !strings.EqualFold(contentType, "application/json") {
		return jsonQueryResult(http.StatusUnsupportedMediaType, map[string]any{"error": "Content-Type must be application/json"})
	}
	if len(body) > 2<<20 {
		return jsonQueryResult(http.StatusRequestEntityTooLarge, map[string]any{"error": "model prices JSON is too large"})
	}
	var input struct {
		Prices       map[string]ModelPrice `json:"prices"`
		SyncSettings *PriceSyncSettings    `json:"sync_settings,omitempty"`
	}
	if err := decodeStrictJSON(body, &input); err != nil || input.Prices == nil {
		return jsonQueryResult(http.StatusBadRequest, map[string]any{"error": "invalid model prices JSON"})
	}
	priceBook, err := s.SavePriceBook(input.Prices, input.SyncSettings)
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	return jsonQueryResult(http.StatusOK, priceBook)
}

func (s *Store) syncPricesResult(body []byte, headers http.Header) QueryResult {
	contentType, _, err := mime.ParseMediaType(headers.Get("Content-Type"))
	if err != nil || !strings.EqualFold(contentType, "application/json") {
		return jsonQueryResult(http.StatusUnsupportedMediaType, map[string]any{"error": "Content-Type must be application/json"})
	}
	if len(body) > 2<<20 {
		return jsonQueryResult(http.StatusRequestEntityTooLarge, map[string]any{"error": "model price synchronization JSON is too large"})
	}
	var input struct {
		Source       string             `json:"source"`
		Models       []string           `json:"models"`
		SyncSettings *PriceSyncSettings `json:"sync_settings,omitempty"`
	}
	if err := decodeStrictJSON(body, &input); err != nil {
		return jsonQueryResult(http.StatusBadRequest, map[string]any{"error": "invalid model price synchronization JSON"})
	}
	if input.Source != "" && input.Source != priceSourceModelsDev {
		return jsonQueryResult(http.StatusBadRequest, map[string]any{"error": `source must be "models.dev"`})
	}
	priceBook, err := SyncModelsDev(s, input.SyncSettings, input.Models)
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	return jsonQueryResult(http.StatusOK, priceBook)
}

func (s *Store) preferencesResult(query url.Values) QueryResult {
	if query.Get("save") == "" {
		if len(query) != 0 {
			return jsonQueryResult(http.StatusBadRequest, map[string]any{"error": "save must be 1 when preference values are supplied"})
		}
		preferences, err := s.QueryDashboardPreferences()
		if err != nil {
			return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
		}
		return jsonQueryResult(http.StatusOK, preferences)
	}
	preferences, err := dashboardPreferencesFromQuery(query)
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	preferences, err = s.SaveDashboardPreferences(preferences)
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	return jsonQueryResult(http.StatusOK, preferences)
}

func (s *Store) resetResult(body []byte) QueryResult {
	var confirmation struct {
		Confirm string `json:"confirm"`
	}
	if err := json.Unmarshal(body, &confirmation); err != nil || confirmation.Confirm != "reset" {
		return jsonQueryResult(http.StatusBadRequest, map[string]any{"error": `body must be {"confirm":"reset"}`})
	}
	if err := s.Reset(); err != nil {
		return jsonQueryResult(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
	return jsonQueryResult(http.StatusOK, map[string]any{"reset": true, "reset_at": nowUTC()})
}

func (s *Store) backupResult() QueryResult {
	data, err := s.Backup()
	if err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	filename := "workbuddy-usage-" + nowUTC().UTC().Format("20060102-150405") + ".db"
	headers := http.Header{}
	headers.Set("Content-Type", "application/octet-stream")
	headers.Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	headers.Set("Cache-Control", "no-store")
	headers.Set("X-Content-Type-Options", "nosniff")
	return QueryResult{Status: http.StatusOK, Headers: headers, Body: data}
}

func (s *Store) restoreResult(body []byte, headers http.Header) QueryResult {
	contentType := strings.TrimSpace(headers.Get("Content-Type"))
	if contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || !strings.EqualFold(mediaType, "application/octet-stream") {
			return jsonQueryResult(http.StatusUnsupportedMediaType, map[string]any{"error": "Content-Type must be application/octet-stream"})
		}
	}
	if headers.Get("X-Confirm-Restore") != "replace" {
		return jsonQueryResult(http.StatusBadRequest, map[string]string{"error": "missing X-Confirm-Restore: replace header"})
	}
	if len(body) == 0 {
		return jsonQueryResult(http.StatusBadRequest, map[string]any{"error": "backup body must not be empty"})
	}
	if len(body) > maxDatabaseBackupBytes {
		return jsonQueryResult(http.StatusRequestEntityTooLarge, map[string]any{"error": "backup body is too large"})
	}
	if err := s.RestoreBackup(body); err != nil {
		return jsonQueryResult(errorHTTPStatus(err), map[string]any{"error": err.Error()})
	}
	return jsonQueryResult(http.StatusOK, map[string]any{"restored": true, "restored_at": nowUTC()})
}

// -----------------------------------------------------------------------------
// Ported helpers (originally in management.go of the tracker plugin)
// -----------------------------------------------------------------------------

func jsonQueryResult(status int, value any) QueryResult {
	body, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
	}
	headers := http.Header{}
	headers.Set("Content-Type", "application/json; charset=utf-8")
	headers.Set("Cache-Control", "no-store")
	headers.Set("X-Content-Type-Options", "nosniff")
	return QueryResult{Status: status, Headers: headers, Body: body}
}

func methodNotAllowedResult(allowed string) QueryResult {
	result := jsonQueryResult(http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	result.Headers.Set("Allow", allowed)
	return result
}

func sortGroupStats(items []GroupStats, sortKey, direction string) error {
	if sortKey == "" {
		sortKey = "total_tokens"
	}
	if direction == "" {
		direction = "desc"
	}
	if direction != "asc" && direction != "desc" {
		return withStatus(http.StatusBadRequest, "direction must be asc or desc")
	}
	numeric := map[string]bool{"requests": true, "failed_requests": true, "input_tokens": true, "output_tokens": true, "reasoning_tokens": true, "cache_read_tokens": true, "cache_creation_tokens": true, "total_tokens": true, "average_latency_ns": true, "average_ttft_ns": true}
	text := map[string]bool{"model": true, "provider": true, "api_key": true, "alias": true, "source": true, "executor_type": true, "auth_type": true, "session_key": true, "reasoning_effort": true}
	if !numeric[sortKey] && !text[sortKey] {
		return withStatus(http.StatusBadRequest, "unsupported group sort %q", sortKey)
	}
	value := func(item GroupStats) string {
		switch sortKey {
		case "model":
			return compactModelName(item.Model)
		case "provider":
			return item.Provider
		case "api_key":
			return item.APIKeyHash
		case "alias":
			return item.Alias
		case "source":
			return item.Source
		case "executor_type":
			return item.ExecutorType
		case "auth_type":
			return item.AuthType
		case "session_key":
			return item.SessionKey
		case "reasoning_effort":
			return item.ReasoningEffort
		default:
			return ""
		}
	}
	number := func(item GroupStats) uint64 {
		switch sortKey {
		case "requests":
			return item.Requests
		case "failed_requests":
			return item.FailedRequests
		case "input_tokens":
			return item.InputTokens
		case "output_tokens":
			return item.OutputTokens
		case "reasoning_tokens":
			return item.ReasoningTokens
		case "cache_read_tokens":
			return item.CacheReadTokens
		case "cache_creation_tokens":
			return item.CacheCreationTokens
		case "total_tokens":
			return item.TotalTokens
		case "average_latency_ns":
			return item.AverageLatencyNS
		case "average_ttft_ns":
			return item.AverageTTFTNS
		default:
			return 0
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		var less bool
		if numeric[sortKey] {
			less = number(items[i]) < number(items[j])
		} else {
			less = value(items[i]) < value(items[j])
		}
		if numeric[sortKey] && number(items[i]) == number(items[j]) || !numeric[sortKey] && value(items[i]) == value(items[j]) {
			return compareDimensions(items[i].Dimensions, items[j].Dimensions) < 0
		}
		if direction == "desc" {
			return !less
		}
		return less
	})
	return nil
}

func parseNonNegativeQueryInt(raw string, fallback int, name string) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, withStatus(http.StatusBadRequest, "%s must be a non-negative integer", name)
	}
	return value, nil
}

func dashboardPreferencesFromQuery(query map[string][]string) (DashboardPreferences, error) {
	allowed := map[string]struct{}{
		"save": {}, "request_page_size": {}, "dimension_page_size": {},
		"hidden_request_column": {}, "hidden_dimension_column": {}, "time_range_mode": {},
		"time_range_start": {}, "time_range_end": {},
	}
	for key := range query {
		if _, ok := allowed[key]; !ok {
			return DashboardPreferences{}, withStatus(http.StatusBadRequest, "unsupported dashboard preference query parameter %q", key)
		}
	}
	if values := query["save"]; len(values) != 1 || values[0] != "1" {
		return DashboardPreferences{}, withStatus(http.StatusBadRequest, "save must be 1")
	}
	requestPageSize, err := parseDashboardPageSize(query, "request_page_size")
	if err != nil {
		return DashboardPreferences{}, err
	}
	dimensionPageSize, err := parseDashboardPageSize(query, "dimension_page_size")
	if err != nil {
		return DashboardPreferences{}, err
	}
	timeRangeMode, err := optionalDashboardPreference(query, "time_range_mode")
	if err != nil {
		return DashboardPreferences{}, err
	}
	timeRangeStart, err := optionalDashboardPreference(query, "time_range_start")
	if err != nil {
		return DashboardPreferences{}, err
	}
	timeRangeEnd, err := optionalDashboardPreference(query, "time_range_end")
	if err != nil {
		return DashboardPreferences{}, err
	}
	return DashboardPreferences{
		RequestPageSize:        requestPageSize,
		DimensionPageSize:      dimensionPageSize,
		HiddenRequestColumns:   append([]string{}, query["hidden_request_column"]...),
		HiddenDimensionColumns: append([]string{}, query["hidden_dimension_column"]...),
		TimeRangeMode:          timeRangeMode,
		TimeRangeStart:         timeRangeStart,
		TimeRangeEnd:           timeRangeEnd,
	}, nil
}

func optionalDashboardPreference(query map[string][]string, name string) (string, error) {
	values := query[name]
	if len(values) > 1 {
		return "", withStatus(http.StatusBadRequest, "%s must be supplied at most once", name)
	}
	if len(values) == 0 {
		return "", nil
	}
	return values[0], nil
}

func parseDashboardPageSize(query map[string][]string, name string) (int, error) {
	values := query[name]
	if len(values) != 1 {
		return 0, withStatus(http.StatusBadRequest, "%s must be supplied exactly once", name)
	}
	value, err := strconv.Atoi(values[0])
	if err != nil || value < 1 || value > maxDashboardPageSize {
		return 0, withStatus(http.StatusBadRequest, "%s must be an integer between 1 and %d", name, maxDashboardPageSize)
	}
	return value, nil
}

func decodeStrictJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != nil {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}
