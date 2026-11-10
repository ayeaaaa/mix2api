package pricing

import "strings"

// BandMatches reports whether a band's single condition holds. It is exported
// for the panel/tooling; the cost engine uses the same predicate internally.
func BandMatches(band Band, epoch float64, promptTokens float64) bool {
	if band.MinPromptTokens > 0 {
		return !otherCondition(&band) && promptTokens >= float64(band.MinPromptTokens)
	}
	if !timeCondition(&band) {
		return false
	}
	day, hhmm := utcParts(epoch)
	return bandApplies(&band, day, hhmm)
}

// ValidBand reports whether a band carries exactly one usable condition. The
// shipped catalogue satisfies this; the check exists so a hand-edited pricing
// file cannot silently turn a conditional price into a base price.
func ValidBand(band Band) bool {
	length := band.MinPromptTokens > 0
	window := timeCondition(&band)
	if length == window {
		return false
	}
	if window {
		start := band.Start
		end := band.End
		if end == 0 {
			end = 2400
		}
		if start == end {
			return false
		}
		for _, day := range band.Days {
			if !validWeekday(day) {
				return false
			}
		}
	}
	return true
}

func validWeekday(day string) bool {
	name := strings.ToLower(strings.TrimSpace(day))
	if isShortWeekday(name) {
		return true
	}
	for _, long := range weekdayLongNames {
		if name == long {
			return true
		}
	}
	return false
}

func isShortWeekday(day string) bool {
	switch strings.ToLower(strings.TrimSpace(day)) {
	case "mon", "tue", "wed", "thu", "fri", "sat", "sun":
		return true
	}
	return false
}
