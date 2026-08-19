package usagestats

import (
	"sync"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	modelsDevCatalogURL      = "https://models.dev/api.json"
	modelsDevRequestTimeout  = 15 * time.Second
	modelsDevMaxResponseSize = 16 << 20
	modelsDevMaxModels       = 20_000
)

type modelsDevFetcher struct {
	client *http.Client
	url    string
}

type modelsDevProvider struct {
	ID     string                    `json:"id"`
	Name   string                    `json:"name"`
	Label  string                    `json:"label"`
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	ID           string                 `json:"id"`
	Name         string                 `json:"name"`
	Cost         *modelsDevCost         `json:"cost"`
	Experimental *modelsDevExperimental `json:"experimental"`
}

type modelsDevExperimental struct {
	Modes map[string]modelsDevMode `json:"modes"`
}

type modelsDevMode struct {
	Cost     *modelsDevCost        `json:"cost"`
	Provider modelsDevModeProvider `json:"provider"`
}

type modelsDevModeProvider struct {
	Body modelsDevModeBody `json:"body"`
}

type modelsDevModeBody struct {
	ServiceTier string `json:"service_tier"`
}

type modelsDevCost struct {
	Input      float64             `json:"input"`
	Output     float64             `json:"output"`
	CacheRead  float64             `json:"cache_read"`
	CacheWrite float64             `json:"cache_write"`
	Tiers      []modelsDevCostTier `json:"tiers"`
}

type modelsDevCostTier struct {
	Input      float64           `json:"input"`
	Output     float64           `json:"output"`
	CacheRead  float64           `json:"cache_read"`
	CacheWrite float64           `json:"cache_write"`
	Tier       modelsDevTierKind `json:"tier"`
}

type modelsDevTierKind struct {
	Type string `json:"type"`
	Size uint64 `json:"size"`
}

type modelsDevCandidate struct {
	provider string
	model    string
	price    ModelPrice
	rank     int
}

type modelsDevMatchResult struct {
	Prices    map[string]ModelPrice
	Observed  int
	Matched   int
	Unmatched int
}

// syncModelsDevOnce guards concurrent model price synchronization. Ported
// from the pluginRuntime fields of the original plugin.
var (
	modelsDevSyncMu         sync.Mutex
	modelsDevSyncing        bool
	globalModelsDevFetcher  = newModelsDevFetcher()
)

// SyncModelsDev synchronizes the stored model price book from models.dev.
func SyncModelsDev(store *Store, settings *PriceSyncSettings, sourceModels []string) (ModelPricesResponse, error) {
	modelsDevSyncMu.Lock()
	if modelsDevSyncing {
		modelsDevSyncMu.Unlock()
		return ModelPricesResponse{}, withStatus(http.StatusConflict, "model price synchronization is already running")
	}
	modelsDevSyncing = true
	modelsDevSyncMu.Unlock()
	defer func() {
		modelsDevSyncMu.Lock()
		modelsDevSyncing = false
		modelsDevSyncMu.Unlock()
	}()

	if store == nil {
		return ModelPricesResponse{}, withStatus(http.StatusServiceUnavailable, "plugin storage is not initialized")
	}
	priceBook, err := store.QueryPriceBook()
	if err != nil {
		return ModelPricesResponse{}, err
	}
	activeSettings := priceBook.SyncSettings
	if settings != nil {
		activeSettings, err = normalizePriceSyncSettings(*settings)
		if err != nil {
			return ModelPricesResponse{}, withStatus(http.StatusBadRequest, "%v", err)
		}
	}
	models, err := normalizeSyncModels(sourceModels)
	if err != nil {
		return ModelPricesResponse{}, withStatus(http.StatusBadRequest, "%v", err)
	}
	fetcher := globalModelsDevFetcher
	if fetcher == nil {
		fetcher = newModelsDevFetcher()
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelsDevRequestTimeout)
	defer cancel()
	catalog, err := fetcher.fetch(ctx)
	if err != nil {
		return ModelPricesResponse{}, publicModelsDevError(err)
	}
	now := time.Now().UTC()
	matched, err := matchModelsDevPrices(catalog, models, activeSettings, now)
	if err != nil {
		return ModelPricesResponse{}, withStatus(http.StatusBadGateway, "models.dev returned an invalid price catalog")
	}
	metadata := PriceSyncMetadata{
		Source:      priceSourceModelsDev,
		CompletedAt: now,
		Observed:    matched.Observed,
		Matched:     matched.Matched,
		Unmatched:   matched.Unmatched,
	}
	return store.ApplyModelPriceSync(matched.Prices, activeSettings, metadata, priceBook.Revision)
}

func normalizeSyncModels(input []string) ([]string, error) {
	if len(input) > maxModelPriceEntries {
		return nil, fmt.Errorf("model synchronization must contain at most %d models", maxModelPriceEntries)
	}
	seen := make(map[string]struct{}, len(input))
	models := make([]string, 0, len(input))
	for _, raw := range input {
		model := strings.TrimSpace(raw)
		if model == "" {
			continue
		}
		if !utf8.ValidString(model) || utf8.RuneCountInString(model) > maxDimensionRunes {
			return nil, fmt.Errorf("synchronized model name %q is invalid or too long", model)
		}
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("model synchronization requires at least one CLIProxyAPI model")
	}
	sort.Strings(models)
	return models, nil
}

func publicModelsDevError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return withStatus(http.StatusGatewayTimeout, "models.dev synchronization timed out")
	}
	return withStatus(http.StatusBadGateway, "models.dev synchronization failed")
}

func newModelsDevFetcher() *modelsDevFetcher {
	expected, _ := url.Parse(modelsDevCatalogURL)
	client := &http.Client{
		Timeout: modelsDevRequestTimeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("models.dev redirected too many times")
			}
			if request.URL.Scheme != "https" || !strings.EqualFold(request.URL.Host, expected.Host) {
				return errors.New("models.dev redirect must remain on the HTTPS models.dev host")
			}
			return nil
		},
	}
	return &modelsDevFetcher{client: client, url: modelsDevCatalogURL}
}

func (f *modelsDevFetcher) fetch(ctx context.Context) (map[string]modelsDevProvider, error) {
	if f == nil || f.client == nil {
		return nil, errors.New("models.dev HTTP client is unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
	if err != nil {
		return nil, fmt.Errorf("create models.dev request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "cap-token-usage-tracker/"+version)
	response, err := f.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch models.dev catalog: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch models.dev catalog: HTTP %d", response.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return nil, fmt.Errorf("fetch models.dev catalog: expected application/json response")
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, modelsDevMaxResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("read models.dev catalog: %w", err)
	}
	if len(body) > modelsDevMaxResponseSize {
		return nil, fmt.Errorf("read models.dev catalog: response exceeds %d bytes", modelsDevMaxResponseSize)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	var catalog map[string]modelsDevProvider
	if err := decoder.Decode(&catalog); err != nil {
		return nil, fmt.Errorf("decode models.dev catalog: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("decode models.dev catalog: trailing JSON value")
		}
		return nil, fmt.Errorf("decode models.dev catalog: %w", err)
	}
	if len(catalog) == 0 {
		return nil, errors.New("decode models.dev catalog: no providers found")
	}
	count := 0
	for _, provider := range catalog {
		count += len(provider.Models)
		if count > modelsDevMaxModels {
			return nil, fmt.Errorf("decode models.dev catalog: more than %d models", modelsDevMaxModels)
		}
	}
	return catalog, nil
}

func matchModelsDevPrices(catalog map[string]modelsDevProvider, observed []string, settings PriceSyncSettings, now time.Time) (modelsDevMatchResult, error) {
	normalizedSettings, err := normalizePriceSyncSettings(settings)
	if err != nil {
		return modelsDevMatchResult{}, err
	}
	priority := make(map[string]int, len(normalizedSettings.ProviderPriority))
	for rank, provider := range normalizedSettings.ProviderPriority {
		priority[provider] = rank
	}

	candidates := make(map[string]modelsDevCandidate)
	for providerKey, provider := range catalog {
		providerName := firstNonEmpty(provider.ID, provider.Name, provider.Label, providerKey)
		normalizedProvider := normalizeCatalogName(providerName)
		rank, prioritized := priority[normalizedProvider]
		if !prioritized {
			rank = len(priority)
		}
		for modelKey, model := range provider.Models {
			if model.Cost == nil {
				continue
			}
			catalogModel := firstNonEmpty(model.ID, modelKey, model.Name)
			comparison := comparisonModelName(catalogModel, normalizedSettings)
			if comparison == "" {
				continue
			}
			price := modelPriceFromModelsDev(model, normalizedProvider, catalogModel, now)
			candidate := modelsDevCandidate{provider: normalizedProvider, model: catalogModel, price: price, rank: rank}
			current, exists := candidates[comparison]
			if !exists || candidateLess(candidate, current) {
				candidates[comparison] = candidate
			}
		}
	}

	uniqueObserved := make(map[string]struct{}, len(observed))
	for _, raw := range observed {
		model := strings.TrimSpace(raw)
		if model != "" {
			uniqueObserved[model] = struct{}{}
		}
	}
	models := make([]string, 0, len(uniqueObserved))
	for model := range uniqueObserved {
		models = append(models, model)
	}
	sort.Strings(models)

	result := modelsDevMatchResult{Prices: make(map[string]ModelPrice), Observed: len(models)}
	for _, model := range models {
		candidate, ok := candidates[comparisonModelName(model, normalizedSettings)]
		if !ok {
			result.Unmatched++
			continue
		}
		result.Prices[model] = candidate.price
		result.Matched++
	}
	normalized, err := normalizeModelPrices(result.Prices)
	if err != nil {
		return modelsDevMatchResult{}, fmt.Errorf("validate synchronized model prices: %w", err)
	}
	result.Prices = normalized
	return result, nil
}

func modelPriceFromModelsDev(catalogModel modelsDevModel, provider, model string, now time.Time) ModelPrice {
	cost := *catalogModel.Cost
	price := ModelPrice{
		Input:           cost.Input,
		Output:          cost.Output,
		CacheRead:       cost.CacheRead,
		CacheCreation:   cost.CacheWrite,
		Source:          priceSourceModelsDev,
		CatalogProvider: provider,
		CatalogModel:    model,
		UpdatedAt:       now.UTC(),
	}
	for _, tier := range cost.Tiers {
		if tier.Tier.Type != "context" || tier.Tier.Size == 0 {
			continue
		}
		price.ContextTiers = append(price.ContextTiers, ContextPriceTier{
			Threshold:     tier.Tier.Size,
			Input:         tier.Input,
			Output:        tier.Output,
			CacheRead:     tier.CacheRead,
			CacheCreation: tier.CacheWrite,
		})
	}
	if catalogModel.Experimental != nil {
		modeNames := make([]string, 0, len(catalogModel.Experimental.Modes))
		for name := range catalogModel.Experimental.Modes {
			modeNames = append(modeNames, name)
		}
		sort.Strings(modeNames)
		for _, name := range modeNames {
			mode := catalogModel.Experimental.Modes[name]
			serviceTier := strings.ToLower(strings.TrimSpace(mode.Provider.Body.ServiceTier))
			if mode.Cost == nil || serviceTier == "" {
				continue
			}
			if price.ServiceTiers == nil {
				price.ServiceTiers = make(map[string]ServiceTierPrice)
			}
			if _, exists := price.ServiceTiers[serviceTier]; exists {
				continue
			}
			price.ServiceTiers[serviceTier] = serviceTierPriceFromModelsDev(*mode.Cost)
		}
	}
	return price
}

func serviceTierPriceFromModelsDev(cost modelsDevCost) ServiceTierPrice {
	price := ServiceTierPrice{
		Input: cost.Input, Output: cost.Output, CacheRead: cost.CacheRead, CacheCreation: cost.CacheWrite,
	}
	for _, tier := range cost.Tiers {
		if tier.Tier.Type != "context" || tier.Tier.Size == 0 {
			continue
		}
		price.ContextTiers = append(price.ContextTiers, ContextPriceTier{
			Threshold: tier.Tier.Size, Input: tier.Input, Output: tier.Output,
			CacheRead: tier.CacheRead, CacheCreation: tier.CacheWrite,
		})
	}
	return price
}

func comparisonModelName(value string, settings PriceSyncSettings) string {
	value = normalizeCatalogName(value)
	for _, mapping := range settings.Mappings {
		if value == mapping.Source {
			value = mapping.Target
			break
		}
	}
	for {
		previous := value
		for _, suffix := range settings.IgnoredSuffixes {
			if strings.HasSuffix(value, suffix) {
				value = strings.TrimSpace(strings.TrimSuffix(value, suffix))
				break
			}
		}
		if value == previous {
			return value
		}
	}
}

func normalizeCatalogName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	parts := strings.Split(value, "/")
	return strings.TrimSpace(parts[len(parts)-1])
}

func candidateLess(left, right modelsDevCandidate) bool {
	if left.rank != right.rank {
		return left.rank < right.rank
	}
	if left.provider != right.provider {
		return left.provider < right.provider
	}
	return left.model < right.model
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
