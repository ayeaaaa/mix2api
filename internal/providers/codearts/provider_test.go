package codearts

import (
	"path/filepath"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
)

func TestProviderUsesStationIdentity(t *testing.T) {
	store := config.New(filepath.Join(t.TempDir(), "settings.json"))
	cn, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	if cn.ID() != "codearts" || cn.Realm() != "codearts" || cn.Station().ID != "mainland" {
		t.Fatalf("mainland identity = %q/%q/%q", cn.ID(), cn.Realm(), cn.Station().ID)
	}
	intl, err := NewWithStation(store, International)
	if err != nil {
		t.Fatal(err)
	}
	if intl.ID() != "codearts-intl" || intl.Realm() != "codearts-intl" || intl.Station().ID != "international" {
		t.Fatalf("intl identity = %q/%q/%q", intl.ID(), intl.Realm(), intl.Station().ID)
	}
}
