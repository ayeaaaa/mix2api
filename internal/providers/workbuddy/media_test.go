package workbuddy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func TestMediaCapabilities(t *testing.T) {
	provider := &Provider{}
	caps := provider.Capabilities()
	if !caps.MediaGeneration || !caps.MediaImages || !caps.MediaVideos || !caps.WindowProbe || !caps.QuotaScan {
		t.Fatalf("capabilities = %#v", caps)
	}
}

func TestWorkBuddyMediaModelsAndImage(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "media-a", realmCN, map[string]any{"accessToken": "secret-media-token"})
	provider, _ := newPoolProvider(t, realmCN, dir)
	fake, server := newTaskTestUpstream(t, func(fake *taskTestUpstream, r *http.Request, raw []byte) (int, any) {
		switch r.URL.Path {
		case mediaConfigPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"models": []any{
				map[string]any{"id": "chat-1", "tags": []any{"craft"}},
				map[string]any{"id": "img-1", "name": "Image", "tags": []any{"text-to-image"}},
			}}}
		case mediaImagePath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"created": 123, "data": []any{map[string]any{"url": "https://cdn.example/i.png", "revised_prompt": "better"}},
				"usage": map[string]any{"credit": 5.71},
			}}
		default:
			return http.StatusNotFound, map[string]any{"code": 404, "msg": "not found"}
		}
	})
	provider.mediaBaseURL = server.URL
	models, err := provider.MediaModels(context.Background(), realmCN)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0]["id"] != "img-1" {
		t.Fatalf("models = %#v", models)
	}
	result, err := provider.GenerateImage(context.Background(), providers.MediaRequest{
		Model: "img-1", Prompt: "a cat",
		Body:   map[string]any{"model": "img-1", "prompt": "a cat", "size": "1024x1024"},
		Params: map[string]any{"cost_hint": 5.71},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://cdn.example/i.png" || result.Credit == nil || *result.Credit != 5.71 || result.Account != "media-a" {
		t.Fatalf("result = %#v", result)
	}
	calls := fake.snapshot()
	imageCall := false
	for _, call := range calls {
		if call.Path == mediaImagePath {
			imageCall = true
			if call.Header.Get("Authorization") == "" && call.Header.Get("X-API-Key") == "" {
				t.Fatalf("media request has no auth headers: %v", call.Header)
			}
			if call.Body["prompt"] != "a cat" {
				t.Fatalf("media body = %#v", call.Body)
			}
			if strings.Contains(string(call.Raw), "secret-media-token") {
				t.Fatalf("raw body leaked token: %s", call.Raw)
			}
		}
	}
	if !imageCall {
		t.Fatal("image POST not observed")
	}
}

func TestWorkBuddyMediaVideoLifecycle(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "media-v", realmCN, map[string]any{"accessToken": "token-v"})
	provider, _ := newPoolProvider(t, realmCN, dir)
	fake, server := newTaskTestUpstream(t, func(fake *taskTestUpstream, r *http.Request, raw []byte) (int, any) {
		switch r.URL.Path {
		case mediaVideoSubmitPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"id": "up-9"}}
		case mediaVideoTaskPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"status": "completed", "data": []any{map[string]any{"url": "https://cdn.example/v.mp4"}},
				"usage": map[string]any{"credit": 71.43},
			}}
		case "/artifact":
			return http.StatusOK, map[string]any{"ok": true}
		default:
			return http.StatusNotFound, nil
		}
	})
	provider.mediaBaseURL = server.URL
	submission, err := provider.SubmitVideo(context.Background(), providers.MediaRequest{
		Model: "vid-1", Body: map[string]any{"model": "vid-1", "prompt": "x"},
		Params: map[string]any{"cost_hint": 10},
	})
	if err != nil || submission.UpstreamID != "up-9" || submission.Account != "media-v" {
		t.Fatalf("submission = %#v err=%v", submission, err)
	}
	state, err := provider.PollVideo(context.Background(), submission.Account, "up-9")
	if err != nil || state.Status != "completed" || len(state.Videos) != 1 || state.Credit == nil || *state.Credit != 71.43 {
		t.Fatalf("state = %#v err=%v", state, err)
	}
	dest := dir + "/video.mp4"
	if err := provider.DownloadVideo(context.Background(), submission.Account, server.URL+"/artifact", dest); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dest)
	if err != nil || len(raw) == 0 {
		t.Fatalf("artifact = %q err=%v", raw, err)
	}
	if fake.count(http.MethodPost, mediaVideoSubmitPath) != 1 || fake.count(http.MethodPost, mediaVideoTaskPath) != 1 {
		t.Fatalf("unexpected upstream calls: %#v", fake.snapshot())
	}
}

func TestWorkBuddyMediaErrorsAreSafe(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "media-e", realmCN, map[string]any{"accessToken": "super-secret"})
	provider, _ := newPoolProvider(t, realmCN, dir)
	_, server := newTaskTestUpstream(t, func(fake *taskTestUpstream, r *http.Request, raw []byte) (int, any) {
		return http.StatusTooManyRequests, map[string]any{"code": 429, "msg": "slow down"}
	})
	provider.mediaBaseURL = server.URL
	_, err := provider.GenerateImage(context.Background(), providers.MediaRequest{
		Model: "img", Body: map[string]any{"prompt": "x"}, Params: map[string]any{"cost_hint": 1},
	})
	var mediaErr *providers.MediaError
	if err == nil || !errorIsMedia(err, &mediaErr) || mediaErr.Code != "rate_limit" {
		t.Fatalf("err = %#v", err)
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("error leaked token: %v", err)
	}
}

func errorIsMedia(err error, target **providers.MediaError) bool {
	for err != nil {
		if typed, ok := err.(*providers.MediaError); ok {
			*target = typed
			return true
		}
		break
	}
	return false
}

func TestWorkBuddyReferenceUpload(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "media-ref", realmCN, map[string]any{"accessToken": "token"})
	provider, _ := newPoolProvider(t, realmCN, dir)
	var submitBody map[string]any
	fake, server := newTaskTestUpstream(t, func(fake *taskTestUpstream, r *http.Request, raw []byte) (int, any) {
		switch r.URL.Path {
		case mediaPresignPath:
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"items": []any{
				map[string]any{"upload_url": "http://" + r.Host + "/put", "download_url": "https://cdn.example/uploaded.mp4"},
			}}}
		case "/put":
			return http.StatusOK, map[string]any{"ok": true}
		case mediaVideoSubmitPath:
			_ = json.Unmarshal(raw, &submitBody)
			return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"id": "up-ref"}}
		default:
			return http.StatusNotFound, nil
		}
	})
	provider.mediaBaseURL = server.URL
	payload := base64.StdEncoding.EncodeToString([]byte("video-bytes"))
	_, err := provider.SubmitVideo(context.Background(), providers.MediaRequest{
		Model: "vid-1",
		Body: map[string]any{
			"model": "vid-1", "prompt": "x",
			"reference_videos": []map[string]any{{"video_url": "data:video/mp4;base64," + payload}},
		},
		Params: map[string]any{"cost_hint": 1, "seconds": 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	references, _ := submitBody["reference_videos"].([]any)
	if len(references) != 1 {
		t.Fatalf("submit body = %#v", submitBody)
	}
	item, _ := references[0].(map[string]any)
	if item["video_url"] != "https://cdn.example/uploaded.mp4" {
		t.Fatalf("reference url = %#v", item["video_url"])
	}
	if fake.count(http.MethodPost, mediaPresignPath) != 1 || fake.count(http.MethodPut, "/put") != 1 {
		t.Fatalf("upload calls = %#v", fake.snapshot())
	}
}
