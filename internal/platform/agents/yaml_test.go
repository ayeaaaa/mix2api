package agents

import (
	"strings"
	"testing"
)

func TestYAMLRefsUpsertFlowMap(t *testing.T) {
	const flow = "refs:\n  {\n    AAA: 'v1',\n    BBB: 'v2'\n  }\n"

	out, err := yamlRefsUpsert(flow, "NEW_KEY", "new-val")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "NEW_KEY: new-val") {
		t.Fatalf("new key missing: %q", out)
	}
	if strings.Index(out, "NEW_KEY") > strings.Index(out, "}") {
		t.Fatalf("new key inserted after closing brace: %q", out)
	}
	if !strings.Contains(out, "BBB: 'v2',") {
		t.Fatalf("previous entry did not get a comma: %q", out)
	}
	if !strings.Contains(out, "AAA: 'v1'") {
		t.Fatalf("original entries changed: %q", out)
	}

	out, err = yamlRefsUpsert(flow, "AAA", "updated")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "AAA: updated,") {
		t.Fatalf("existing non-last entry lost its comma: %q", out)
	}
	if !strings.Contains(out, "BBB: 'v2'") {
		t.Fatalf("sibling entry changed: %q", out)
	}

	out, err = yamlRefsUpsert(flow, "BBB", "upd2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "BBB: upd2\n") {
		t.Fatalf("last entry gained a forced comma: %q", out)
	}

	const multiline = "refs:\n  {\n    LONG: 'line1\n      line2',\n    SHORT: 's'\n  }\n"
	out, err = yamlRefsUpsert(multiline, "LONG", "replaced")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "LONG: replaced,") || strings.Contains(out, "line2") {
		t.Fatalf("multi-line entry not replaced as one unit: %q", out)
	}
	out, err = yamlRefsUpsert(multiline, "NEW2", "x2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "NEW2: x2") || !strings.Contains(out, "line2',") {
		t.Fatalf("insert into multi-line flow map = %q", out)
	}
}

func TestYAMLRefsUpsertBlockMapStillWorks(t *testing.T) {
	out, err := yamlRefsUpsert("refs:\n  KEEP: v\n", "ADD", "a-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "KEEP: v") || !strings.Contains(out, "ADD: a-1") {
		t.Fatalf("block mapping changed: %q", out)
	}
}
