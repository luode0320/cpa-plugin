package usagestats

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	exchangeRateURL             = "https://open.er-api.com/v6/latest/USD"
	exchangeRateRequestTimeout  = 8 * time.Second
	exchangeRateMaxResponseSize = 1 << 20
	exchangeRateFreshTTL        = time.Hour
	exchangeRateStaleTTL        = 24 * time.Hour
	exchangeRateRetryBackoff    = time.Minute
)

type exchangeRateFetcher struct {
	client *http.Client
	url    string
}

type exchangeRateService struct {
	mu         sync.Mutex
	fetcher    *exchangeRateFetcher
	now        func() time.Time
	cached     *ExchangeRateResponse
	freshUntil time.Time
	staleUntil time.Time
	retryAfter time.Time
	lastError  error
}

type exchangeRateProviderResponse struct {
	Result             string             `json:"result"`
	BaseCode           string             `json:"base_code"`
	TimeLastUpdateUnix int64              `json:"time_last_update_unix"`
	Rates              map[string]float64 `json:"rates"`
}

type ExchangeRateResponse struct {
	SchemaVersion uint32    `json:"schema_version"`
	Base          string    `json:"base"`
	Quote         string    `json:"quote"`
	Rate          float64   `json:"rate"`
	EffectiveAt   time.Time `json:"effective_at"`
	FetchedAt     time.Time `json:"fetched_at"`
	Source        string    `json:"source"`
	Stale         bool      `json:"stale"`
}

func newExchangeRateFetcher() *exchangeRateFetcher {
	expected, _ := url.Parse(exchangeRateURL)
	client := &http.Client{
		Timeout: exchangeRateRequestTimeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("exchange-rate provider redirected too many times")
			}
			if request.URL.Scheme != "https" || !strings.EqualFold(request.URL.Host, expected.Host) {
				return errors.New("exchange-rate redirect must remain on the configured HTTPS host")
			}
			return nil
		},
	}
	return &exchangeRateFetcher{client: client, url: exchangeRateURL}
}

func newExchangeRateService() *exchangeRateService {
	return &exchangeRateService{fetcher: newExchangeRateFetcher(), now: nowUTC}
}

func (s *exchangeRateService) latest() (ExchangeRateResponse, error) {
	if s == nil {
		return ExchangeRateResponse{}, withStatus(http.StatusServiceUnavailable, "exchange-rate service is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	if s.cached != nil && now.Before(s.freshUntil) {
		result := *s.cached
		result.Stale = false
		return result, nil
	}
	if s.lastError != nil && now.Before(s.retryAfter) {
		if s.cached != nil && now.Before(s.staleUntil) {
			result := *s.cached
			result.Stale = true
			return result, nil
		}
		return ExchangeRateResponse{}, publicExchangeRateError(s.lastError)
	}

	fetcher := s.fetcher
	if fetcher == nil {
		fetcher = newExchangeRateFetcher()
	}
	ctx, cancel := context.WithTimeout(context.Background(), exchangeRateRequestTimeout)
	defer cancel()
	provider, err := fetcher.fetch(ctx)
	if err == nil {
		effectiveAt := now
		if provider.TimeLastUpdateUnix > 0 {
			effectiveAt = time.Unix(provider.TimeLastUpdateUnix, 0).UTC()
		}
		result := ExchangeRateResponse{
			SchemaVersion: 1,
			Base:          "USD",
			Quote:         "CNY",
			Rate:          provider.Rates["CNY"],
			EffectiveAt:   effectiveAt,
			FetchedAt:     now,
			Source:        "open.er-api.com",
		}
		s.cached = &result
		s.freshUntil = now.Add(exchangeRateFreshTTL)
		s.staleUntil = now.Add(exchangeRateStaleTTL)
		s.retryAfter = time.Time{}
		s.lastError = nil
		return result, nil
	}
	s.retryAfter = now.Add(exchangeRateRetryBackoff)
	s.lastError = err
	if s.cached != nil && now.Before(s.staleUntil) {
		result := *s.cached
		result.Stale = true
		return result, nil
	}
	return ExchangeRateResponse{}, publicExchangeRateError(err)
}

func (f *exchangeRateFetcher) fetch(ctx context.Context) (exchangeRateProviderResponse, error) {
	if f == nil || f.client == nil {
		return exchangeRateProviderResponse{}, errors.New("exchange-rate HTTP client is unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
	if err != nil {
		return exchangeRateProviderResponse{}, fmt.Errorf("create exchange-rate request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "cap-token-usage-tracker/"+version)
	response, err := f.client.Do(request)
	if err != nil {
		return exchangeRateProviderResponse{}, fmt.Errorf("fetch exchange rate: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return exchangeRateProviderResponse{}, fmt.Errorf("fetch exchange rate: HTTP %d", response.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return exchangeRateProviderResponse{}, errors.New("fetch exchange rate: expected application/json response")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, exchangeRateMaxResponseSize+1))
	if err != nil {
		return exchangeRateProviderResponse{}, fmt.Errorf("read exchange rate: %w", err)
	}
	if len(body) > exchangeRateMaxResponseSize {
		return exchangeRateProviderResponse{}, fmt.Errorf("read exchange rate: response exceeds %d bytes", exchangeRateMaxResponseSize)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	var result exchangeRateProviderResponse
	if err := decoder.Decode(&result); err != nil {
		return exchangeRateProviderResponse{}, fmt.Errorf("decode exchange rate: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return exchangeRateProviderResponse{}, errors.New("decode exchange rate: trailing JSON value")
		}
		return exchangeRateProviderResponse{}, fmt.Errorf("decode exchange rate: %w", err)
	}
	if !strings.EqualFold(result.Result, "success") || !strings.EqualFold(result.BaseCode, "USD") {
		return exchangeRateProviderResponse{}, errors.New("decode exchange rate: provider returned an unsuccessful or unexpected base response")
	}
	rate, ok := result.Rates["CNY"]
	if !ok || math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0 {
		return exchangeRateProviderResponse{}, errors.New("decode exchange rate: missing or invalid CNY rate")
	}
	return result, nil
}

func publicExchangeRateError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return withStatus(http.StatusGatewayTimeout, "exchange-rate request timed out")
	}
	return withStatus(http.StatusBadGateway, "exchange-rate request failed")
}
