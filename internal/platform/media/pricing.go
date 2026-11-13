package media

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Cost estimates used only for pre-dispatch affordability checks. Measured
// prices, when learned from an upstream settlement, always win.
const (
	ImageCostEstimate           = 6.0
	VideoCostPerSecondCeiling   = 26.0
	DefaultVideoSecondsEstimate = 5
)

// Pricing is the on-disk evidence ledger for media credits. It never invents a
// price: an unobserved model/resolution returns nil.
type Pricing struct {
	Dir string

	mu       sync.Mutex
	video    map[videoPriceKey]float64
	image    map[string]float64
	loaded   bool
	videoOut string
	imageOut string
}

type videoPriceKey struct {
	Model      string
	Resolution string
	WithRef    bool
}

// NewPricing loads the ledger under dir. Missing or corrupt files are an empty
// ledger, never an error.
func NewPricing(dir string) *Pricing {
	p := &Pricing{Dir: dir}
	p.load()
	return p
}

// LoadPricing is an alias for NewPricing.
func LoadPricing(dir string) *Pricing { return NewPricing(dir) }

func (p *Pricing) paths() (string, string) {
	dir := strings.TrimSpace(p.Dir)
	if dir == "" {
		dir = "media"
	}
	return filepath.Join(dir, "video-prices.json"), filepath.Join(dir, "image-prices.json")
}

func (p *Pricing) load() {
	if p == nil {
		return
	}
	videoPath, imagePath := p.paths()
	p.videoOut = videoPath
	p.imageOut = imagePath
	p.video = map[videoPriceKey]float64{}
	p.image = map[string]float64{}

	if raw, err := os.ReadFile(imagePath); err == nil {
		var stored map[string]float64
		if json.Unmarshal(raw, &stored) == nil {
			for model, price := range stored {
				model = strings.TrimSpace(model)
				if model != "" && price > 0 {
					p.image[model] = price
				}
			}
		}
	}
	if raw, err := os.ReadFile(videoPath); err == nil {
		var stored map[string]float64
		if json.Unmarshal(raw, &stored) == nil {
			for key, price := range stored {
				if price <= 0 {
					continue
				}
				parts := strings.Split(key, "|")
				if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
					continue
				}
				ref := false
				if len(parts) > 2 {
					if parts[2] != "" && parts[2] != "ref" {
						continue
					}
					ref = parts[2] == "ref"
				}
				resolution := ""
				if len(parts) > 1 {
					resolution = parts[1]
				}
				p.video[videoPriceKey{Model: strings.TrimSpace(parts[0]), Resolution: strings.TrimSpace(resolution), WithRef: ref}] = price
			}
		}
	}
	p.loaded = true
}

func (p *Pricing) ensure() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if !p.loaded {
		p.mu.Unlock()
		p.load()
		p.mu.Lock()
	}
	p.mu.Unlock()
}

// LearnImage records a positive measured image credit. Zero/negative or empty
// values are ignored, matching v1's "never guess" rule.
func (p *Pricing) LearnImage(model string, credit float64) *float64 {
	if p == nil {
		return nil
	}
	model = strings.TrimSpace(model)
	if model == "" || credit <= 0 {
		return nil
	}
	price := round4(credit)
	p.ensure()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.image[model] = price
	p.saveImageLocked()
	return &price
}

// ImagePrice returns the measured per-image price, or nil when unknown.
func (p *Pricing) ImagePrice(model string) *float64 {
	if p == nil {
		return nil
	}
	p.ensure()
	p.mu.Lock()
	defer p.mu.Unlock()
	price, ok := p.image[strings.TrimSpace(model)]
	if !ok {
		return nil
	}
	return &price
}

// LearnVideo records credit/seconds for one model/resolution/reference tier.
func (p *Pricing) LearnVideo(model, resolution string, credit float64, seconds int, withReference bool) *float64 {
	if p == nil {
		return nil
	}
	model = strings.TrimSpace(model)
	if model == "" || credit <= 0 || seconds <= 0 {
		return nil
	}
	price := round4(credit / float64(seconds))
	key := videoPriceKey{Model: model, Resolution: strings.TrimSpace(resolution), WithRef: withReference}
	p.ensure()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.video[key] = price
	p.saveVideoLocked()
	return &price
}

// PricePerSecond returns a measured per-second price. exactOnly disables the
// same-tier maximum fallback, which is required for honest quoting.
func (p *Pricing) PricePerSecond(model, resolution string, exactOnly bool, withReference bool) *float64 {
	if p == nil {
		return nil
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	key := videoPriceKey{Model: model, Resolution: strings.TrimSpace(resolution), WithRef: withReference}
	p.ensure()
	p.mu.Lock()
	defer p.mu.Unlock()
	if price, ok := p.video[key]; ok {
		return &price
	}
	if exactOnly {
		return nil
	}
	best := 0.0
	found := false
	for candidate, price := range p.video {
		if candidate.Model == model && candidate.WithRef == withReference {
			if !found || price > best {
				best = price
				found = true
			}
		}
	}
	if !found {
		return nil
	}
	return &best
}

// CostHint is the pre-dispatch estimate: measured values when available,
// otherwise the v1 conservative ceiling. It is never a quote.
func (p *Pricing) CostHint(kind, model string, seconds int, resolution string, withReference bool) float64 {
	switch kind {
	case "image":
		if price := p.ImagePrice(model); price != nil {
			return *price
		}
		return ImageCostEstimate
	case "video":
		if seconds <= 0 {
			seconds = DefaultVideoSecondsEstimate
		}
		if price := p.PricePerSecond(model, resolution, false, withReference); price != nil {
			return round2(*price * float64(seconds))
		}
		return round2(VideoCostPerSecondCeiling * float64(seconds))
	default:
		return 0
	}
}

// VideoSnapshot returns a copy of the ledger keyed by v1's wire key.
func (p *Pricing) VideoSnapshot() map[string]float64 {
	if p == nil {
		return map[string]float64{}
	}
	p.ensure()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]float64, len(p.video))
	keys := make([]videoPriceKey, 0, len(p.video))
	for key := range p.video {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Model != keys[j].Model {
			return keys[i].Model < keys[j].Model
		}
		if keys[i].Resolution != keys[j].Resolution {
			return keys[i].Resolution < keys[j].Resolution
		}
		return !keys[i].WithRef && keys[j].WithRef
	})
	for _, key := range keys {
		wire := key.Model + "|" + key.Resolution
		if key.WithRef {
			wire += "|ref"
		}
		out[wire] = p.video[key]
	}
	return out
}

// ImageSnapshot returns a copy of the measured image ledger.
func (p *Pricing) ImageSnapshot() map[string]float64 {
	if p == nil {
		return map[string]float64{}
	}
	p.ensure()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]float64, len(p.image))
	for model, price := range p.image {
		out[model] = price
	}
	return out
}

func (p *Pricing) saveImageLocked() {
	_ = os.MkdirAll(filepath.Dir(p.imageOut), 0o700)
	raw, err := json.MarshalIndent(p.image, "", "  ")
	if err != nil {
		return
	}
	_ = atomicWriteFile(p.imageOut, append(raw, '\n'), 0o600)
}

func (p *Pricing) saveVideoLocked() {
	_ = os.MkdirAll(filepath.Dir(p.videoOut), 0o700)
	payload := map[string]float64{}
	for key, price := range p.video {
		wire := key.Model + "|" + key.Resolution
		if key.WithRef {
			wire += "|ref"
		}
		payload[wire] = price
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	_ = atomicWriteFile(p.videoOut, append(raw, '\n'), 0o600)
}

func round2(value float64) float64 {
	return float64(int64(value*100+0.5)) / 100
}

func round4(value float64) float64 {
	return float64(int64(value*10000+0.5)) / 10000
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	remove := func() { _ = os.Remove(name) }
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		remove()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		remove()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		remove()
		return err
	}
	if err := tmp.Close(); err != nil {
		remove()
		return err
	}
	if err := os.Rename(name, path); err != nil {
		remove()
		return err
	}
	return nil
}
