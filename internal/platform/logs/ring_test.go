package logs

import (
	"fmt"
	"testing"
)

func ringEntries(ring *Ring, count int, level, tag string) {
	for index := 0; index < count; index++ {
		ring.Append(Entry{
			TS:    "2026-11-10 00:00:00",
			Time:  "00:00:00",
			Level: level,
			Tag:   tag,
			Msg:   fmt.Sprintf("entry %d", index),
		})
	}
}

func TestRingCapacityAndFilters(t *testing.T) {
	ring := NewRing(2000)
	ringEntries(ring, 2500, "INFO", "system")

	all := ring.Query(Query{})
	if all.Total != 2000 {
		t.Fatalf("retained total = %d, want 2000", all.Total)
	}
	if len(all.Logs) != 2000 {
		t.Fatalf("retained rows = %d, want 2000", len(all.Logs))
	}
	if all.Logs[0].ID != 501 || all.Logs[len(all.Logs)-1].ID != 2500 {
		t.Fatalf("retained ids = %d..%d, want 501..2500", all.Logs[0].ID, all.Logs[len(all.Logs)-1].ID)
	}
	if all.MaxID != 2500 {
		t.Fatalf("max_id = %d, want 2500", all.MaxID)
	}

	limited := ring.Query(Query{Limit: 10})
	if limited.Total != 2000 || len(limited.Logs) != 10 {
		t.Fatalf("limit result = total %d rows %d, want total 2000 rows 10", limited.Total, len(limited.Logs))
	}
	if limited.Logs[0].ID != 2491 || limited.MaxID != 2500 {
		t.Fatalf("limited ids = %d..%d max %d, want 2491..2500/2500", limited.Logs[0].ID, limited.Logs[len(limited.Logs)-1].ID, limited.MaxID)
	}

	filterRing := NewRing(100)
	filterRing.Append(Entry{Level: "INFO", Tag: "Tasks", Msg: "first needle"})
	filterRing.Append(Entry{Level: "ERROR", Tag: "Chat", Msg: "second needle"})
	filterRing.Append(Entry{Level: "WARN", Tag: "tasks", Msg: "unrelated"})

	if got := filterRing.Query(Query{Level: "error"}); got.Total != 1 || got.Logs[0].Level != "ERROR" {
		t.Fatalf("case-insensitive level filter = %#v", got)
	}
	if got := filterRing.Query(Query{Tag: "TASKS"}); got.Total != 2 {
		t.Fatalf("tag filter total = %d, want 2", got.Total)
	}
	if got := filterRing.Query(Query{Search: "NEEDLE"}); got.Total != 2 {
		t.Fatalf("search msg total = %d, want 2", got.Total)
	}
	if got := filterRing.Query(Query{Search: "chat"}); got.Total != 1 || got.Logs[0].Tag != "Chat" {
		t.Fatalf("search tag result = %#v", got)
	}

	since := filterRing.Query(Query{SinceID: 1, Limit: 1})
	if since.Total != 2 || len(since.Logs) != 2 {
		t.Fatalf("since_id must ignore limit: total %d rows %d", since.Total, len(since.Logs))
	}
	if since.Logs[0].ID != 2 || since.Logs[1].ID != 3 || since.MaxID != 3 {
		t.Fatalf("since_id ids = %#v max %d", since.Logs, since.MaxID)
	}
	if got := filterRing.Query(Query{SinceID: 99}); got.Total != 0 || len(got.Logs) != 0 || got.MaxID != 99 {
		t.Fatalf("empty since_id result = %#v", got)
	}
}
