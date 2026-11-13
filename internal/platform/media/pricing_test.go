package media

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMediaPricingLedger(t *testing.T) {
	dir := t.TempDir()
	pricing := NewPricing(dir)
	if pricing.ImagePrice("img") != nil || pricing.PricePerSecond("vid", "720P", true, false) != nil {
		t.Fatal("unknown price was invented")
	}
	if price := pricing.LearnImage("img", 5.71); price == nil || *price != 5.71 {
		t.Fatalf("image learn = %v", price)
	}
	if pricing.LearnImage("img", 0) != nil {
		t.Fatal("zero image credit was learned")
	}
	if price := pricing.LearnVideo("vid", "720P", 57.14, 4, false); price == nil || *price != 14.285 {
		t.Fatalf("video learn = %v", price)
	}
	if price := pricing.LearnVideo("vid", "720P", 142.86, 5, true); price == nil || *price != 28.572 {
		t.Fatalf("reference learn = %v", price)
	}
	if price := pricing.LearnVideo("vid", "1080P", 0, 5, false); price != nil {
		t.Fatal("zero video credit learned")
	}
	if price := pricing.PricePerSecond("vid", "720P", true, false); price == nil || *price != 14.285 {
		t.Fatalf("exact price = %v", price)
	}
	if price := pricing.PricePerSecond("vid", "1080P", false, false); price == nil || *price != 14.285 {
		t.Fatalf("fallback price = %v", price)
	}
	if pricing.PricePerSecond("vid", "1080P", true, false) != nil {
		t.Fatal("exact-only guessed a different resolution")
	}
	if hint := pricing.CostHint("video", "vid", 4, "720P", false); hint != 57.14 {
		t.Fatalf("video hint = %v", hint)
	}
	if hint := pricing.CostHint("image", "missing", 0, "", false); hint != ImageCostEstimate {
		t.Fatalf("image fallback = %v", hint)
	}

	// Round-trip a fresh instance.
	reloaded := NewPricing(dir)
	if price := reloaded.ImagePrice("img"); price == nil || *price != 5.71 {
		t.Fatalf("reloaded image = %v", price)
	}
	if price := reloaded.PricePerSecond("vid", "720P", true, true); price == nil || *price != 28.572 {
		t.Fatalf("reloaded ref = %v", price)
	}

	// A corrupt ledger is ignored, never fatal.
	if err := os.WriteFile(filepath.Join(dir, "video-prices.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "image-prices.json"), []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := NewPricing(dir)
	if empty.ImagePrice("img") != nil || empty.PricePerSecond("vid", "720P", false, false) != nil {
		t.Fatal("corrupt ledger retained prices")
	}
}
