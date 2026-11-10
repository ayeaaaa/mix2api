// Package proxy stores the panel's proxy-slot list in accounts/settings.json
// and rebinds provider account pools through the optional
// providers.ProxySlotBinder hook.
package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

var slotIDPattern = regexp.MustCompile(`^slot-(\d+)$`)

// Slot is one configured proxy exit. Probe fields are preserved on save so a
// panel round-trip does not discard the last successful exit lookup.
type Slot struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Enabled     bool   `json:"enabled"`
	IP          string `json:"ip"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	IPType      string `json:"ip_type"`
	ISP         string `json:"isp"`
	ASN         string `json:"asn"`
	ProbedAt    int64  `json:"probed_at"`

	Bound int    `json:"bound"`
	Label string `json:"label"`
}

// Slots manages proxy slots for a settings file.
type Slots struct {
	store     *config.Store
	providers []providers.Provider

	lastUnbound int
}

// New returns a slot service. The provider list is consulted only through the
// optional ProxySlotBinder interface.
func New(store *config.Store, list []providers.Provider) *Slots {
	return &Slots{store: store, providers: list}
}

// List returns every slot in stored order with bound counts and display
// labels. Duplicate URLs are dropped first-wins.
func (s *Slots) List() ([]Slot, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("proxy: settings store is required")
	}
	data, err := s.store.Load()
	if err != nil {
		return nil, fmt.Errorf("proxy: load settings: %w", err)
	}
	slots := slotsFromData(data)
	if s != nil {
		counts := s.Bindings()
		for i := range slots {
			slots[i].Bound = counts[slots[i].ID]
			slots[i].Label = Label(slots[i])
		}
	}
	return slots, nil
}

// Replace writes the submitted list using stable, never-recycled slot ids and
// then re-applies bindings to every provider. The returned list carries the
// refreshed bound counts.
func (s *Slots) Replace(incoming []Slot) ([]Slot, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("proxy: settings store is required")
	}
	var saved []Slot
	_, err := s.store.Update(func(data map[string]any) error {
		storedRaw, _ := data["proxy_slots"].([]any)
		seq := slotSeq(data)
		for _, raw := range storedRaw {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if match := slotIDPattern.FindStringSubmatch(strings.TrimSpace(text(entry["id"]))); match != nil {
				number, _ := strconv.Atoi(match[1])
				if number > seq {
					seq = number
				}
			}
		}
		for _, slot := range incoming {
			if match := slotIDPattern.FindStringSubmatch(strings.TrimSpace(slot.ID)); match != nil {
				number, _ := strconv.Atoi(match[1])
				if number > seq {
					seq = number
				}
			}
		}
		if len(storedRaw) > seq {
			seq = len(storedRaw)
		}

		cleaned := make([]Slot, 0, len(incoming))
		seenURLs := make(map[string]bool, len(incoming))
		seenIDs := make(map[string]bool, len(incoming))
		for _, raw := range incoming {
			entry, ok := cleanSlot(slotMap(raw), "")
			if !ok {
				continue
			}
			if seenURLs[entry.URL] {
				continue
			}
			seenURLs[entry.URL] = true
			if entry.ID == "" || seenIDs[entry.ID] {
				seq++
				entry.ID = fmt.Sprintf("slot-%d", seq)
			}
			seenIDs[entry.ID] = true
			cleaned = append(cleaned, entry)
		}

		data["proxy_slots"] = slotMaps(cleaned)
		data["proxy_slot_seq"] = seq
		saved = cleaned
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.applyBindings(transportSlots(saved))
	counts := s.Bindings()
	for i := range saved {
		saved[i].Bound = counts[saved[i].ID]
		saved[i].Label = Label(saved[i])
	}
	return saved, nil
}

// Update merges fields into one stored slot. Defaults are applied only to
// fields that are still empty at write time.
func (s *Slots) Update(id string, fields map[string]any, defaults map[string]any) (Slot, bool, error) {
	if s == nil || s.store == nil {
		return Slot{}, false, errors.New("proxy: settings store is required")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return Slot{}, false, nil
	}
	var found Slot
	var ok bool
	_, err := s.store.Update(func(data map[string]any) error {
		slots := slotsFromData(data)
		for index := range slots {
			if slots[index].ID != id {
				continue
			}
			raw := slotMap(slots[index])
			for key, value := range fields {
				raw[key] = value
			}
			for key, value := range defaults {
				if isEmpty(raw[key]) {
					raw[key] = value
				}
			}
			entry, cleanOK := cleanSlot(raw, slots[index].ID)
			if cleanOK {
				slots[index] = entry
				found = entry
				ok = true
			}
			break
		}
		if ok {
			data["proxy_slots"] = slotMaps(slots)
		}
		return nil
	})
	if err != nil {
		return Slot{}, false, err
	}
	if ok {
		found.Bound = s.Bindings()[found.ID]
		found.Label = Label(found)
	}
	return found, ok, nil
}

// Bindings sums ProxySlotBindings across every provider that implements the
// optional hook. Providers without the hook contribute nothing.
func (s *Slots) Bindings() map[string]int {
	out := map[string]int{}
	if s == nil {
		return out
	}
	for _, provider := range s.providers {
		binder, ok := provider.(providers.ProxySlotBinder)
		if !ok {
			continue
		}
		for id, count := range binder.ProxySlotBindings() {
			if strings.TrimSpace(id) == "" || count <= 0 {
				continue
			}
			out[id] += count
		}
	}
	return out
}

// Views returns the panel slot maps for settings composition.
func (s *Slots) Views() []map[string]any {
	slots, err := s.List()
	if err != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(slots))
	for _, slot := range slots {
		out = append(out, map[string]any{
			"id": slot.ID, "name": slot.Name, "url": slot.URL,
			"enabled": slot.Enabled, "ip": slot.IP, "country": slot.Country,
			"country_code": slot.CountryCode, "ip_type": slot.IPType,
			"isp": slot.ISP, "asn": slot.ASN, "probed_at": slot.ProbedAt,
			"bound": slot.Bound, "label": slot.Label,
		})
	}
	return out
}

// LastUnbound returns the number of accounts unbound by the most recent
// Replace.
func (s *Slots) LastUnbound() int {
	if s == nil {
		return 0
	}
	return s.lastUnbound
}

func (s *Slots) applyBindings(slots transport.Slots) {
	if s == nil {
		return
	}
	unbound := 0
	for _, provider := range s.providers {
		binder, ok := provider.(providers.ProxySlotBinder)
		if !ok {
			continue
		}
		unbound += binder.ApplyProxySlots(slots)
	}
	s.lastUnbound = unbound
}

func slotsFromData(data map[string]any) []Slot {
	rawList, ok := data["proxy_slots"].([]any)
	if !ok {
		return []Slot{}
	}
	out := make([]Slot, 0, len(rawList))
	seenURLs := make(map[string]bool, len(rawList))
	for _, raw := range rawList {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		slot, ok := cleanSlot(entry, "")
		if !ok || seenURLs[slot.URL] {
			continue
		}
		seenURLs[slot.URL] = true
		if slot.ID == "" {
			slot.ID = nextSlotID(out)
		}
		out = append(out, slot)
	}
	return out
}

func cleanSlot(raw map[string]any, fallbackID string) (Slot, bool) {
	if raw == nil {
		return Slot{}, false
	}
	proxyURL := transport.NormalizeProxyURL(text(raw["url"]))
	if proxyURL == "" {
		return Slot{}, false
	}
	id := strings.TrimSpace(text(raw["id"]))
	if id == "" {
		id = fallbackID
	}
	probedAt := int64(0)
	if value, ok := integer(raw["probed_at"]); ok {
		probedAt = value
	}
	enabled := true
	if value, ok := raw["enabled"].(bool); ok && !value {
		enabled = false
	}
	return Slot{
		ID:          id,
		Name:        strings.TrimSpace(text(raw["name"])),
		URL:         proxyURL,
		Enabled:     enabled,
		IP:          strings.TrimSpace(text(raw["ip"])),
		Country:     strings.TrimSpace(text(raw["country"])),
		CountryCode: strings.ToUpper(strings.TrimSpace(text(raw["country_code"]))),
		IPType:      strings.ToLower(strings.TrimSpace(text(raw["ip_type"]))),
		ISP:         strings.TrimSpace(text(raw["isp"])),
		ASN:         strings.TrimSpace(text(raw["asn"])),
		ProbedAt:    probedAt,
	}, true
}

func nextSlotID(existing []Slot) string {
	highest := 0
	for _, slot := range existing {
		if match := slotIDPattern.FindStringSubmatch(slot.ID); match != nil {
			number, _ := strconv.Atoi(match[1])
			if number > highest {
				highest = number
			}
		}
	}
	return fmt.Sprintf("slot-%d", highest+1)
}

func slotSeq(data map[string]any) int {
	if data == nil {
		return 0
	}
	if value, ok := integer(data["proxy_slot_seq"]); ok && value > 0 {
		return int(value)
	}
	return 0
}

func integer(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		return int64(typed), true
	case json.Number:
		if number, err := typed.Int64(); err == nil {
			return number, true
		}
		if number, err := typed.Float64(); err == nil {
			return int64(number), true
		}
	case string:
		number, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return number, err == nil
	}
	return 0, false
}

func text(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}

func isEmpty(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(typed) == ""
	case int:
		return typed == 0
	case int64:
		return typed == 0
	case float64:
		return typed == 0
	case json.Number:
		return typed.String() == "" || typed.String() == "0"
	case bool:
		return !typed
	default:
		return false
	}
}

func slotMap(slot Slot) map[string]any {
	return map[string]any{
		"id":           slot.ID,
		"name":         slot.Name,
		"url":          slot.URL,
		"enabled":      slot.Enabled,
		"ip":           slot.IP,
		"country":      slot.Country,
		"country_code": slot.CountryCode,
		"ip_type":      slot.IPType,
		"isp":          slot.ISP,
		"asn":          slot.ASN,
		"probed_at":    slot.ProbedAt,
	}
}

func slotMaps(slots []Slot) []any {
	out := make([]any, len(slots))
	for i, slot := range slots {
		out[i] = slotMap(slot)
	}
	return out
}

func transportSlots(slots []Slot) transport.Slots {
	out := make(transport.Slots, 0, len(slots))
	for _, slot := range slots {
		out = append(out, transport.Slot{ID: slot.ID, Name: slot.Name, URL: slot.URL, Enabled: slot.Enabled})
	}
	return out
}

// Label mirrors v1's auto-label: the operator's name wins, otherwise country
// plus the known residential/datacenter kind, otherwise the stable id. An
// unknown kind or a missing country falls back to the id rather than showing a
// label that cannot tell two slots apart.
func Label(slot Slot) string {
	if strings.TrimSpace(slot.Name) != "" {
		return strings.TrimSpace(slot.Name)
	}
	country := strings.TrimSpace(slot.Country)
	if country == "" {
		return slot.ID
	}
	switch strings.ToLower(strings.TrimSpace(slot.IPType)) {
	case "residential":
		return country + " 住宅"
	case "datacenter":
		return country + " 机房"
	default:
		return country
	}
}
