package media

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// Internal task states. Public status mapping is in StatusOut/ErrorCodeFor.
const (
	TaskWaiting    = "waiting"
	TaskSubmitting = "submitting"
	TaskQueued     = "queued"
	TaskRunning    = "running"
	TaskCompleted  = "completed"
	TaskRejected   = "rejected"
	TaskFailed     = "failed"
	TaskLost       = "lost"
	TaskCancelled  = "cancelled"
)

// Task is one persistent video task row. Prompt text is never stored; only a
// head and length survive for diagnostics.
type Task struct {
	ID                    string   `json:"id"`
	Status                string   `json:"status"`
	Model                 string   `json:"model"`
	Realm                 string   `json:"realm"`
	Account               string   `json:"account,omitempty"`
	Key                   string   `json:"key,omitempty"`
	UpstreamID            string   `json:"upstream_id,omitempty"`
	Seconds               any      `json:"seconds,omitempty"`
	Resolution            any      `json:"resolution,omitempty"`
	AspectRatio           any      `json:"aspect_ratio,omitempty"`
	EnableAudio           any      `json:"enable_audio,omitempty"`
	ImageToVideo          bool     `json:"image_to_video"`
	HasReferenceMaterials bool     `json:"has_reference_materials"`
	AllowedAccounts       []string `json:"-"`
	PromptHead            string   `json:"prompt_head,omitempty"`
	PromptLen             int      `json:"prompt_len,omitempty"`
	CreatedAt             int64    `json:"created_at"`
	StartedAt             *int64   `json:"started_at,omitempty"`
	CompletedAt           *int64   `json:"completed_at,omitempty"`
	Credit                *float64 `json:"credit,omitempty"`
	Error                 string   `json:"error,omitempty"`
	Videos                []string `json:"videos,omitempty"`
	Warnings              []string `json:"warnings,omitempty"`
}

// TaskStoreOptions configures the persistent video task table.
type TaskStoreOptions struct {
	Dir          string
	Workers      int
	Concurrency  int
	MaxPending   int
	MaxTasks     int
	MaxDiskMB    int
	MaxWaitingMB int
	TTLDays      int
	PollInterval time.Duration
	PollTimeout  time.Duration
	Now          func() time.Time
	Logger       func(string)
	Autostart    bool
	OnFinish     func(Task)
	StatePath    string
}

const (
	DefaultWorkers      = 3
	DefaultConcurrency  = 3
	DefaultMaxPending   = 20
	DefaultMaxTasks     = 200
	DefaultMaxDiskMB    = 2048
	DefaultTTLDays      = 7
	DefaultPollInterval = 5 * time.Second
	DefaultPollTimeout  = 600 * time.Second
	DefaultSignTTL      = time.Hour
)

// TaskStore owns the task table, submit/poll scheduling, artifacts, retention,
// and signed download tokens. One mutex protects all state transitions.
type TaskStore struct {
	mu              sync.Mutex
	wg              sync.WaitGroup
	dir             string
	statePath       string
	workers         int
	concurrency     int
	maxPending      int
	maxTasks        int
	maxDiskBytes    int64
	maxWaitingBytes int64
	waitingBytes    int64
	ttl             time.Duration
	pollInterval    time.Duration
	pollTimeout     time.Duration
	now             func() time.Time
	logger          func(string)

	tasks   map[string]*Task
	order   []string
	waiting map[string]waitingTask // body-bearing waiting rows, memory only
	started map[string]bool

	submitter    func(context.Context, Task) (providers.MediaVideoSubmit, error)
	poller       func(context.Context, string, string) (providers.MediaVideoState, error)
	download     func(context.Context, string, string, string) error
	taskPoller   func(context.Context, Task) (providers.MediaVideoState, error)
	taskDownload func(context.Context, Task, string, string) error
	onFinish     func(Task)

	stopCh  chan struct{}
	stopped bool
	sema    chan struct{}
	secret  []byte
}

// NewTaskStore creates the store and loads tasks.json. Active tasks from a
// previous process are marked lost; never-submitted tasks failed (retryable).
func NewTaskStore(opts TaskStoreOptions) *TaskStore {
	if strings.TrimSpace(opts.Dir) == "" {
		opts.Dir = "media"
	}
	if opts.Workers <= 0 {
		opts.Workers = DefaultWorkers
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.MaxPending <= 0 {
		opts.MaxPending = DefaultMaxPending
	}
	if opts.MaxTasks <= 0 {
		opts.MaxTasks = DefaultMaxTasks
	}
	if opts.MaxDiskMB <= 0 {
		opts.MaxDiskMB = DefaultMaxDiskMB
	}
	if opts.MaxWaitingMB <= 0 {
		opts.MaxWaitingMB = 64
	}
	if opts.TTLDays <= 0 {
		opts.TTLDays = DefaultTTLDays
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.PollTimeout <= 0 {
		opts.PollTimeout = DefaultPollTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	statePath := opts.StatePath
	if statePath == "" {
		statePath = filepath.Join(opts.Dir, "tasks.json")
	}
	store := &TaskStore{
		dir:             opts.Dir,
		statePath:       statePath,
		workers:         opts.Workers,
		concurrency:     opts.Concurrency,
		maxPending:      opts.MaxPending,
		maxTasks:        opts.MaxTasks,
		maxDiskBytes:    int64(opts.MaxDiskMB) * 1024 * 1024,
		maxWaitingBytes: int64(opts.MaxWaitingMB) * 1024 * 1024,
		ttl:             time.Duration(opts.TTLDays) * 24 * time.Hour,
		pollInterval:    opts.PollInterval,
		pollTimeout:     opts.PollTimeout,
		now:             opts.Now,
		logger:          opts.Logger,
		tasks:           map[string]*Task{},
		waiting:         map[string]waitingTask{},
		started:         map[string]bool{},
		onFinish:        opts.OnFinish,
		stopCh:          make(chan struct{}),
		sema:            make(chan struct{}, opts.Workers),
		secret:          randomSecret(),
	}
	store.load()
	return store
}

// SetSubmitter installs the video submit callback. The store calls it when a
// waiting task reaches the front of the concurrency budget.
func (s *TaskStore) SetSubmitter(fn func(context.Context, Task) (providers.MediaVideoSubmit, error)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.submitter = fn
	s.mu.Unlock()
}

// SetPoller installs the poll/download callbacks used by runTask.
func (s *TaskStore) SetPoller(
	poll func(context.Context, string, string) (providers.MediaVideoState, error),
	download func(context.Context, string, string, string) error,
) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.poller = poll
	s.download = download
	s.mu.Unlock()
}

// SetTaskPoller installs realm-aware callbacks. The task row carries the realm
// needed to find the provider that submitted it.
func (s *TaskStore) SetTaskPoller(
	poll func(context.Context, Task) (providers.MediaVideoState, error),
	download func(context.Context, Task, string, string) error,
) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.taskPoller = poll
	s.taskDownload = download
	s.mu.Unlock()
}

// SetOnFinish replaces the terminal callback (used by service composition).
func (s *TaskStore) SetOnFinish(fn func(Task)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.onFinish = fn
	s.mu.Unlock()
}

// Concurrency returns the active task cap.
func (s *TaskStore) Concurrency() int {
	if s == nil {
		return 0
	}
	return s.concurrency
}

// MaxPending returns the pre-submit queue cap.
func (s *TaskStore) MaxPending() int { return s.maxPending }

// SetMaxPending updates the pre-submit queue cap.
func (s *TaskStore) SetMaxPending(value int) {
	if s == nil || value <= 0 {
		return
	}
	s.mu.Lock()
	s.maxPending = value
	s.mu.Unlock()
}

// SetConcurrency updates the active video-task cap. Existing tasks keep
// running; the next pump uses the new value.
func (s *TaskStore) SetConcurrency(value int) {
	if s == nil || value <= 0 {
		return
	}
	s.mu.Lock()
	s.concurrency = value
	s.mu.Unlock()
}

func (s *TaskStore) log(message string) {
	if s == nil {
		return
	}
	if s.logger != nil {
		s.logger(message)
	}
}

// Create records an already-submitted upstream task.
func (s *TaskStore) Create(model string, params map[string]any, accountUID string, body map[string]any, upstreamID, realm, keyID string, warnings ...string) (Task, error) {
	if s == nil {
		return Task{}, errors.New("media: nil task store")
	}
	if strings.TrimSpace(upstreamID) == "" {
		return Task{}, errors.New("media: upstream task id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.newTaskLocked(model, params, accountUID, body, realm, keyID, TaskQueued)
	task.UpstreamID = strings.TrimSpace(upstreamID)
	task.Warnings = append(task.Warnings, warnings...)
	s.tasks[task.ID] = &task
	s.order = append(s.order, task.ID)
	s.evictLocked()
	s.persistLocked()
	s.startPollLocked(task.ID)
	return task, nil
}

// ErrQueueFull is returned when the in-memory waiting-body budget is full.
var ErrQueueFull = errors.New("media: video queue is full")

// CreatePending records a locally queued task. body stays in memory only and
// counts toward MaxWaitingMB so a burst cannot exhaust process memory.
func (s *TaskStore) CreatePending(model string, params map[string]any, body map[string]any, realm, keyID string) (Task, error) {
	if s == nil {
		return Task{}, errors.New("media: nil task store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := map[string]any{}
	size := int64(0)
	if body != nil {
		for key, value := range body {
			copied[key] = value
		}
		if raw, err := json.Marshal(copied); err == nil {
			size = int64(len(raw))
		}
	}
	if s.maxWaitingBytes > 0 && s.waitingBytes+size > s.maxWaitingBytes {
		return Task{}, ErrQueueFull
	}
	task := s.newTaskLocked(model, params, "", body, realm, keyID, TaskWaiting)
	s.tasks[task.ID] = &task
	s.order = append(s.order, task.ID)
	s.waitingBytes += size
	s.waiting[task.ID] = waitingTask{
		Task: Task{
			ID: task.ID, Model: model, Realm: realm, Key: keyID,
			Seconds: task.Seconds, Resolution: task.Resolution,
			AspectRatio: task.AspectRatio, EnableAudio: task.EnableAudio,
			ImageToVideo: task.ImageToVideo, PromptHead: task.PromptHead,
			HasReferenceMaterials: task.HasReferenceMaterials,
			AllowedAccounts:       append([]string(nil), task.AllowedAccounts...),
			PromptLen:             task.PromptLen,
			Warnings:              append([]string(nil), task.Warnings...),
		},
		Body: copied,
		Size: size,
	}
	s.evictLocked()
	s.persistLocked()
	s.startPumpLocked()
	return task, nil
}

// PendingBody returns the in-memory submit body for a waiting task. It is
// available only until the task is handed to the submitter.
func (s *TaskStore) PendingBody(id string) (map[string]any, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, ok := s.waiting[strings.TrimSpace(id)]
	if !ok || pending.Body == nil {
		return nil, false
	}
	copied := make(map[string]any, len(pending.Body))
	for key, value := range pending.Body {
		copied[key] = value
	}
	return copied, true
}

// waitingTask keeps the prompt-bearing submit body in memory only. It is never
// serialized with the task table.
type waitingTask struct {
	Task
	Body map[string]any
	Size int64
}

func (s *TaskStore) newTaskLocked(model string, params map[string]any, accountUID string, body map[string]any, realm, keyID, status string) Task {
	prompt := ""
	if body != nil {
		prompt, _ = body["prompt"].(string)
	}
	now := s.now().Unix()
	task := Task{
		ID: "vid_" + randomHex(8), Status: status, Model: model, Realm: realm,
		Account: accountUID, Key: keyID, CreatedAt: now,
		ImageToVideo:    boolValue(params["image_to_video"]),
		AllowedAccounts: append([]string(nil), stringList(params["allowed_accounts"])...),
		PromptHead:      truncateRunes(prompt, 80), PromptLen: len([]rune(prompt)),
		Warnings: append([]string(nil), stringList(params["warnings"])...),
	}
	if value, ok := params["seconds"]; ok {
		task.Seconds = value
	}
	if value, ok := params["resolution"]; ok {
		task.Resolution = value
	}
	if value, ok := params["aspect_ratio"]; ok {
		task.AspectRatio = value
	}
	if value, ok := params["enable_audio"]; ok {
		task.EnableAudio = value
	}
	if body != nil {
		for _, key := range []string{"reference_images", "reference_videos", "reference_audios"} {
			if value, ok := body[key]; ok && value != nil {
				if list, ok := value.([]map[string]any); ok && len(list) > 0 {
					task.HasReferenceMaterials = true
				} else if list, ok := value.([]any); ok && len(list) > 0 {
					task.HasReferenceMaterials = true
				}
			}
		}
	}
	return task
}

// Get returns a copy of one task.
func (s *TaskStore) Get(id string) (Task, bool) {
	if s == nil {
		return Task{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.tasks[strings.TrimSpace(id)]
	if task == nil {
		return Task{}, false
	}
	return cloneTask(*task), true
}

// List returns tasks newest first.
func (s *TaskStore) List(limit int) []Task {
	if s == nil {
		return nil
	}
	if limit <= 0 {
		limit = 50
	}
	s.mu.Lock()
	if s.maxTasks > 0 && limit > s.maxTasks {
		limit = s.maxTasks
	}
	if limit > 1000 {
		limit = 1000
	}
	defer s.mu.Unlock()
	out := make([]Task, 0, limit)
	for index := len(s.order) - 1; index >= 0 && len(out) < limit; index-- {
		if task := s.tasks[s.order[index]]; task != nil {
			out = append(out, cloneTask(*task))
		}
	}
	return out
}

// ActiveCount is the number of tasks already submitted upstream.
func (s *TaskStore) ActiveCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeCountLocked()
}

func (s *TaskStore) activeCountLocked() int {
	count := 0
	for _, task := range s.tasks {
		if task.Status == TaskQueued || task.Status == TaskRunning {
			count++
		}
	}
	return count
}

func (s *TaskStore) submittingCountLocked() int {
	count := 0
	for _, task := range s.tasks {
		if task.Status == TaskSubmitting {
			count++
		}
	}
	return count
}

// PendingCount is the number of queued/running tasks. It deliberately matches
// v1's queue-full gate, which counted tasks already submitted.
func (s *TaskStore) PendingCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeCountLocked() + s.waitingCountLocked()
}

// InFlightCount counts submitted and currently-submitting tasks. It is the
// direct-submit concurrency gate (waiting tasks do not hold an upstream slot).
func (s *TaskStore) InFlightCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeCountLocked() + s.submittingCountLocked()
}

// WaitingCount is the number of locally queued, not-yet-submitted tasks.
func (s *TaskStore) WaitingCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waitingCountLocked()
}

func (s *TaskStore) waitingCountLocked() int {
	count := 0
	for _, task := range s.tasks {
		if task.Status == TaskWaiting {
			count++
		}
	}
	return count
}

// BusyAccounts returns accounts with an active video task.
func (s *TaskStore) BusyAccounts() map[string]bool {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	for _, task := range s.tasks {
		if (task.Status == TaskQueued || task.Status == TaskRunning) && task.Account != "" {
			out[task.Account] = true
		}
	}
	return out
}

// QueuePosition returns 1-based position among waiting tasks, or 0.
func (s *TaskStore) QueuePosition(id string) int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if task := s.tasks[id]; task == nil || task.Status != TaskWaiting {
		return 0
	}
	position := 0
	for _, taskID := range s.order {
		if task := s.tasks[taskID]; task != nil && task.Status == TaskWaiting {
			position++
			if taskID == id {
				return position
			}
		}
	}
	return 0
}

// Stats returns the v1 panel shape.
func (s *TaskStore) Stats() map[string]any {
	if s == nil {
		return map[string]any{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byStatus := map[string]int{}
	for _, task := range s.tasks {
		byStatus[task.Status]++
	}
	return map[string]any{
		"total": len(s.tasks), "pending": s.activeCountLocked(),
		"by_status": byStatus, "workers": s.workers,
		"concurrency": s.concurrency, "active": s.activeCountLocked(),
		"submitting": s.submittingCountLocked(),
		"waiting":    s.waitingCountLocked(), "queue_depth": s.waitingCountLocked(),
		"busy_accounts": len(s.busyAccountsLocked()),
		"max_pending":   s.maxPending, "max_tasks": s.maxTasks,
	}
}

func (s *TaskStore) busyAccountsLocked() map[string]bool {
	out := map[string]bool{}
	for _, task := range s.tasks {
		if (task.Status == TaskQueued || task.Status == TaskRunning) && task.Account != "" {
			out[task.Account] = true
		}
	}
	return out
}

// Cancel stops a waiting or queued task. Running/submitting cannot be cancelled
// because the upstream may already have billed it.
func (s *TaskStore) Cancel(id string) (bool, error) {
	if s == nil {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.tasks[strings.TrimSpace(id)]
	if task == nil {
		return false, nil
	}
	switch task.Status {
	case TaskWaiting:
		task.Status = TaskCancelled
		task.Error = "cancelled while queued (未提交到上游，未扣费)"
		now := s.now().Unix()
		task.CompletedAt = &now
		if pending, ok := s.waiting[task.ID]; ok {
			s.waitingBytes -= pending.Size
			delete(s.waiting, task.ID)
		}
		s.persistLocked()
		return true, nil
	case TaskQueued:
		task.Status = TaskCancelled
		task.Error = "cancelled before polling（上游已受理，仍会跑完并计费）"
		now := s.now().Unix()
		task.CompletedAt = &now
		s.persistLocked()
		return true, nil
	default:
		return false, nil
	}
}

// Wait blocks until the task reaches a terminal state or the timeout expires.
func (s *TaskStore) Wait(id string, timeout time.Duration) (Task, bool) {
	if s == nil {
		return Task{}, false
	}
	deadline := s.now().Add(timeout)
	for {
		task, ok := s.Get(id)
		if !ok {
			return Task{}, false
		}
		switch task.Status {
		case TaskWaiting, TaskSubmitting, TaskQueued, TaskRunning:
		default:
			return task, true
		}
		if !s.now().Before(deadline) {
			return task, true
		}
		time.Sleep(minDuration(50*time.Millisecond, time.Until(deadline)))
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if b < a {
		if b < 0 {
			return 0
		}
		return b
	}
	return a
}

// Stop ends polling goroutines. In-flight tasks keep their state.
func (s *TaskStore) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.stopped {
		s.stopped = true
		close(s.stopCh)
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *TaskStore) startPollLocked(id string) {
	if s.stopped || s.started[id] {
		return
	}
	s.started[id] = true
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runTask(id)
	}()
}

func (s *TaskStore) startPumpLocked() {
	if s.stopped {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.pump()
	}()
}

// startPump is the unlocked variant used after a state transition has
// released the mutex. It rechecks stopped under the same lock Stop takes, so
// wg.Add can never race wg.Wait.
func (s *TaskStore) startPump() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startPumpLocked()
}

func (s *TaskStore) pump() {
	s.mu.Lock()
	if s.stopped || s.submitter == nil {
		s.mu.Unlock()
		return
	}
	if s.activeCountLocked()+s.submittingCountLocked() >= s.concurrency {
		s.mu.Unlock()
		return
	}
	candidate := ""
	for _, id := range s.order {
		if task := s.tasks[id]; task != nil && task.Status == TaskWaiting {
			candidate = id
			break
		}
	}
	if candidate == "" {
		s.mu.Unlock()
		return
	}
	task := s.tasks[candidate]
	task.Status = TaskSubmitting
	body := s.waiting[candidate]
	s.mu.Unlock()

	submitter := s.submitter
	if submitter == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	outcome, err := submitter(ctx, body.Task)
	cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.tasks[candidate]
	if current == nil || current.Status != TaskSubmitting {
		return
	}
	if pending, ok := s.waiting[candidate]; ok {
		s.waitingBytes -= pending.Size
	}
	delete(s.waiting, candidate)
	if err != nil {
		current.Status = TaskRejected
		current.Error = err.Error()
		now := s.now().Unix()
		current.CompletedAt = &now
		s.persistLocked()
		s.fireFinishLocked(candidate)
		s.startPumpLocked()
		return
	}
	if strings.TrimSpace(outcome.UpstreamID) == "" {
		current.Status = TaskRejected
		current.Error = "上游没有返回任务 id"
		now := s.now().Unix()
		current.CompletedAt = &now
		s.persistLocked()
		s.fireFinishLocked(candidate)
		s.startPumpLocked()
		return
	}
	current.Status = TaskQueued
	current.UpstreamID = outcome.UpstreamID
	if outcome.Account != "" {
		current.Account = outcome.Account
	}
	if outcome.Credit != nil {
		current.Credit = outcome.Credit
	}
	current.Warnings = append(current.Warnings, outcome.Warnings...)
	s.persistLocked()
	s.startPollLocked(candidate)
	s.startPumpLocked()
}

func (s *TaskStore) runTask(id string) {
	// Limit concurrent poll loops to the worker cap.
	select {
	case s.sema <- struct{}{}:
	case <-s.stopCh:
		return
	}
	defer func() { <-s.sema }()

	ctx, cancel := context.WithTimeout(context.Background(), s.pollTimeout)
	defer cancel()
	deadline := s.now().Add(s.pollTimeout)
	for {
		select {
		case <-s.stopCh:
			return
		default:
		}
		task, ok := s.Get(id)
		if !ok || (task.Status != TaskQueued && task.Status != TaskRunning) {
			return
		}
		if !s.now().Before(deadline) {
			s.finish(id, TaskFailed, "轮询超过 "+s.pollTimeout.String()+" 仍未完成", nil)
			return
		}
		if task.Status == TaskQueued {
			s.update(id, func(task *Task) {
				task.Status = TaskRunning
				now := s.now().Unix()
				task.StartedAt = &now
			})
		}
		s.mu.Lock()
		poller := s.poller
		taskPoller := s.taskPoller
		s.mu.Unlock()
		var state providers.MediaVideoState
		var err error
		if taskPoller != nil {
			state, err = taskPoller(ctx, task)
		} else if poller != nil {
			state, err = poller(ctx, task.Account, task.UpstreamID)
		} else {
			s.finish(id, TaskFailed, "没有注入任务轮询器", nil)
			return
		}
		if err != nil {
			status := TaskFailed
			var mediaErr *providers.MediaError
			if errors.As(err, &mediaErr) && mediaErr != nil && mediaErr.Code == "task_lost" {
				status = TaskLost
			}
			s.finish(id, status, err.Error(), nil)
			return
		}
		switch strings.ToLower(strings.TrimSpace(state.Status)) {
		case "completed":
			if len(state.Videos) == 0 {
				// Upstream sometimes reports completed before the artifact
				// list arrives; keep waiting until the overall timeout.
				break
			}
			paths := make([]string, 0, len(state.Videos))
			for index, url := range state.Videos {
				dest := filepath.Join(s.dir, fmt.Sprintf("video-%s-%d.mp4", safeToken(id), index))
				s.mu.Lock()
				download := s.download
				taskDownload := s.taskDownload
				s.mu.Unlock()
				var downloadErr error
				switch {
				case taskDownload != nil:
					downloadErr = taskDownload(ctx, task, url, dest)
				case download != nil:
					downloadErr = download(ctx, task.Account, url, dest)
				default:
					downloadErr = errors.New("没有注入下载器")
				}
				if downloadErr != nil {
					s.finish(id, TaskFailed, "下载产物失败："+downloadErr.Error(), state.Videos)
					return
				}
				paths = append(paths, dest)
			}
			s.finishWithVideos(id, TaskCompleted, "", state.Credit, paths)
			return
		case "failed", "cancelled":
			message := state.Error
			if message == "" {
				message = "上游任务失败"
			}
			s.finish(id, TaskFailed, message, state.Videos)
			return
		default:
			// queued/running/unknown: wait for the next poll.
		}
		wait := s.pollInterval
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-s.stopCh:
				timer.Stop()
				return
			case <-ctx.Done():
				timer.Stop()
				return
			}
		}
	}
}

func (s *TaskStore) update(id string, mutate func(*Task)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if task := s.tasks[id]; task != nil {
		mutate(task)
		s.persistLocked()
	}
}

func (s *TaskStore) finish(id, status, message string, videos []string) {
	s.finishWithVideos(id, status, message, nil, videos)
}

func (s *TaskStore) finishWithVideos(id, status, message string, credit *float64, videos []string) {
	s.mu.Lock()
	task := s.tasks[id]
	if task == nil {
		s.mu.Unlock()
		return
	}
	if task.Status == TaskCancelled && status != TaskCancelled {
		s.mu.Unlock()
		return
	}
	task.Status = status
	task.Error = message
	now := s.now().Unix()
	task.CompletedAt = &now
	if credit != nil {
		task.Credit = credit
	}
	if videos != nil {
		task.Videos = append([]string(nil), videos...)
	}
	s.persistLocked()
	s.pruneLocked()
	s.fireFinishLocked(id)
	s.mu.Unlock()
	s.startPump()
}

func (s *TaskStore) fireFinishLocked(id string) {
	callback := s.onFinish
	if callback == nil || s.stopped {
		return
	}
	task := s.tasks[id]
	if task == nil {
		return
	}
	copied := cloneTask(*task)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		callback(copied)
	}()
}

func (s *TaskStore) evictLocked() {
	if len(s.order) <= s.maxTasks {
		return
	}
	kept := make([]string, 0, len(s.order))
	for _, id := range s.order {
		task := s.tasks[id]
		if task == nil {
			continue
		}
		if len(kept)+len(s.order)-len(kept) <= s.maxTasks {
			// no-op marker; real elision is below
		}
		kept = append(kept, id)
	}
	// Remove terminal tasks from oldest until under the cap.
	for index := 0; index < len(kept) && len(s.tasks) > s.maxTasks; index++ {
		id := kept[index]
		task := s.tasks[id]
		if task == nil {
			continue
		}
		switch task.Status {
		case TaskCompleted, TaskFailed, TaskRejected, TaskLost, TaskCancelled:
			delete(s.tasks, id)
			delete(s.waiting, id)
		}
	}
	s.order = s.order[:0]
	for id := range s.tasks {
		s.order = append(s.order, id)
	}
	// Rebuild in original creation order as much as possible.
	sort.SliceStable(s.order, func(i, j int) bool {
		left, right := s.tasks[s.order[i]], s.tasks[s.order[j]]
		if left == nil || right == nil {
			return s.order[i] < s.order[j]
		}
		return left.CreatedAt < right.CreatedAt
	})
}

func (s *TaskStore) pruneLocked() {
	now := s.now()
	// Artifact TTL and disk cap; only completed rows with files.
	type artifact struct {
		id   string
		at   int64
		size int64
	}
	var artifacts []artifact
	total := int64(0)
	for _, task := range s.tasks {
		if len(task.Videos) == 0 {
			continue
		}
		size := int64(0)
		for _, path := range task.Videos {
			if info, err := os.Stat(path); err == nil {
				size += info.Size()
			}
		}
		at := int64(0)
		if task.CompletedAt != nil {
			at = *task.CompletedAt
		}
		artifacts = append(artifacts, artifact{id: task.ID, at: at, size: size})
		total += size
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].at < artifacts[j].at })
	for _, item := range artifacts {
		task := s.tasks[item.id]
		if task == nil || len(task.Videos) == 0 {
			continue
		}
		expired := item.at > 0 && now.Sub(time.Unix(item.at, 0)) > s.ttl
		oversized := total > s.maxDiskBytes
		if !expired && !oversized {
			continue
		}
		for _, path := range task.Videos {
			_ = os.Remove(path)
		}
		task.Videos = nil
		total -= item.size
	}
	// Drop terminal rows older than TTL as well.
	for id, task := range s.tasks {
		if task.CompletedAt == nil {
			continue
		}
		if now.Sub(time.Unix(*task.CompletedAt, 0)) <= s.ttl {
			continue
		}
		switch task.Status {
		case TaskCompleted, TaskFailed, TaskRejected, TaskLost, TaskCancelled:
			delete(s.tasks, id)
			delete(s.waiting, id)
		}
	}
}

func (s *TaskStore) persistLocked() {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		s.log("media tasks: create directory failed")
		return
	}
	tasks := make([]Task, 0, len(s.order))
	for _, id := range s.order {
		if task := s.tasks[id]; task != nil {
			tasks = append(tasks, cloneTask(*task))
		}
	}
	payload := map[string]any{"saved_at": s.now().Unix(), "tasks": tasks}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	if err := atomicWriteFile(s.statePath, append(raw, '\n'), 0o600); err != nil {
		s.log("media tasks: persist failed")
	}
}

func (s *TaskStore) load() {
	for _, path := range []string{s.statePath, filepath.Join(s.dir, "tasks.json")} {
		info, statErr := os.Stat(path)
		if statErr != nil || info.IsDir() {
			continue
		}
		if info.Size() > 32<<20 {
			s.log("media tasks: state file too large, ignoring")
			continue
		}
		file, openErr := os.Open(path)
		if openErr != nil {
			continue
		}
		decoder := json.NewDecoder(io.LimitReader(file, 32<<20))
		decoder.UseNumber()
		var payload struct {
			Tasks []Task `json:"tasks"`
		}
		decodeErr := decoder.Decode(&payload)
		_ = file.Close()
		if decodeErr != nil {
			continue
		}
		limit := s.maxTasks
		if limit <= 0 {
			limit = DefaultMaxTasks
		}
		for index, task := range payload.Tasks {
			if index >= limit {
				break
			}
			if strings.TrimSpace(task.ID) == "" {
				continue
			}
			switch task.Status {
			case TaskQueued, TaskRunning:
				task.Status = TaskLost
				task.Error = "网关重启时任务仍在进行，上游可能已扣费"
				now := s.now().Unix()
				task.CompletedAt = &now
			case TaskWaiting, TaskSubmitting:
				task.Status = TaskFailed
				task.Error = "排队期间网关重启（未提交到上游，未扣费），请重新提交"
				now := s.now().Unix()
				task.CompletedAt = &now
			}
			copied := task
			s.tasks[task.ID] = &copied
			s.order = append(s.order, task.ID)
		}
		break
	}
}

func cloneTask(task Task) Task {
	copy := task
	if task.Videos != nil {
		copy.Videos = append([]string(nil), task.Videos...)
	}
	if task.Warnings != nil {
		copy.Warnings = append([]string(nil), task.Warnings...)
	}
	return copy
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

func randomHex(bytes int) string {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func randomSecret() []byte {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return buf
}

// StatusOut maps the internal five-plus states to the OpenAI video status
// vocabulary. Unknown states are reported in_progress, never failed.
func StatusOut(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case TaskQueued, TaskWaiting, TaskSubmitting:
		return "queued"
	case TaskRunning:
		return "in_progress"
	case TaskCompleted:
		return "completed"
	case TaskRejected, TaskFailed, TaskLost, TaskCancelled:
		return "failed"
	default:
		return "in_progress"
	}
}

// ErrorCodeFor maps internal failures to the public error.code vocabulary.
func ErrorCodeFor(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case TaskRejected:
		return "invalid_request"
	case TaskLost:
		return "task_lost"
	case TaskCancelled:
		return "cancelled"
	case TaskFailed:
		return "upstream_error"
	default:
		return ""
	}
}
