package workbuddy

import "testing"

func TestInlineKimiToolSchemas(t *testing.T) {
	body := map[string]any{
		"model": "kimi-k3.1-pro",
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "lookup",
				"parameters": map[string]any{
					"type": "object",
					"$defs": map[string]any{
						"Item": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"value": map[string]any{"type": "string"},
							},
						},
					},
					"properties": map[string]any{
						"item": map[string]any{"$ref": "#/$defs/Item"},
					},
				},
			},
		}},
	}
	inlineKimiToolSchemas(body, "kimi-k3.1-pro")
	tools := body["tools"].([]any)
	params := tools[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	item := params["properties"].(map[string]any)["item"].(map[string]any)
	if _, exists := item["$ref"]; exists {
		t.Fatalf("$ref survived: %#v", item)
	}
	if item["type"] != "object" || item["properties"].(map[string]any)["value"].(map[string]any)["type"] != "string" {
		t.Fatalf("expanded item = %#v", item)
	}
	if _, exists := params["$defs"]; exists {
		t.Fatalf("$defs survived: %#v", params)
	}
}

func TestInlineKimiLiteralRefDataIsNotTouched(t *testing.T) {
	body := map[string]any{
		"model": "kimi-k3.1-pro",
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "lookup",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"value": map[string]any{
							"type": "string", "default": map[string]any{"$ref": "keep"},
						},
					},
				},
			},
		}},
	}
	inlineKimiToolSchemas(body, "kimi-k3.1-pro")
	tools := body["tools"].([]any)
	params := tools[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	value := params["properties"].(map[string]any)["value"].(map[string]any)
	if value["default"].(map[string]any)["$ref"] != "keep" {
		t.Fatalf("literal $ref data was rewritten: %#v", value)
	}
}
