package providers

import "testing"

func TestHardenSchemaKeywords(t *testing.T) {
	schema := map[string]any{
		"minimum": "1", "maximum": "10", "minLength": "2", "uniqueItems": "true",
		"deprecated": "false", "description": 5, "title": nil, "format": 1,
		"enum": "x", "examples": []any{"x"}, "default": nil,
		"additionalProperties": "false",
	}
	HardenSchemaKeywords(schema)
	if schema["minimum"] != int64(1) || schema["maximum"] != int64(10) || schema["minLength"] != int64(2) {
		t.Fatalf("numeric keywords = %#v", schema)
	}
	if schema["uniqueItems"] != true || schema["deprecated"] != false {
		t.Fatalf("boolean keywords = %#v", schema)
	}
	if schema["description"] != "5" || schema["format"] != "1" {
		t.Fatalf("string keywords = %#v", schema)
	}
	if _, ok := schema["title"]; ok {
		t.Fatalf("null title kept: %#v", schema)
	}
	if _, ok := schema["enum"]; ok {
		t.Fatalf("non-list enum kept: %#v", schema)
	}
	if _, ok := schema["default"]; ok {
		t.Fatalf("null default kept: %#v", schema)
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("additionalProperties = %#v", schema)
	}
	first := schema["minimum"]
	HardenSchemaKeywords(schema)
	if schema["minimum"] != first {
		t.Fatalf("not idempotent: %#v", schema)
	}
}
