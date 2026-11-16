package telemetry

import (
	"path/filepath"
	"testing"
	"time"
)

// TestStatisticsStreaksAndPersistence checks the streak, peak, session and
// round-trip behaviour the Statistic tab relies on.
func TestStatisticsStreaksAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "statistics.json")
	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	statistics := newStatistics(path, now)
	for _, day := range []string{"2026-01-01", "2026-01-02", "2026-01-03", "2026-01-05", "2026-01-06"} {
		parsed, err := time.Parse(statisticsLayout, day)
		if err != nil {
			t.Fatal(err)
		}
		clock = parsed
		total := uint64(parsed.Day() * 10)
		statistics.Record("model-a", "session-"+day, TokenCounts{Input: total, Total: total})
	}
	snapshot := statistics.Snapshot()
	if snapshot.Lifetime.Total != 170 {
		t.Fatalf("lifetime total = %d, want 170", snapshot.Lifetime.Total)
	}
	if snapshot.PeakTokens != 60 || snapshot.PeakDay != "2026-01-06" {
		t.Fatalf("peak = %d on %q, want 60 on 2026-01-06", snapshot.PeakTokens, snapshot.PeakDay)
	}
	if snapshot.CurrentStreak != 2 {
		t.Fatalf("current streak = %d, want 2", snapshot.CurrentStreak)
	}
	if snapshot.LongestStreak != 3 {
		t.Fatalf("longest streak = %d, want 3", snapshot.LongestStreak)
	}
	if snapshot.UsefulSessions != 5 {
		t.Fatalf("useful sessions = %d, want 5", snapshot.UsefulSessions)
	}
	if len(snapshot.Models) != 1 || snapshot.Models[0].Model != "model-a" {
		t.Fatalf("models = %+v, want model-a", snapshot.Models)
	}
	statistics.Close()

	reopened := newStatistics(path, now)
	defer reopened.Close()
	again := reopened.Snapshot()
	if again.Lifetime.Total != snapshot.Lifetime.Total || again.LongestStreak != snapshot.LongestStreak {
		t.Fatalf("reloaded snapshot = %+v, want lifetime %d and longest %d", again, snapshot.Lifetime.Total, snapshot.LongestStreak)
	}
	if len(again.Days) != 5 {
		t.Fatalf("reloaded days = %d, want 5", len(again.Days))
	}
}
