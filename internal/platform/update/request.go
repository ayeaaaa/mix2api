package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrInstallDisabled = errors.New("安装更新服务尚未配置")
	ErrRequestActive   = errors.New("已有更新任务正在运行")
	ErrTargetInvalid   = errors.New("目标版本无效或不高于当前版本")
	ErrRollbackInvalid = errors.New("回退目标版本无效")
	ErrRollbackSame    = errors.New("回退目标与当前版本相同")
	ErrNoBackup        = errors.New("没有可回退的完整备份")
	ErrRequestWrite    = errors.New("无法提交更新任务，请检查更新服务状态")
)

// RequestManagerOptions configures the panel-side request bridge.
type RequestManagerOptions struct {
	Dir      string
	Executor *Executor
	Now      func() time.Time
	PID      int
	Port     int
	Root     string
	LAN      bool
}

// RequestManager queues validated requests and reads executor state.
type RequestManager struct {
	Dir      string
	Executor *Executor
	Now      func() time.Time
	PID      int
	Port     int
	Root     string
	LAN      bool
}

// NewRequestManager builds the bridge. It performs no filesystem writes.
func NewRequestManager(opts RequestManagerOptions) *RequestManager {
	if strings.TrimSpace(opts.Dir) == "" {
		opts.Dir = envValue("WB_UPDATE_REQUEST_DIR", DefaultRequestDir)
	}
	if opts.Executor == nil {
		opts.Executor = NewExecutor(opts.Dir)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.PID == 0 {
		opts.PID = os.Getpid()
	}
	root := strings.TrimSpace(opts.Root)
	if root == "" {
		if executable, err := os.Executable(); err == nil {
			root = filepath.Dir(executable)
		} else {
			root = "."
		}
	}
	opts.Root = filepath.Clean(root)
	return &RequestManager{
		Dir: opts.Dir, Executor: opts.Executor, Now: opts.Now,
		PID: opts.PID, Port: opts.Port, Root: opts.Root, LAN: opts.LAN,
	}
}

func (m *RequestManager) now() time.Time {
	if m != nil && m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *RequestManager) executor() *Executor {
	if m != nil && m.Executor != nil {
		return m.Executor
	}
	dir := ""
	if m != nil {
		dir = m.Dir
	}
	return NewExecutor(dir)
}

// Enabled reports whether the deployment registered an executor.
func (m *RequestManager) Enabled() bool { return m.executor().Enabled() }

// Kind returns the executor kind.
func (m *RequestManager) Kind() string { return m.executor().Kind() }

// Status returns the current installation state, including the v1 "queued"
// override when a request file exists but status.json is not yet active.
func (m *RequestManager) Status() Installation {
	if m == nil || !m.Enabled() {
		return Installation{State: "idle"}
	}
	executor := m.executor()
	installation, ok := executor.ReadStatus()
	if !ok {
		if fileExists(filepath.Join(executor.dir(), RequestFile)) {
			installation = Installation{State: "queued"}
		} else {
			installation = Installation{State: "idle"}
		}
	}
	if fileExists(filepath.Join(executor.dir(), RequestFile)) && !ActiveStates[installation.State] {
		installation = Installation{State: "queued", Version: installation.Version, UpdatedAt: installation.UpdatedAt}
	}
	return installation
}

// Backups lists the executor inventory. Source is systemd for the privileged
// worker and local for external/portable deployments. No paths are exposed.
func (m *RequestManager) Backups() BackupInventory {
	kind := m.Kind()
	if kind == "" || kind == "none" {
		return BackupInventory{Source: "", Backups: []Backup{}}
	}
	source := "local"
	if kind == "systemd" {
		source = "systemd"
	}
	items, ok := m.executor().ReadBackups()
	if !ok {
		items = []Backup{}
	}
	for index := range items {
		items[index].Source = source
	}
	return BackupInventory{Source: source, Backups: items}
}

// Install validates a stable target and writes one create-only request.
func (m *RequestManager) Install(tag, currentVersion string, port int, lan bool, repo string) (Installation, error) {
	if m == nil || !m.Enabled() {
		return Installation{}, ErrInstallDisabled
	}
	tag = strings.TrimSpace(tag)
	target := NormalizeVersion(tag)
	if !stableVersionPattern.MatchString(target) || !IsNewer(target, currentVersion) {
		return Installation{}, ErrTargetInvalid
	}
	if m.active() {
		return Installation{}, ErrRequestActive
	}
	version := target
	request := m.requestFields(version, port, lan)
	request.Action = "install"
	request.Tag = tag
	if err := m.queue(request); err != nil {
		return Installation{}, err
	}
	return Installation{State: "queued", Version: version, Message: "更新任务已排队"}, nil
}

// Rollback validates a complete retained backup and queues a restore request.
func (m *RequestManager) Rollback(version, currentVersion string, port int, lan bool) (Installation, error) {
	if m == nil || !m.Enabled() {
		return Installation{}, ErrInstallDisabled
	}
	target := NormalizeVersion(version)
	if !IsReleaseVersion(target) {
		return Installation{}, ErrRollbackInvalid
	}
	current := NormalizeVersion(currentVersion)
	if target == current {
		return Installation{}, ErrRollbackSame
	}
	complete := false
	for _, backup := range m.Backups().Backups {
		if backup.Complete && backup.Version == target {
			complete = true
			break
		}
	}
	if !complete {
		return Installation{}, fmt.Errorf("没有可回退到 %s 的完整备份", target)
	}
	if m.active() {
		return Installation{}, ErrRequestActive
	}
	request := m.requestFields(target, port, lan)
	request.Action = "rollback"
	// The v1 compatibility promise: an executor that predates `action` will
	// treat this as "reinstall v<version>" and still reach the same version.
	request.Tag = "v" + target
	if err := m.queue(request); err != nil {
		return Installation{}, err
	}
	return Installation{State: "queued", Version: target, Message: "回退任务已排队"}, nil
}

func (m *RequestManager) active() bool {
	executor := m.executor()
	if fileExists(filepath.Join(executor.dir(), RequestFile)) {
		return true
	}
	installation, ok := executor.ReadStatus()
	return ok && ActiveStates[installation.State]
}

func (m *RequestManager) requestFields(version string, port int, lan bool) Request {
	if port <= 0 {
		if m.Port > 0 {
			port = m.Port
		} else {
			port = 8788
		}
	}
	request := Request{
		Version:     version,
		RequestedAt: m.now().Format("2006-01-02 15:04:05"),
		PID:         m.PID, Port: port, Root: m.Root,
	}
	if profile := strings.TrimSpace(os.Getenv("WB_UPDATE_PROFILE")); profile != "" {
		request.Profile = profile
	}
	return request
}

func (m *RequestManager) queue(request Request) error {
	executor := m.executor()
	dir := executor.dir()
	if !isDir(dir) {
		return ErrRequestWrite
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return ErrRequestWrite
	}
	path := filepath.Join(dir, RequestFile)
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL | secureOpenFlags()
	fd, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return ErrRequestActive
		}
		return ErrRequestWrite
	}
	closeAndRemove := func() {
		_ = fd.Close()
		_ = os.Remove(path)
	}
	if _, err := fd.Write(raw); err != nil {
		closeAndRemove()
		return ErrRequestWrite
	}
	if err := fd.Sync(); err != nil {
		closeAndRemove()
		return ErrRequestWrite
	}
	if err := fd.Close(); err != nil {
		_ = os.Remove(path)
		return ErrRequestWrite
	}
	return nil
}

// KeepBackupsDefault mirrors v1's default retention when settings are absent.
const KeepBackupsDefault = 1

// KeepBackupsMax is the panel upper bound.
const KeepBackupsMax = 3
