package admin

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"opencode2api/internal/config"
)

// sessionStoreVersion is the layout version of the sessions sidecar file.
const sessionStoreVersion = 1

// storedSession is one session as written to disk. The map key is the token
// digest, so the sidecar never contains a usable cookie value.
type storedSession struct {
	Username    string    `json:"username"`
	AuthVersion string    `json:"auth_version"`
	CSRF        string    `json:"csrf_token"`
	Expires     time.Time `json:"expires_at"`
}

type sessionStore struct {
	Version  int                      `json:"version"`
	Sessions map[string]storedSession `json:"sessions"`
}

// loadSessions restores the sessions saved by an earlier process. Entries that
// expired, or that belong to a different username or password hash, are dropped.
func (a *Server) loadSessions() {
	if a.sessionsPath == "" {
		return
	}
	data, err := os.ReadFile(a.sessionsPath)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		a.logger.Warn("admin sessions not restored", "component", "auth", "event", "sessions_restore_failed", "path", a.sessionsPath, "error", err)
		return
	}
	var store sessionStore
	if err := json.Unmarshal(data, &store); err != nil {
		a.logger.Warn("admin sessions ignored", "component", "auth", "event", "sessions_restore_invalid", "path", a.sessionsPath, "error", err)
		return
	}
	cfg := a.manager.Config()
	authVersion := config.Fingerprint(cfg.WebUI.PasswordHash)
	now := time.Now()
	restored := 0
	a.mu.Lock()
	for digest, stored := range store.Sessions {
		if restored+len(a.sessions) >= 2048 {
			break
		}
		if stored.CSRF == "" || now.After(stored.Expires) || stored.Username != cfg.WebUI.Username || stored.AuthVersion != authVersion {
			continue
		}
		a.sessions[digest] = adminSession{Username: stored.Username, AuthVersion: stored.AuthVersion, CSRF: stored.CSRF, Expires: stored.Expires}
		restored++
	}
	a.mu.Unlock()
	if restored > 0 {
		a.logger.Info("admin sessions restored", "component", "auth", "event", "sessions_restored", "count", restored)
	}
}

// persistSessionsLocked writes the live sessions to the sidecar so a restart
// keeps them; a.mu must be held. Failures are logged and otherwise ignored:
// losing the sidecar only costs the operator one login.
func (a *Server) persistSessionsLocked() {
	if a.sessionsPath == "" {
		return
	}
	store := sessionStore{Version: sessionStoreVersion, Sessions: make(map[string]storedSession, len(a.sessions))}
	for digest, session := range a.sessions {
		store.Sessions[digest] = storedSession{Username: session.Username, AuthVersion: session.AuthVersion, CSRF: session.CSRF, Expires: session.Expires}
	}
	data, err := json.Marshal(store)
	if err != nil {
		a.logger.Warn("admin sessions not saved", "component", "auth", "event", "sessions_save_failed", "path", a.sessionsPath, "error", err)
		return
	}
	temp, err := os.CreateTemp(filepath.Dir(a.sessionsPath), ".sessions-*.tmp")
	if err != nil {
		a.logger.Warn("admin sessions not saved", "component", "auth", "event", "sessions_save_failed", "path", a.sessionsPath, "error", err)
		return
	}
	tempPath := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}
	if _, err := temp.Write(data); err != nil {
		cleanup()
		a.logger.Warn("admin sessions not saved", "component", "auth", "event", "sessions_save_failed", "path", a.sessionsPath, "error", err)
		return
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		a.logger.Warn("admin sessions not saved", "component", "auth", "event", "sessions_save_failed", "path", a.sessionsPath, "error", err)
		return
	}
	if err := os.Rename(tempPath, a.sessionsPath); err != nil {
		_ = os.Remove(tempPath)
		a.logger.Warn("admin sessions not saved", "component", "auth", "event", "sessions_save_failed", "path", a.sessionsPath, "error", err)
	}
}
