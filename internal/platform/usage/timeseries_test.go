package usage

import (
	"context"
	"testing"
	"time"
)

func TestTimeseriesBuckets(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	since := time.Date(2026, 1, 5, 10, 0, 0, 0, time.Local)
	until := time.Date(2026, 1, 5, 12, 0, 0, 0, time.Local)
	rows := []map[string]any{
		row(float64(since.Unix()), "m", "intl", "a", 10, 1, map[string]any{"credit": 1.0}),
		row(float64(since.Add(30*time.Minute).Unix()), "m", "intl", "a", 20, 2, map[string]any{"credit": 2.0}),
		row(float64(since.Add(60*time.Minute).Unix()), "m", "intl", "a", 0, 0, map[string]any{
			"error": true, "outcome": "failed", "credit": 3.0,
		}),
		row(float64(since.Add(70*time.Minute).Unix()), "m", "intl", "a", 0, 0, map[string]any{
			"outcome": "client_aborted", "credit": 9.0,
		}),
	}
	env := newTestEnv(t, rows)
	service := env.service()
	service.Now = func() time.Time { return now }
	payload, err := service.Timeseries(context.Background(), SeriesOptions{
		Range: "custom", Since: &since, Until: &until, BucketSeconds: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	series := payload["series"].([]SeriesBucket)
	if len(series) != 2 {
		t.Fatalf("series = %+v", series)
	}
	if series[0].At != float64(since.Unix()) || series[0].Requests != 2 || series[0].Credit != 3 {
		t.Fatalf("first bucket = %+v", series[0])
	}
	if series[1].Errors != 2 || series[1].Credit != 3 {
		t.Fatalf("second bucket = %+v", series[1])
	}
	credits := payload["credits"].([]SeriesCredit)
	if len(credits) != 4 {
		t.Fatalf("credit history = %+v", credits)
	}
	if credits[0].Credit != 9 || credits[1].Credit != 3 || credits[2].Credit != 2 || credits[3].Credit != 1 {
		t.Fatalf("credit order = %+v", credits)
	}

	customBucket, err := service.Timeseries(context.Background(), SeriesOptions{
		Range: "custom", Since: &since, Until: &until, BucketSeconds: 120,
	})
	if err != nil {
		t.Fatal(err)
	}
	customSeries := customBucket["series"].([]SeriesBucket)
	if len(customSeries) == 0 || customSeries[0].At != float64(since.Unix()) {
		t.Fatalf("custom bucket should anchor at window start: %+v", customSeries)
	}
}
