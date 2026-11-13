package providers

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

var schemaNumericKeys = []string{
	"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum",
	"multipleOf", "minLength", "maxLength", "minItems", "maxItems",
	"minProperties", "maxProperties",
}

var schemaBooleanKeys = []string{"uniqueItems", "deprecated", "readOnly", "writeOnly"}
var schemaStringKeys = []string{"description", "title", "format", "pattern"}

// HardenSchemaKeywords coerces the keyword value shapes that the upstream Go
// validator rejects with code 11129. The existing schema normalizers repair
// type/properties/required/items; this final pass handles numeric and boolean
// strings, string keywords, enum/examples lists, additionalProperties, and
// null-valued keywords. It is idempotent.
func HardenSchemaKeywords(schema map[string]any) {
	if schema == nil {
		return
	}
	for _, key := range schemaNumericKeys {
		if value, ok := schema[key]; ok {
			if number, valid := schemaNumber(value); valid {
				schema[key] = number
			} else {
				delete(schema, key)
			}
		}
	}
	for _, key := range schemaBooleanKeys {
		if value, ok := schema[key]; ok {
			if flag, valid := schemaBool(value); valid {
				schema[key] = flag
			} else {
				delete(schema, key)
			}
		}
	}
	for _, key := range schemaStringKeys {
		value, ok := schema[key]
		if !ok {
			continue
		}
		if value == nil {
			delete(schema, key)
		} else if _, ok := value.(string); !ok {
			schema[key] = fmt.Sprint(value)
		}
	}
	if value, ok := schema["additionalProperties"]; ok {
		if nested, ok := value.(map[string]any); ok {
			HardenSchemaKeywords(nested)
		} else if flag, valid := schemaBool(value); valid {
			schema["additionalProperties"] = flag
		} else {
			delete(schema, "additionalProperties")
		}
	}
	if value, ok := schema["enum"]; ok {
		if _, valid := value.([]any); !valid {
			delete(schema, "enum")
		}
	}
	if value, ok := schema["examples"]; ok {
		if _, valid := value.([]any); !valid {
			delete(schema, "examples")
		}
	}
	for key, value := range schema {
		if value == nil {
			delete(schema, key)
		}
	}
}

func schemaNumber(value any) (any, bool) {
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Float64()
		if err != nil {
			return nil, false
		}
		return schemaNumberValue(number), true
	case float64:
		return schemaNumberValue(typed), true
	case float32:
		return schemaNumberValue(float64(typed)), true
	case int:
		return typed, true
	case int64:
		return typed, true
	case int32:
		return int64(typed), true
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return nil, false
		}
		return schemaNumberValue(number), true
	default:
		return nil, false
	}
}

func schemaNumberValue(number float64) any {
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return nil
	}
	if number == math.Trunc(number) && number >= math.MinInt64 && number <= math.MaxInt64 {
		return int64(number)
	}
	return number
}

func schemaBool(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}
