package usagestats

import (
	"fmt"
	"time"
)

const (
	defaultRequestPageSize = 100
	maxRequestPageSize     = 500
)

// RequestDetail contains metadata and usage counters for one model request.
// Prompt and response content are intentionally never persisted.
type RequestDetail struct {
	Sequence uint64    `json:"sequence"`
	Time     time.Time `json:"time"`
	Dimensions
	Counters
	Result        string         `json:"result"`
	LatencyNS     uint64         `json:"latency_ns"`
	TTFTNS        uint64         `json:"ttft_ns"`
	GenerationNS  uint64         `json:"generation_ns"`
	TPS           float64        `json:"tps"`
	CacheHit      bool           `json:"cache_hit"`
	EstimatedCost *EstimatedCost `json:"estimated_cost,omitempty"`
}

type RequestPage struct {
	GeneratedAt       time.Time       `json:"generated_at"`
	Range             string          `json:"range"`
	PriceBookRevision uint64          `json:"price_book_revision"`
	Total             int             `json:"total"`
	Offset            int             `json:"offset"`
	Limit             int             `json:"limit"`
	Items             []RequestDetail `json:"items"`
}

func (p *RequestPage) Redact() {
	for i := range p.Items {
		p.Items[i].Dimensions.Redact()
	}
}

func (p *RequestPage) Reveal(decrypt DecryptFunc) {
	for i := range p.Items {
		p.Items[i].Dimensions.Reveal(decrypt)
	}
}

func requestDetailForUsage(usage normalizedUsage, sequence uint64) RequestDetail {
	generationNS := usage.LatencyNS
	if usage.TTFTNS > 0 && usage.LatencyNS >= usage.TTFTNS {
		generationNS = usage.LatencyNS - usage.TTFTNS
	}
	var tps float64
	if generationNS > 0 {
		tps = float64(usage.Counters.OutputTokens) / (float64(generationNS) / float64(time.Second))
	}
	result := "成功"
	if usage.Dimensions.Failed {
		result = "失败"
		if usage.Dimensions.FailureStatus > 0 {
			result = fmt.Sprintf("失败 (HTTP %d)", usage.Dimensions.FailureStatus)
		}
	}
	return RequestDetail{
		Sequence:     sequence,
		Time:         usage.RequestedAt.UTC(),
		Dimensions:   usage.Dimensions,
		Counters:     usage.Counters,
		Result:       result,
		LatencyNS:    usage.LatencyNS,
		TTFTNS:       usage.TTFTNS,
		GenerationNS: generationNS,
		TPS:          tps,
		CacheHit:     usage.Counters.CacheReadTokens > 0,
	}
}
