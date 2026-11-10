package media

import (
	"encoding/json"
	"testing"
)

func TestVideoNumericParamsAnd4KResolution(t *testing.T) {
	generation := map[string]any{
		"defaultSeconds": 5,
		"resolutions":    []any{"720P", "1080P", "4K"},
		"aspectRatios":   []any{"16:9", "9:16"},
	}
	params, err := ResolveVideoParams(map[string]any{
		"prompt": "p", "seconds": json.Number("10"), "size": "4096x2160",
	}, generation)
	if err != nil {
		t.Fatal(err)
	}
	if params["seconds"] != 10 || params["resolution"] != "4K" || params["aspect_ratio"] != "16:9" {
		t.Fatalf("params = %#v", params)
	}
	if _, _, _, err := ParseSize("4096x2160"); err == nil {
		t.Fatal("image pixel cap stopped applying")
	}
	if _, _, normalized, err := ParseDimensions("4096x2160"); err != nil || normalized != "4096x2160" {
		t.Fatalf("dimensions = %q err=%v", normalized, err)
	}
}

func TestReferenceMaterialExclusivity(t *testing.T) {
	generation := map[string]any{
		"framesExclusiveWithReferenceMaterials": true,
		"maxReferenceImages":                    2,
	}
	err := ValidateReferenceCounts(map[string]any{
		"image": "data:image/png;base64,aGk=",
		"reference_images": []any{
			map[string]any{"image_url": "https://example.com/a.png"},
		},
	}, generation)
	if err == nil {
		t.Fatal("frame/reference exclusivity not enforced")
	}
	generation["framesExclusiveWithReferenceMaterials"] = false
	generation["allowMixedReferenceMaterials"] = false
	err = ValidateReferenceCounts(map[string]any{
		"reference_images": []any{"https://example.com/a.png"},
		"reference_videos": []any{"https://example.com/a.mp4"},
	}, generation)
	if err == nil {
		t.Fatal("mixed reference materials not rejected")
	}
}
