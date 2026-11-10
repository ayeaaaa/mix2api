package main

import (
	"strings"
	"testing"
)

func TestUpdateRequestForAction(t *testing.T) {
	request, err := updateRequestForAction("install", "v2.0.0", t.TempDir(), "/srv/mix2api")
	if err != nil || request.Action != "install" || request.Version != "2.0.0" || request.Tag != "v2.0.0" {
		t.Fatalf("install request = %#v err=%v", request, err)
	}
	if _, err := updateRequestForAction("install", "", t.TempDir(), "/srv/mix2api"); err == nil {
		t.Fatal("install without version accepted")
	}
	if _, err := updateRequestForAction("sideways", "2.0.0", t.TempDir(), "/srv/mix2api"); err == nil {
		t.Fatal("unknown action accepted")
	}
}

func TestUpdateCommandRequiresAction(t *testing.T) {
	var out, errOut strings.Builder
	if code := runUpdateCommand(nil, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(errOut.String(), "missing action") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestUpdateRecoverNoJournalIsIdle(t *testing.T) {
	root := t.TempDir()
	var out, errOut strings.Builder
	if code := runUpdateCommand([]string{"recover", "--root", root, "--request-dir", t.TempDir(), "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"state":"idle"`) || !strings.Contains(out.String(), "没有中断的更新") {
		t.Fatalf("output = %q", out.String())
	}
}
