package usagestats

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxModelPriceEntries = 10_000
	maxContextPriceTiers = 16
	maxServiceTierPrices = 16
	maxTokenPricePerM    = 1_000_000.0

	accountingModeInputExcludesCache = "input_excludes_cache"
	accountingModeInputIncludesCache = "input_includes_cache"

	priceSourceManual    = "manual"
	priceSourceModelsDev = "models.dev"
)

// TokenRates stores USD prices per one million tokens for each billable counter.
type TokenRates struct {
	Input         float64 `json:"input"`
	Output        float64 `json:"output"`
	CacheRead     float64 `json:"cache_read"`
	CacheCreation float64 `json:"cache_creation"`
}

// ContextPriceTier replaces the base rates when one request exceeds Threshold context tokens.
type ContextPriceTier struct {
	Threshold     uint64  `json:"threshold"`
	Input         float64 `json:"input"`
	Output        float64 `json:"output"`
	CacheRead     float64 `json:"cache_read"`
	CacheCreation float64 `json:"cache_creation"`
}

// ServiceTierPrice replaces the base price schedule for one request service tier.
// It may define its own context tiers because catalog mode pricing can vary by
// both service tier and context size.
type ServiceTierPrice struct {
	Input         float64            `json:"input"`
	Output        float64            `json:"output"`
	CacheRead     float64            `json:"cache_read"`
	CacheCreation float64            `json:"cache_creation"`
	ContextTiers  []ContextPriceTier `json:"context_tiers,omitempty"`
}

// ModelPrice stores the active rates and optional synchronization provenance for one model.
type ModelPrice struct {
	Input           float64                     `json:"input"`
	Output          float64                     `json:"output"`
	CacheRead       float64                     `json:"cache_read"`
	CacheCreation   float64                     `json:"cache_creation"`
	ContextTiers    []ContextPriceTier          `json:"context_tiers,omitempty"`
	ServiceTiers    map[string]ServiceTierPrice `json:"service_tiers,omitempty"`
	AccountingMode  string                      `json:"accounting_mode,omitempty"`
	Source          string                      `json:"source,omitempty"`
	CatalogProvider string                      `json:"catalog_provider,omitempty"`
	CatalogModel    string                      `json:"catalog_model,omitempty"`
	UpdatedAt       time.Time                   `json:"updated_at,omitempty"`
}

type PriceSyncMapping struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

type PriceSyncSettings struct {
	ProviderPriority []string           `json:"provider_priority"`
	IgnoredSuffixes  []string           `json:"ignored_suffixes"`
	Mappings         []PriceSyncMapping `json:"mappings"`
}

type PriceSyncMetadata struct {
	Source        string    `json:"source"`
	CompletedAt   time.Time `json:"completed_at"`
	Observed      int       `json:"observed"`
	Matched       int       `json:"matched"`
	Created       int       `json:"created"`
	Updated       int       `json:"updated"`
	SkippedManual int       `json:"skipped_manual"`
	Unmatched     int       `json:"unmatched"`
}

type ModelPricesResponse struct {
	SchemaVersion uint32                `json:"schema_version"`
	Revision      uint64                `json:"revision"`
	Prices        map[string]ModelPrice `json:"prices"`
	SyncSettings  PriceSyncSettings     `json:"sync_settings"`
	LastSync      *PriceSyncMetadata    `json:"last_sync,omitempty"`
}

func defaultPriceSyncSettings() PriceSyncSettings {
	return PriceSyncSettings{
		ProviderPriority: []string{"openai", "google", "anthropic"},
		IgnoredSuffixes: []string{
			"-thinking", "-preview", "-high", "-low", "(thinking)", "(xhigh)", "(high)", "(low)",
		},
	}
}

func normalizePriceSyncSettings(input PriceSyncSettings) (PriceSyncSettings, error) {
	defaults := defaultPriceSyncSettings()
	result := PriceSyncSettings{}
	if len(input.ProviderPriority) == 0 {
		result.ProviderPriority = append([]string(nil), defaults.ProviderPriority...)
	} else {
		seen := make(map[string]struct{}, len(input.ProviderPriority))
		for _, raw := range input.ProviderPriority {
			value := normalizeCatalogName(raw)
			if value == "" {
				continue
			}
			if _, exists := seen[value]; exists {
				continue
			}
			seen[value] = struct{}{}
			result.ProviderPriority = append(result.ProviderPriority, value)
		}
		if len(result.ProviderPriority) == 0 {
			result.ProviderPriority = append([]string(nil), defaults.ProviderPriority...)
		}
	}
	if len(input.IgnoredSuffixes) == 0 {
		result.IgnoredSuffixes = append([]string(nil), defaults.IgnoredSuffixes...)
	} else {
		seen := make(map[string]struct{}, len(input.IgnoredSuffixes))
		for _, raw := range input.IgnoredSuffixes {
			value := strings.ToLower(strings.TrimSpace(raw))
			if value == "" {
				continue
			}
			if !validOptionalDimension(value) {
				return PriceSyncSettings{}, fmt.Errorf("ignored model suffix %q is invalid or too long", raw)
			}
			if _, exists := seen[value]; exists {
				continue
			}
			seen[value] = struct{}{}
			result.IgnoredSuffixes = append(result.IgnoredSuffixes, value)
		}
		if len(result.IgnoredSuffixes) == 0 {
			result.IgnoredSuffixes = append([]string(nil), defaults.IgnoredSuffixes...)
		}
	}
	if len(input.Mappings) > maxModelPriceEntries {
		return PriceSyncSettings{}, fmt.Errorf("model price mappings must contain at most %d entries", maxModelPriceEntries)
	}
	seenMappings := make(map[string]struct{}, len(input.Mappings))
	for _, mapping := range input.Mappings {
		source := normalizeCatalogName(mapping.Source)
		target := normalizeCatalogName(mapping.Target)
		if source == "" || target == "" {
			return PriceSyncSettings{}, fmt.Errorf("model price mappings require non-empty source and target")
		}
		key := source + "\x00" + target
		if _, exists := seenMappings[key]; exists {
			continue
		}
		seenMappings[key] = struct{}{}
		result.Mappings = append(result.Mappings, PriceSyncMapping{Source: source, Target: target})
	}
	return result, nil
}

func (p ModelPrice) tokenRates() TokenRates {
	return TokenRates{Input: p.Input, Output: p.Output, CacheRead: p.CacheRead, CacheCreation: p.CacheCreation}
}

func (t ContextPriceTier) tokenRates() TokenRates {
	return TokenRates{Input: t.Input, Output: t.Output, CacheRead: t.CacheRead, CacheCreation: t.CacheCreation}
}

func (p ServiceTierPrice) tokenRates() TokenRates {
	return TokenRates{Input: p.Input, Output: p.Output, CacheRead: p.CacheRead, CacheCreation: p.CacheCreation}
}

func normalizeModelPrices(input map[string]ModelPrice) (map[string]ModelPrice, error) {
	if len(input) > maxModelPriceEntries {
		return nil, fmt.Errorf("model prices must contain at most %d entries", maxModelPriceEntries)
	}
	result := make(map[string]ModelPrice, len(input))
	seen := make(map[string]string, len(input))
	for rawModel, rawPrice := range input {
		model := strings.TrimSpace(rawModel)
		if model == "" {
			return nil, fmt.Errorf("model price name must not be empty")
		}
		if !utf8.ValidString(model) || utf8.RuneCountInString(model) > maxDimensionRunes {
			return nil, fmt.Errorf("model price name %q is invalid or too long", model)
		}
		if previous, exists := seen[model]; exists {
			return nil, fmt.Errorf("model price names %q and %q normalize to the same model", previous, rawModel)
		}
		seen[model] = rawModel

		price, err := normalizeModelPrice(model, rawPrice)
		if err != nil {
			return nil, err
		}
		if price.Source == "" {
			price.Source = priceSourceManual
		}
		result[model] = price
	}
	return result, nil
}

func normalizeModelPrice(model string, price ModelPrice) (ModelPrice, error) {
	if err := validateTokenRates(price.tokenRates(), model, "base"); err != nil {
		return ModelPrice{}, err
	}
	price.AccountingMode = strings.TrimSpace(price.AccountingMode)
	switch price.AccountingMode {
	case "", accountingModeInputExcludesCache, accountingModeInputIncludesCache:
	default:
		return ModelPrice{}, fmt.Errorf("accounting mode for model %q must be %q or %q", model, accountingModeInputExcludesCache, accountingModeInputIncludesCache)
	}

	price.Source = strings.TrimSpace(price.Source)
	switch price.Source {
	case "", priceSourceManual, priceSourceModelsDev:
	default:
		return ModelPrice{}, fmt.Errorf("price source for model %q is invalid", model)
	}
	price.CatalogProvider = strings.TrimSpace(price.CatalogProvider)
	price.CatalogModel = strings.TrimSpace(price.CatalogModel)
	if !validOptionalDimension(price.CatalogProvider) || !validOptionalDimension(price.CatalogModel) {
		return ModelPrice{}, fmt.Errorf("catalog identity for model %q is invalid or too long", model)
	}

	tiers, err := normalizeContextPriceTiers(model, "", price.ContextTiers)
	if err != nil {
		return ModelPrice{}, err
	}
	price.ContextTiers = tiers

	if len(price.ServiceTiers) > maxServiceTierPrices {
		return ModelPrice{}, fmt.Errorf("model %q must contain at most %d service tier prices", model, maxServiceTierPrices)
	}
	serviceTiers := make(map[string]ServiceTierPrice, len(price.ServiceTiers))
	for rawTier, rawSchedule := range price.ServiceTiers {
		tier := strings.ToLower(strings.TrimSpace(rawTier))
		if tier == "" || !validOptionalDimension(tier) {
			return ModelPrice{}, fmt.Errorf("service tier %q for model %q is invalid or too long", rawTier, model)
		}
		if _, exists := serviceTiers[tier]; exists {
			return ModelPrice{}, fmt.Errorf("service tier prices for model %q normalize to the same tier %q", model, tier)
		}
		if err := validateTokenRates(rawSchedule.tokenRates(), model, "service tier "+tier); err != nil {
			return ModelPrice{}, err
		}
		scheduleTiers, err := normalizeContextPriceTiers(model, "service tier "+tier+" ", rawSchedule.ContextTiers)
		if err != nil {
			return ModelPrice{}, err
		}
		rawSchedule.ContextTiers = scheduleTiers
		serviceTiers[tier] = rawSchedule
	}
	if len(serviceTiers) == 0 {
		price.ServiceTiers = nil
	} else {
		price.ServiceTiers = serviceTiers
	}
	return price, nil
}

func normalizeContextPriceTiers(model, scope string, input []ContextPriceTier) ([]ContextPriceTier, error) {
	if len(input) > maxContextPriceTiers {
		return nil, fmt.Errorf("model %q must contain at most %d %scontext price tiers", model, maxContextPriceTiers, scope)
	}
	tiers := append([]ContextPriceTier(nil), input...)
	sort.Slice(tiers, func(i, j int) bool { return tiers[i].Threshold < tiers[j].Threshold })
	for index := range tiers {
		tier := tiers[index]
		if tier.Threshold == 0 {
			return nil, fmt.Errorf("%scontext price threshold for model %q must be greater than zero", scope, model)
		}
		if index > 0 && tiers[index-1].Threshold == tier.Threshold {
			return nil, fmt.Errorf("%scontext price thresholds for model %q must be unique", scope, model)
		}
		if err := validateTokenRates(tier.tokenRates(), model, fmt.Sprintf("%scontext tier %d", scope, tier.Threshold)); err != nil {
			return nil, err
		}
	}
	return tiers, nil
}

func validateTokenRates(rates TokenRates, model, scope string) error {
	for _, value := range []struct {
		name  string
		price float64
	}{
		{name: "input", price: rates.Input},
		{name: "output", price: rates.Output},
		{name: "cache read", price: rates.CacheRead},
		{name: "cache creation", price: rates.CacheCreation},
	} {
		if err := validateTokenPrice(value.price, model, scope+" "+value.name); err != nil {
			return err
		}
	}
	return nil
}

func validateTokenPrice(value float64, model, kind string) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > maxTokenPricePerM {
		return fmt.Errorf("%s price for model %q must be between 0 and %.0f USD per 1M tokens", kind, model, maxTokenPricePerM)
	}
	return nil
}

func validOptionalDimension(value string) bool {
	return value == "" || utf8.ValidString(value) && utf8.RuneCountInString(value) <= maxDimensionRunes
}

func tokenRatesZero(rates TokenRates) bool {
	return rates.Input == 0 && rates.Output == 0 && rates.CacheRead == 0 && rates.CacheCreation == 0
}

func sameEditableModelPrice(left, right ModelPrice) bool {
	if left.Input != right.Input || left.Output != right.Output || left.CacheRead != right.CacheRead || left.CacheCreation != right.CacheCreation || left.AccountingMode != right.AccountingMode || len(left.ContextTiers) != len(right.ContextTiers) || len(left.ServiceTiers) != len(right.ServiceTiers) {
		return false
	}
	for index := range left.ContextTiers {
		if left.ContextTiers[index] != right.ContextTiers[index] {
			return false
		}
	}
	for tier, leftSchedule := range left.ServiceTiers {
		rightSchedule, ok := right.ServiceTiers[tier]
		if !ok || leftSchedule.Input != rightSchedule.Input || leftSchedule.Output != rightSchedule.Output || leftSchedule.CacheRead != rightSchedule.CacheRead || leftSchedule.CacheCreation != rightSchedule.CacheCreation || len(leftSchedule.ContextTiers) != len(rightSchedule.ContextTiers) {
			return false
		}
		for index := range leftSchedule.ContextTiers {
			if leftSchedule.ContextTiers[index] != rightSchedule.ContextTiers[index] {
				return false
			}
		}
	}
	return true
}

func cloneModelPrices(input map[string]ModelPrice) map[string]ModelPrice {
	result := make(map[string]ModelPrice, len(input))
	for model, price := range input {
		price.ContextTiers = append([]ContextPriceTier(nil), price.ContextTiers...)
		if len(price.ServiceTiers) > 0 {
			price.ServiceTiers = make(map[string]ServiceTierPrice, len(price.ServiceTiers))
			for tier, schedule := range input[model].ServiceTiers {
				schedule.ContextTiers = append([]ContextPriceTier(nil), schedule.ContextTiers...)
				price.ServiceTiers[tier] = schedule
			}
		}
		result[model] = price
	}
	return result
}

func clonePriceSyncSettings(input PriceSyncSettings) PriceSyncSettings {
	return PriceSyncSettings{
		ProviderPriority: append([]string(nil), input.ProviderPriority...),
		IgnoredSuffixes:  append([]string(nil), input.IgnoredSuffixes...),
		Mappings:         append([]PriceSyncMapping(nil), input.Mappings...),
	}
}

func clonePriceSyncMetadata(input *PriceSyncMetadata) *PriceSyncMetadata {
	if input == nil {
		return nil
	}
	result := *input
	return &result
}
