package codearts

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

const codeArtsModelAccessError = `{"error_code":"TM.000010051","error_msg":"您当前无权调用该模型，请联系企业管理员开通。"}`

func TestCodeArtsModelIDCasingFallback(t *testing.T) {
	var fake *t5Fake
	fake, _ = newT5Fake(t, func(w http.ResponseWriter, r *http.Request, call int) {
		captures := fake.snapshot()
		model := modelText(captures[len(captures)-1].Body["model"])
		if model == "glm-5.2" {
			t5WriteSuccess(w)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, codeArtsModelAccessError)
	})
	provider, accounts := newT5Provider(t, "u1")

	for index, model := range []string{"glm-5.2", "GLM-5.2"} {
		stream, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
			"model":    model,
			"messages": t5Messages(t5UserMessage("hi")),
		}, ""))
		if err != nil {
			t.Fatalf("request %d OpenChat(%s): %v", index, model, err)
		}
		frames := t5CollectFrames(t, stream)
		if !strings.Contains(strings.Join(frameTexts(frames), ""), "你好") {
			t.Fatalf("request %d frames = %v", index, frameTexts(frames))
		}
	}

	captures := fake.snapshot()
	models := make([]string, 0, len(captures))
	for _, capture := range captures {
		models = append(models, modelText(capture.Body["model"]))
	}
	want := []string{"GLM-5.2", "glm-5.2", "glm-5.2"}
	if strings.Join(models, ",") != strings.Join(want, ",") {
		t.Fatalf("upstream model attempts = %v, want %v", models, want)
	}
	account := accounts["u1"]
	account.mu.Lock()
	active := account.activeConcurrent
	account.mu.Unlock()
	if active != 0 {
		t.Fatalf("active concurrent = %d, want 0", active)
	}
}

func TestCodeArtsModelIDFallbackSkipsRoleOnlyPreflight(t *testing.T) {
	var fake *t5Fake
	fake, _ = newT5Fake(t, func(w http.ResponseWriter, r *http.Request, call int) {
		captures := fake.snapshot()
		model := modelText(captures[len(captures)-1].Body["model"])
		if model == "glm-5.2" {
			t5WriteSuccess(w)
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: "+codeArtsModelAccessError+"\n\n")
	})
	provider, _ := newT5Provider(t, "u1")
	stream, err := provider.OpenChat(context.Background(), t5Request(t, map[string]any{
		"model":    "GLM-5.2",
		"messages": t5Messages(t5UserMessage("hi")),
	}, ""))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	frames := t5CollectFrames(t, stream)
	if !strings.Contains(strings.Join(frameTexts(frames), ""), "你好") {
		t.Fatalf("frames = %v", frameTexts(frames))
	}
	captures := fake.snapshot()
	if len(captures) != 2 || modelText(captures[0].Body["model"]) != "GLM-5.2" ||
		modelText(captures[1].Body["model"]) != "glm-5.2" {
		t.Fatalf("upstream attempts = %d: %v", len(captures), captures)
	}
}

func frameTexts(frames [][]byte) []string {
	out := make([]string, 0, len(frames))
	for _, frame := range frames {
		out = append(out, string(frame))
	}
	return out
}
