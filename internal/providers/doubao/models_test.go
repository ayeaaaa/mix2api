package doubao

import "testing"

func TestListModels(t *testing.T) {
	items := ListModels()
	if len(items) != 12 {
		t.Fatalf("models = %d, want 12", len(items))
	}
	seen := map[string]bool{}
	for _, item := range items {
		id, _ := item["id"].(string)
		if id == "" || seen[id] {
			t.Fatalf("bad/duplicate model: %#v", item)
		}
		seen[id] = true
	}
	if !seen["deepseek-v4.1-flash"] || !seen["glm-5.3"] || !seen["doubao-fast"] {
		t.Fatalf("missing expected models: %#v", seen)
	}
}

func TestFindModelAliases(t *testing.T) {
	for _, name := range []string{"doubao-fast", "快速", "0", "doubao", "fast"} {
		model, ok := FindModel(name)
		if !ok || model.ID != "doubao-fast" {
			t.Fatalf("FindModel(%q) = %#v, %v", name, model, ok)
		}
	}
	if model, ok := FindModel("person-deepseek-v4-1-flash-261017"); !ok || model.ID != "deepseek-v4.1-flash" {
		t.Fatalf("upstream key alias = %#v, %v", model, ok)
	}
	if _, ok := FindModel("no-such-model"); ok {
		t.Fatal("unknown model accepted")
	}
}
