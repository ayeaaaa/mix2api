package media

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func newTestTaskStore(t *testing.T, mutate func(*TaskStoreOptions)) *TaskStore {
	t.Helper()
	opts := TaskStoreOptions{
		Dir: t.TempDir(), PollInterval: 2 * time.Millisecond, PollTimeout: 500 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&opts)
	}
	store := NewTaskStore(opts)
	t.Cleanup(store.Stop)
	return store
}

func TestVideoTaskLifecycle(t *testing.T) {
	store := newTestTaskStore(t, nil)
	var polls int32
	store.SetPoller(
		func(ctx context.Context, account, upstream string) (providers.MediaVideoState, error) {
			n := atomic.AddInt32(&polls, 1)
			if n < 3 {
				return providers.MediaVideoState{Status: "running"}, nil
			}
			return providers.MediaVideoState{Status: "completed", Videos: []string{"https://cdn.example/v.mp4"}}, nil
		},
		func(ctx context.Context, account, source, dest string) error {
			return os.WriteFile(dest, []byte("mp4"), 0o600)
		},
	)
	task, err := store.Create("vid", map[string]any{"seconds": 5, "resolution": "720P", "aspect_ratio": "16:9", "image_to_video": false}, "acct", map[string]any{"prompt": "a cat"}, "up-1", "cn", "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != TaskQueued || task.PromptHead != "a cat" || task.PromptLen != 5 {
		t.Fatalf("created = %#v", task)
	}
	done, ok := store.Wait(task.ID, 2*time.Second)
	if !ok || done.Status != TaskCompleted || len(done.Videos) != 1 {
		t.Fatalf("done = %#v ok=%v", done, ok)
	}
	if _, err := os.Stat(done.Videos[0]); err != nil {
		t.Fatalf("artifact missing: %v", err)
	}
	if StatusOut(TaskWaiting) != "queued" || StatusOut(TaskSubmitting) != "queued" ||
		StatusOut(TaskRunning) != "in_progress" || StatusOut(TaskCompleted) != "completed" ||
		StatusOut(TaskRejected) != "failed" || StatusOut(TaskCancelled) != "failed" {
		t.Fatal("status mapping differs from v1")
	}
	if ErrorCodeFor(TaskRejected) != "invalid_request" || ErrorCodeFor(TaskLost) != "task_lost" ||
		ErrorCodeFor(TaskCancelled) != "cancelled" || ErrorCodeFor(TaskFailed) != "upstream_error" {
		t.Fatal("error code mapping differs from v1")
	}
}

func TestVideoTaskCancelAndQueuePosition(t *testing.T) {
	store := newTestTaskStore(t, func(opts *TaskStoreOptions) { opts.Concurrency = 1 })
	store.SetPoller(func(ctx context.Context, account, upstream string) (providers.MediaVideoState, error) {
		return providers.MediaVideoState{Status: "running"}, nil
	}, nil)
	first, err := store.Create("vid", map[string]any{"seconds": 5}, "a", nil, "up-1", "cn", "k")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		got, _ := store.Get(first.ID)
		if got.Status == TaskRunning {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	waiting, err := store.CreatePending("vid", map[string]any{"seconds": 5}, map[string]any{"prompt": "secret prompt"}, "cn", "k")
	if err != nil {
		t.Fatal(err)
	}
	if position := store.QueuePosition(waiting.ID); position != 1 {
		t.Fatalf("queue position = %d", position)
	}
	if ok, _ := store.Cancel(waiting.ID); !ok {
		t.Fatal("waiting task cancel failed")
	}
	if got, _ := store.Get(waiting.ID); got.Status != TaskCancelled || ErrorCodeFor(got.Status) != "cancelled" {
		t.Fatalf("cancelled task = %#v", got)
	}
	if ok, _ := store.Cancel(first.ID); ok {
		t.Fatal("running task was cancellable")
	}
}

func TestVideoTaskRetentionAndNoPromptOnDisk(t *testing.T) {
	dir := t.TempDir()
	store := NewTaskStore(TaskStoreOptions{Dir: dir, PollInterval: time.Millisecond, PollTimeout: 100 * time.Millisecond})
	t.Cleanup(store.Stop)
	store.SetPoller(func(ctx context.Context, account, upstream string) (providers.MediaVideoState, error) {
		return providers.MediaVideoState{Status: "running"}, nil
	}, nil)
	_, err := store.CreatePending("vid", map[string]any{"seconds": 5}, map[string]any{"prompt": "TOP-SECRET-PROMPT-THAT-IS-LONGER-THAN-EIGHTY-CHARACTERS-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}, "cn", "k")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "TOP-SECRET-PROMPT-THAT-IS-LONGER-THAN-EIGHTY-CHARACTERS-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx") {
		t.Fatalf("full prompt hit tasks.json: %s", raw)
	}
	if !strings.Contains(string(raw), `"prompt_head": "TOP-SECRET-PROMPT`) || !strings.Contains(string(raw), `"prompt_len": 136`) {
		t.Fatalf("prompt head/len missing: %s", raw)
	}

	// A restart marks waiting tasks failed with the retryable v1 message.
	restarted := NewTaskStore(TaskStoreOptions{Dir: dir, Now: time.Now})
	t.Cleanup(restarted.Stop)
	tasks := restarted.List(10)
	if len(tasks) != 1 || tasks[0].Status != TaskFailed || !strings.Contains(tasks[0].Error, "未提交到上游") {
		t.Fatalf("restart tasks = %#v", tasks)
	}
}

func TestMediaTaskSignedURL(t *testing.T) {
	store := newTestTaskStore(t, nil)
	token := store.Sign("vid_abc", 0, time.Hour)
	if !store.Verify("vid_abc", 0, token) {
		t.Fatal("valid token rejected")
	}
	if store.Verify("vid_abc", 1, token) {
		t.Fatal("token accepted for another index")
	}
	if store.Verify("other", 0, token) {
		t.Fatal("token accepted for another task")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 || len(parts[1]) != 32 {
		t.Fatalf("token shape = %q", token)
	}
	expired := store.Sign("vid_abc", 0, -time.Second)
	if store.Verify("vid_abc", 0, expired) {
		t.Fatal("expired token accepted")
	}
	if store.Verify("vid_abc", 0, "garbage") {
		t.Fatal("garbage token accepted")
	}
}

func TestMediaTaskMaxCountAndDiskPrune(t *testing.T) {
	dir := t.TempDir()
	store := NewTaskStore(TaskStoreOptions{
		Dir: dir, MaxTasks: 2, MaxDiskMB: 1, PollInterval: time.Millisecond,
		PollTimeout: 200 * time.Millisecond,
	})
	t.Cleanup(store.Stop)
	store.SetPoller(func(ctx context.Context, account, upstream string) (providers.MediaVideoState, error) {
		return providers.MediaVideoState{Status: "completed", Videos: []string{"https://cdn.example/v.mp4"}}, nil
	}, func(ctx context.Context, account, source, dest string) error {
		return os.WriteFile(dest, []byte("0123456789"), 0o600)
	})
	for i := 0; i < 4; i++ {
		task, err := store.Create("vid", map[string]any{"seconds": 5}, "a", nil, "up-"+string(rune('a'+i)), "cn", "k")
		if err != nil {
			t.Fatal(err)
		}
		store.Wait(task.ID, time.Second)
	}
	if tasks := store.List(10); len(tasks) > 2 {
		t.Fatalf("max tasks not enforced: %d", len(tasks))
	}
	// Shrink the disk cap and force a completed task; old artifacts are pruned.
	store.mu.Lock()
	store.maxDiskBytes = 1
	store.pruneLocked()
	store.mu.Unlock()
}

func TestMediaWaitingBodyBudgetAndHugeLimit(t *testing.T) {
	store := NewTaskStore(TaskStoreOptions{Dir: t.TempDir(), MaxWaitingMB: 1, MaxTasks: 2})
	t.Cleanup(store.Stop)
	big := strings.Repeat("x", 2*1024*1024)
	if _, err := store.CreatePending("m", map[string]any{"seconds": 5}, map[string]any{"prompt": big}, "cn", "k"); err != ErrQueueFull {
		t.Fatalf("waiting budget err = %v", err)
	}
	if tasks := store.List(1 << 30); len(tasks) > 2 {
		t.Fatalf("huge List allocated/returned %d", len(tasks))
	}
}

func TestWaitingBudgetReleasedOnCancelAndPump(t *testing.T) {
	dir := t.TempDir()
	store := NewTaskStore(TaskStoreOptions{Dir: dir, MaxWaitingMB: 1, PollInterval: time.Millisecond, PollTimeout: 100 * time.Millisecond})
	t.Cleanup(store.Stop)
	body := map[string]any{"prompt": strings.Repeat("a", 600*1024)}
	task, err := store.CreatePending("m", map[string]any{"seconds": 5}, body, "cn", "k")
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	if store.waitingBytes <= 0 {
		store.mu.Unlock()
		t.Fatal("waiting budget was not charged")
	}
	store.mu.Unlock()
	if ok, _ := store.Cancel(task.ID); !ok {
		t.Fatal("cancel failed")
	}
	store.mu.Lock()
	afterCancel := store.waitingBytes
	store.mu.Unlock()
	if afterCancel != 0 {
		t.Fatalf("waiting budget after cancel = %d", afterCancel)
	}
	// Pump releases the budget before handing the body to the submitter.
	store.SetSubmitter(func(context.Context, Task) (providers.MediaVideoSubmit, error) {
		return providers.MediaVideoSubmit{UpstreamID: "up"}, nil
	})
	store.SetPoller(func(context.Context, string, string) (providers.MediaVideoState, error) {
		return providers.MediaVideoState{Status: "running"}, nil
	}, nil)
	if _, err := store.CreatePending("m", map[string]any{"seconds": 5}, body, "cn", "k"); err != nil {
		t.Fatalf("budget not reusable after cancel: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		store.mu.Lock()
		bytes := store.waitingBytes
		store.mu.Unlock()
		if bytes == 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("waiting budget was not released by the submitter pump")
}

func TestInFlightCountIncludesQueuedAndSubmitting(t *testing.T) {
	store := NewTaskStore(TaskStoreOptions{Dir: t.TempDir(), Concurrency: 1})
	t.Cleanup(store.Stop)
	store.mu.Lock()
	store.tasks["q"] = &Task{ID: "q", Status: TaskQueued}
	store.tasks["s"] = &Task{ID: "s", Status: TaskSubmitting}
	store.order = []string{"q", "s"}
	store.mu.Unlock()
	if got := store.InFlightCount(); got != 2 {
		t.Fatalf("InFlightCount = %d, want 2", got)
	}
	if got := store.PendingCount(); got != 1 {
		t.Fatalf("PendingCount = %d, want queued-only active count", got)
	}
}
