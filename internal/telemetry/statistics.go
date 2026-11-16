package telemetry

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Statistics keeps durable per-day token totals for the Statistic tab. Metrics
// only remembers the last hour and the current process; these totals survive a
// restart in statistics.json beside the configuration file.
const (
	statisticsLayout        = "2006-01-02"
	statisticsFlushInterval = 15 * time.Second
	statisticsDayWindow     = 366
	// ponytail: session IDs are high-cardinality, so the set is capped and the
	// oldest day is evicted. Raise the cap if useful-session counts matter
	// beyond a few thousand conversations.
	statisticsMaxSessions = 5000
)

// DayStat is the token usage and routed request count of one local day.
type DayStat struct {
	Tokens   TokenCounts `json:"tokens"`
	Requests uint64      `json:"requests"`
}

// ModelStat is the lifetime usage attributed to one model.
type ModelStat struct {
	TokenCounts
	Requests uint64 `json:"requests"`
}

// SessionStat tracks one canonical session over the days it was seen.
type SessionStat struct {
	TokenCounts
	FirstDay string `json:"first_day"`
	LastDay  string `json:"last_day"`
	Days     uint64 `json:"days"`
}

// StatisticsFile is the on-disk shape.
type StatisticsFile struct {
	Version   int                     `json:"version"`
	UpdatedAt time.Time               `json:"updated_at"`
	Days      map[string]*DayStat     `json:"days"`
	Models    map[string]*ModelStat   `json:"models"`
	Sessions  map[string]*SessionStat `json:"sessions"`
}

// DaySeries is one active day of the Statistic snapshot.
type DaySeries struct {
	Date     string      `json:"date"`
	Tokens   TokenCounts `json:"tokens"`
	Requests uint64      `json:"requests"`
}

// ModelSeries is the lifetime usage of one model in the snapshot.
type ModelSeries struct {
	Model    string `json:"model"`
	Requests uint64 `json:"requests"`
	TokenCounts
}

// StatisticsSnapshot is the long-lived view the Statistic tab renders.
type StatisticsSnapshot struct {
	UpdatedAt      time.Time     `json:"updated_at"`
	Lifetime       TokenCounts   `json:"lifetime"`
	Requests       uint64        `json:"requests"`
	PeakTokens     uint64        `json:"peak_tokens"`
	PeakDay        string        `json:"peak_day"`
	UsefulSessions int           `json:"useful_sessions"`
	CurrentStreak  int           `json:"current_streak"`
	LongestStreak  int           `json:"longest_streak"`
	Days           []DaySeries   `json:"days"`
	Models         []ModelSeries `json:"models"`
}

// Statistics accumulates daily usage and flushes it to path in the background.
type Statistics struct {
	mu        sync.Mutex
	path      string
	now       func() time.Time
	days      map[string]*DayStat
	models    map[string]*ModelStat
	sessions  map[string]*SessionStat
	updatedAt time.Time
	dirty     bool
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// NewStatistics loads the file at path (an unreadable or corrupt file starts
// empty) and starts the background flush loop. Close stops it.
func NewStatistics(path string) *Statistics {
	return newStatistics(path, time.Now)
}

func newStatistics(path string, now func() time.Time) *Statistics {
	s := &Statistics{
		path: path, now: now,
		days:     make(map[string]*DayStat),
		models:   make(map[string]*ModelStat),
		sessions: make(map[string]*SessionStat),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	if path == "" {
		close(s.done)
		return s
	}
	s.load()
	go s.saver()
	return s
}

func (s *Statistics) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("statistics file unreadable", "component", "telemetry", "event", "statistics_load_failed", "path", s.path, "error", err)
		}
		return
	}
	var file StatisticsFile
	if err := json.Unmarshal(data, &file); err != nil {
		slog.Warn("statistics file invalid", "component", "telemetry", "event", "statistics_decode_failed", "path", s.path, "error", err)
		return
	}
	if file.Days != nil {
		s.days = file.Days
	}
	if file.Models != nil {
		s.models = file.Models
	}
	if file.Sessions != nil {
		s.sessions = file.Sessions
	}
	s.updatedAt = file.UpdatedAt
}

// Record adds one routed request's usage to today's totals. A zero total still
// counts the request, but only a session with reported tokens is useful.
func (s *Statistics) Record(model, session string, tokens TokenCounts) {
	if s == nil {
		return
	}
	now := s.now()
	date := now.Format(statisticsLayout)
	s.mu.Lock()
	defer s.mu.Unlock()
	day := s.days[date]
	if day == nil {
		day = &DayStat{}
		s.days[date] = day
	}
	addTokenCounts(&day.Tokens, tokens)
	day.Requests++
	if model != "" {
		entry := s.models[model]
		if entry == nil {
			entry = &ModelStat{}
			s.models[model] = entry
		}
		addTokenCounts(&entry.TokenCounts, tokens)
		entry.Requests++
	}
	if session != "" && tokens.Total > 0 {
		entry := s.sessions[session]
		if entry == nil {
			if len(s.sessions) >= statisticsMaxSessions {
				s.evictSessionLocked()
			}
			entry = &SessionStat{FirstDay: date}
			s.sessions[session] = entry
		}
		addTokenCounts(&entry.TokenCounts, tokens)
		if entry.LastDay != date {
			entry.Days++
			entry.LastDay = date
		}
	}
	s.updatedAt = now
	s.dirty = true
}

// evictSessionLocked removes the session seen on the oldest day. The caller
// holds s.mu.
func (s *Statistics) evictSessionLocked() {
	victim, oldest := "", ""
	for key, entry := range s.sessions {
		if victim == "" || entry.LastDay < oldest {
			victim, oldest = key, entry.LastDay
		}
	}
	delete(s.sessions, victim)
}

// Snapshot copies the durable totals. Streaks count days with reported tokens
// and the peak is the busiest day.
func (s *Statistics) Snapshot() StatisticsSnapshot {
	if s == nil {
		return StatisticsSnapshot{Days: []DaySeries{}, Models: []ModelSeries{}}
	}
	s.mu.Lock()
	days := make(map[string]DayStat, len(s.days))
	for key, value := range s.days {
		days[key] = *value
	}
	models := make(map[string]ModelStat, len(s.models))
	for key, value := range s.models {
		models[key] = *value
	}
	useful := len(s.sessions)
	updated := s.updatedAt
	s.mu.Unlock()

	today := s.now().Format(statisticsLayout)
	active := make(map[string]bool, len(days))
	lifetime := TokenCounts{}
	var requests, peak uint64
	peakDay := ""
	for date, day := range days {
		addTokenCounts(&lifetime, day.Tokens)
		requests += day.Requests
		if day.Tokens.Total > 0 {
			active[date] = true
			if day.Tokens.Total > peak {
				peak, peakDay = day.Tokens.Total, date
			}
		}
	}
	current, longest := streakLengths(active, today)
	cutoff := s.now().AddDate(0, 0, -(statisticsDayWindow - 1)).Format(statisticsLayout)
	series := make([]DaySeries, 0, len(days))
	for date, day := range days {
		if date >= cutoff {
			series = append(series, DaySeries{Date: date, Tokens: day.Tokens, Requests: day.Requests})
		}
	}
	sort.Slice(series, func(i, j int) bool { return series[i].Date < series[j].Date })
	modelSeries := make([]ModelSeries, 0, len(models))
	for name, entry := range models {
		modelSeries = append(modelSeries, ModelSeries{Model: name, Requests: entry.Requests, TokenCounts: entry.TokenCounts})
	}
	sort.Slice(modelSeries, func(i, j int) bool {
		if modelSeries[i].Total != modelSeries[j].Total {
			return modelSeries[i].Total > modelSeries[j].Total
		}
		return modelSeries[i].Model < modelSeries[j].Model
	})
	return StatisticsSnapshot{
		UpdatedAt: updated, Lifetime: lifetime, Requests: requests,
		PeakTokens: peak, PeakDay: peakDay, UsefulSessions: useful,
		CurrentStreak: current, LongestStreak: longest, Days: series, Models: modelSeries,
	}
}

// streakLengths returns the current streak ending today and the longest run of
// consecutive active days.
func streakLengths(active map[string]bool, today string) (int, int) {
	dates := make([]string, 0, len(active))
	for date := range active {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	longest, run := 0, 0
	var previous time.Time
	for _, date := range dates {
		parsed, err := time.Parse(statisticsLayout, date)
		if err != nil {
			continue
		}
		if !previous.IsZero() && parsed.Sub(previous) == 24*time.Hour {
			run++
		} else {
			run = 1
		}
		if run > longest {
			longest = run
		}
		previous = parsed
	}
	current, cursor := 0, today
	for {
		if !active[cursor] {
			break
		}
		current++
		parsed, err := time.Parse(statisticsLayout, cursor)
		if err != nil {
			break
		}
		cursor = parsed.AddDate(0, 0, -1).Format(statisticsLayout)
	}
	return current, longest
}

// Close stops the flush loop after writing the last change.
func (s *Statistics) Close() {
	if s == nil || s.path == "" {
		return
	}
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
	})
}

func (s *Statistics) saver() {
	defer close(s.done)
	ticker := time.NewTicker(statisticsFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.flush()
		case <-s.stop:
			s.flush()
			return
		}
	}
}

// flush writes a dirty snapshot atomically: temp file in the same directory,
// then rename. Marshalling happens under the lock so concurrent Record calls
// cannot mutate the maps mid-encode; the file write does not.
func (s *Statistics) flush() {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	payload := StatisticsFile{
		Version: 1, UpdatedAt: s.updatedAt,
		Days: s.days, Models: s.models, Sessions: s.sessions,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err == nil {
		s.dirty = false
	}
	s.mu.Unlock()
	if err != nil {
		slog.Warn("statistics encode failed", "component", "telemetry", "event", "statistics_encode_failed", "error", err)
		return
	}
	temp, err := os.CreateTemp(filepath.Dir(s.path), ".statistics-*.tmp")
	if err != nil {
		slog.Warn("statistics temp file failed", "component", "telemetry", "event", "statistics_write_failed", "error", err)
		return
	}
	name := temp.Name()
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		os.Remove(name)
		slog.Warn("statistics write failed", "component", "telemetry", "event", "statistics_write_failed", "error", err)
		return
	}
	if err := temp.Close(); err != nil {
		os.Remove(name)
		slog.Warn("statistics write failed", "component", "telemetry", "event", "statistics_write_failed", "error", err)
		return
	}
	if err := os.Rename(name, s.path); err != nil {
		os.Remove(name)
		slog.Warn("statistics rename failed", "component", "telemetry", "event", "statistics_write_failed", "error", err)
	}
}
