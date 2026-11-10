package update

import "sort"

// SortBackups returns newest-first by creation time, then version descending.
func SortBackups(items []Backup) []Backup {
	out := append([]Backup(nil), items...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].Version > out[j].Version
	})
	return out
}

// CompleteBackups filters incomplete rows for rollback eligibility.
func CompleteBackups(items []Backup) []Backup {
	out := []Backup{}
	for _, item := range items {
		if item.Complete && IsReleaseVersion(item.Version) && item.Version != "0.0.0" {
			out = append(out, item)
		}
	}
	return SortBackups(out)
}

// ClampKeepBackups keeps the panel value inside v1's 0..3 range.
func ClampKeepBackups(value int) int {
	if value < 0 {
		return 0
	}
	if value > KeepBackupsMax {
		return KeepBackupsMax
	}
	return value
}
