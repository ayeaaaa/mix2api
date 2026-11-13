package update

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	DefaultRequestDir  = "/run/mix2api-updater"
	DefaultUnitPath    = "/etc/systemd/system/mix2api-updater.path"
	DefaultEnabledLink = "/etc/systemd/system/multi-user.target.wants/mix2api-updater.path"
	StatusFile         = "status.json"
	RequestFile        = "request.json"
	BackupsFile        = "backups.json"
	ExecutorMarker     = "executor.json"
	PortableMarker     = "portable.json"
)

// ActiveStates are states where a request is still in flight.
var ActiveStates = map[string]bool{
	"queued": true, "downloading": true, "verifying": true, "backing_up": true,
	"stopping": true, "applying": true, "restoring": true, "restarting": true,
	"checking": true, "rolling_back": true,
}

// VisibleStates are states the panel may display.
var VisibleStates = map[string]bool{
	"idle": true, "queued": true, "downloading": true, "verifying": true,
	"backing_up": true, "stopping": true, "applying": true, "restoring": true,
	"restarting": true, "checking": true, "succeeded": true, "restored": true,
	"rolling_back": true, "rolled_back": true, "failed": true, "unavailable": true,
}

// Installation is the panel installation state.
type Installation struct {
	State     string `json:"state"`
	Version   string `json:"version"`
	Message   string `json:"message"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// Request is one queued executor request.
type Request struct {
	Action      string `json:"action"`
	Version     string `json:"version"`
	Tag         string `json:"tag"`
	RequestedAt string `json:"requested_at"`
	PID         int    `json:"pid"`
	Port        int    `json:"port"`
	Root        string `json:"root"`
	Profile     string `json:"profile,omitempty"`
}

// Backup is one inventory row. Paths are deliberately absent.
type Backup struct {
	Version   string `json:"version"`
	CreatedAt string `json:"created_at,omitempty"`
	Files     int    `json:"files"`
	Complete  bool   `json:"complete"`
	Reason    string `json:"reason,omitempty"`
	SizeMB    int    `json:"size_mb"`
	Source    string `json:"source,omitempty"`
}

// BackupInventory is the panel backup list.
type BackupInventory struct {
	Source  string   `json:"source"`
	Backups []Backup `json:"backups"`
	Msg     string   `json:"msg,omitempty"`
}

// Executor detects the installed privileged updater. It only reads files; it
// never runs systemctl or replaces application files.
type Executor struct {
	Dir         string
	UnitPath    string
	EnabledLink string
	Env         func(string) string
	AllowOwner  func(uid uint32) bool
}

// NewExecutor returns an executor bound to dir.
func NewExecutor(dir string) *Executor {
	if strings.TrimSpace(dir) == "" {
		dir = envValue("WB_UPDATE_REQUEST_DIR", DefaultRequestDir)
	}
	return &Executor{Dir: dir, UnitPath: DefaultUnitPath, EnabledLink: DefaultEnabledLink}
}

func envValue(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func (e *Executor) env(name string) string {
	if e != nil && e.Env != nil {
		return e.Env(name)
	}
	return os.Getenv(name)
}

func (e *Executor) dir() string {
	if e != nil && strings.TrimSpace(e.Dir) != "" {
		return e.Dir
	}
	return envValue("WB_UPDATE_REQUEST_DIR", DefaultRequestDir)
}

// Kind reports the registered executor kind: systemd, external, portable, none.
func (e *Executor) Kind() string {
	dir := e.dir()
	enabled := strings.TrimSpace(e.env("WB_UPDATE_INSTALL_ENABLED")) == "1"
	if enabled && isDir(dir) {
		unit := e.UnitPath
		if strings.TrimSpace(unit) == "" {
			unit = DefaultUnitPath
		}
		link := e.EnabledLink
		if strings.TrimSpace(link) == "" {
			link = DefaultEnabledLink
		}
		if fileExists(unit) && fileExists(link) {
			return "systemd"
		}
		if strings.EqualFold(strings.TrimSpace(e.env("WB_UPDATE_EXECUTOR")), "external") || fileExists(filepath.Join(dir, ExecutorMarker)) {
			return "external"
		}
	}
	if strings.TrimSpace(e.env("WB_UPDATE_PORTABLE")) == "1" || fileExists(filepath.Join(dir, PortableMarker)) {
		return "portable"
	}
	return "none"
}

// Enabled reports whether any executor can queue an install.
func (e *Executor) Enabled() bool { return e.Kind() != "none" }

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func (e *Executor) ownerAllowed(uid uint32) bool {
	if e != nil && e.AllowOwner != nil {
		return e.AllowOwner(uid)
	}
	if uid == 0 {
		return true
	}
	if strings.TrimSpace(e.env("WB_UPDATE_ALLOW_CURRENT_OWNER")) == "1" {
		return uid == uint32(os.Geteuid())
	}
	return false
}

func (e *Executor) readChecked(name string, limit int64) ([]byte, bool) {
	path := filepath.Join(e.dir(), name)
	flags := os.O_RDONLY | secureOpenFlags()
	fd, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return nil, false
	}
	defer fd.Close()
	info, err := fd.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit || info.Mode().Perm()&0o022 != 0 {
		return nil, false
	}
	if uid, ok := fileOwner(info); ok && !e.ownerAllowed(uid) {
		return nil, false
	}
	raw, err := ioReadAllLimit(fd, limit+1)
	if err != nil || int64(len(raw)) > limit {
		return nil, false
	}
	return raw, true
}

func ioReadAllLimit(file *os.File, limit int64) ([]byte, error) {
	buffer := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	total := int64(0)
	for {
		read, err := file.Read(chunk)
		if read > 0 {
			total += int64(read)
			if total > limit {
				return nil, os.ErrInvalid
			}
			buffer = append(buffer, chunk[:read]...)
		}
		if err != nil {
			if err == io.EOF {
				return buffer, nil
			}
			return nil, err
		}
	}
}

// ReadStatus returns a validated status document.
func (e *Executor) ReadStatus() (Installation, bool) {
	raw, ok := e.readChecked(StatusFile, 8192)
	if !ok {
		return Installation{}, false
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return Installation{}, false
	}
	state, _ := payload["state"].(string)
	state = strings.TrimSpace(state)
	if !VisibleStates[state] {
		return Installation{}, false
	}
	version, _ := payload["version"].(string)
	message, _ := payload["message"].(string)
	updatedAt, _ := payload["updated_at"].(string)
	return Installation{
		State: state, Version: truncateString(version, 32), Message: truncateString(message, 240),
		UpdatedAt: truncateString(strings.TrimSpace(updatedAt), 32),
	}, true
}

// ReadBackups returns a validated inventory without any filesystem paths.
func (e *Executor) ReadBackups() ([]Backup, bool) {
	raw, ok := e.readChecked(BackupsFile, 65536)
	if !ok {
		return nil, false
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return nil, false
	}
	items, _ := payload["backups"].([]any)
	out := []Backup{}
	for _, rawItem := range items {
		if len(out) >= 8 {
			break
		}
		item, _ := rawItem.(map[string]any)
		if item == nil {
			continue
		}
		version := strings.TrimSpace(stringValue(item["version"]))
		if !IsReleaseVersion(version) || version == "0.0.0" {
			continue
		}
		complete, _ := item["complete"].(bool)
		out = append(out, Backup{
			Version: version, CreatedAt: truncateString(strings.TrimSpace(stringValue(item["created_at"])), 32),
			Files: intValue(item["files"]), Complete: complete,
			Reason: truncateString(strings.TrimSpace(stringValue(item["reason"])), 120),
			SizeMB: intValue(item["size_mb"]),
		})
	}
	return out, true
}

// ReadRequest returns a validated request document.
func (e *Executor) ReadRequest() (Request, bool) {
	raw, ok := e.readChecked(RequestFile, 16384)
	if !ok {
		return Request{}, false
	}
	var request Request
	if json.Unmarshal(raw, &request) != nil {
		return Request{}, false
	}
	if request.Action != "install" && request.Action != "rollback" {
		return Request{}, false
	}
	return request, true
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		number, _ := typed.Int64()
		return int(number)
	default:
		return 0
	}
}
