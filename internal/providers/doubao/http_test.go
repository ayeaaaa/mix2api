package doubao

import (
	"strings"
	"testing"
)

func TestBuildBodyNewAndContinuing(t *testing.T) {
	model, _ := FindModel("doubao-fast")
	fresh := buildBody(model, "hello", &conversation{})
	option := fresh["option"].(map[string]any)
	if option["need_create_conversation"] != true {
		t.Fatalf("fresh need_create = %#v", option["need_create_conversation"])
	}
	meta := fresh["client_meta"].(map[string]any)
	if _, ok := meta["local_conversation_id"].(string); !ok {
		t.Fatalf("fresh local_conversation_id = %#v", meta["local_conversation_id"])
	}
	messages := fresh["messages"].([]any)
	block := messages[0].(map[string]any)["content_block"].([]any)[0].(map[string]any)
	if !strings.Contains(block["block_id"].(string), "-") {
		t.Fatalf("block id is not UUID-shaped: %#v", block["block_id"])
	}

	conv := &conversation{ConversationID: "c1", SectionID: "s1", LastMessageIndex: float64(7), LocalConversationID: "local_x"}
	continued := buildBody(model, "next", conv)
	option = continued["option"].(map[string]any)
	if option["need_create_conversation"] != false {
		t.Fatalf("continued need_create = %#v", option["need_create_conversation"])
	}
	meta = continued["client_meta"].(map[string]any)
	if meta["conversation_id"] != "c1" || meta["last_section_id"] != "s1" {
		t.Fatalf("continued client_meta = %#v", meta)
	}
	if _, ok := meta["local_conversation_id"]; ok {
		t.Fatalf("continued client_meta must not create local id: %#v", meta)
	}
}

func TestBuildQuery(t *testing.T) {
	p := &Provider{cfg: Config{AID: "1044603"}}
	account := &Account{
		Cookies: map[string]string{"s_v_web_id": "verify%2Fabc"},
		Device:  map[string]string{"web_id": "wid", "tea_uuid": "tea", "web_tab_id": "tab"},
	}
	query := p.buildQuery(account)
	for _, want := range []string{"aid=1044603", "device_id=wid", "tea_uuid=tea", "web_tab_id=tab", "fp=verify%2Fabc", "version_code=20800"} {
		if !strings.Contains(query, want) {
			t.Fatalf("query %q missing %q", query, want)
		}
	}
}
