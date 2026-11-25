package codearts

import "testing"

func TestIntlModelsOwner(t *testing.T) {
	p, err := NewWithStation(newTestStore(t), International)
	if err != nil {
		t.Fatal(err)
	}
	models := p.wireModels([]Model{{"id": "GLM-5.2", "name": "GLM-5.2"}})
	if len(models) != 1 || models[0]["owned_by"] != "codearts-intl" || models[0]["provider"] != "codearts-intl" {
		t.Fatalf("models = %#v", models)
	}
}
