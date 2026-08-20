package web

import (
	"math"
	"testing"
)

func TestEstimateUsageCostSeparatesNewCachedAndOutputTokens(t *testing.T) {
	got := estimateUsageCost("gpt-5.2", 1_000_000, 1_000_000, 1_000_000)
	want := 1.75 + 14.00 + 0.175
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("estimateUsageCost() = %v, want %v", got, want)
	}
}

func TestEstimateUsageCostIncrementalRequestChargesCachedHistory(t *testing.T) {
	got := estimateUsageCost("gpt-5.2", 1_000, 500, 100_000)
	want := float64(1_000)/1_000_000*1.75 +
		float64(500)/1_000_000*14.00 +
		float64(100_000)/1_000_000*0.175
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("estimateUsageCost() = %v, want %v", got, want)
	}
}

func TestPriceForModelIsCaseInsensitive(t *testing.T) {
	got := priceForModel(" GPT-5.2-REASONING ")
	if got.InputPerMillion != 1.75 || got.CachedInputPerMillion != 0.175 || got.OutputPerMillion != 14.00 {
		t.Fatalf("unexpected model price: %+v", got)
	}
}

func TestPriceForUnknownModelUsesEstimatedDefault(t *testing.T) {
	got := priceForModel("unknown-model")
	if got != defaultModelPrice {
		t.Fatalf("priceForModel() = %+v, want default %+v", got, defaultModelPrice)
	}
	if !got.Estimated || got.Currency != "USD" {
		t.Fatalf("default price must be marked as a USD estimate: %+v", got)
	}
}
