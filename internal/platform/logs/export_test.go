package logs

import (
	"bytes"
	"errors"
	"testing"
)

func TestLogExportRedactsLegacyRows(t *testing.T) {
	result := Result{
		Total: 2,
		Logs: []Entry{
			{
				ID:    1,
				TS:    "2026-10-01 00:00:00",
				Time:  "00:00:00",
				Level: "INFO",
				Tag:   "tasks",
				Msg:   "legacy row with 13812345678 in it",
			},
			{
				ID:    2,
				TS:    "2026-10-01 00:00:01",
				Time:  "00:00:01",
				Level: "ERROR",
				Tag:   "auth",
				Msg:   "callback /oauth/callback?code=abc&state=xyz",
			},
		},
		MaxID: 2,
	}

	var output bytes.Buffer
	if err := WriteExport(&output, result); err != nil {
		t.Fatalf("WriteExport: %v", err)
	}
	want := "[2026-10-01 00:00:00] [INFO] [tasks] legacy row with 138****5678 in it\n" +
		"[2026-10-01 00:00:01] [ERROR] [auth] callback /oauth/callback?code=<redacted>&state=<redacted>"
	if output.String() != want {
		t.Fatalf("export = %q, want %q", output.String(), want)
	}

	empty := bytes.Buffer{}
	if err := WriteExport(&empty, Result{}); err != nil {
		t.Fatalf("WriteExport(empty): %v", err)
	}
	if empty.Len() != 0 {
		t.Fatalf("empty export = %q", empty.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestLogExportPropagatesWriterError(t *testing.T) {
	err := WriteExport(failingWriter{}, Result{Logs: []Entry{{TS: "ts", Level: "INFO", Tag: "system", Msg: "row"}}})
	if err == nil {
		t.Fatal("WriteExport hid the writer error")
	}
}
