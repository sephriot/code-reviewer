package db

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type ReviewTiming struct {
	Repo       string
	PRNumber   int
	Direction  string
	OpenedAt   time.Time
	ReviewedAt *time.Time
	UpdatedAt  time.Time
}

type TimingSyncState struct {
	BackfillSince  time.Time  `json:"backfill_since"`
	BackfillBefore time.Time  `json:"backfill_before"`
	LastSyncedAt   *time.Time `json:"last_synced_at"`
	LastError      string     `json:"last_error"`
}

func (d *DB) TimingSyncState(username string, now time.Time) (TimingSyncState, error) {
	username = strings.ToLower(username)
	since := truncateUTCDay(now.UTC().AddDate(0, 0, -365))
	before := truncateUTCDay(now).AddDate(0, 0, 1)
	_, err := d.Exec(`INSERT INTO review_timing_sync(username, backfill_since, backfill_before) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, username, timingTimestamp(since), timingTimestamp(before))
	if err != nil {
		return TimingSyncState{}, err
	}
	return d.GetTimingSyncState(username)
}

func (d *DB) GetTimingSyncState(username string) (TimingSyncState, error) {
	var state TimingSyncState
	var since, before scanTime
	var last nullScanTime
	err := d.QueryRow(`SELECT backfill_since, backfill_before, last_synced_at, last_error FROM review_timing_sync WHERE username = ?`, strings.ToLower(username)).Scan(&since, &before, &last, &state.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	state.BackfillSince, state.BackfillBefore = time.Time(since), time.Time(before)
	if last.Valid {
		state.LastSyncedAt = &last.Time
	}
	return state, err
}

func (d *DB) SaveTimingSyncState(username string, state TimingSyncState) error {
	_, err := d.Exec(`UPDATE review_timing_sync SET backfill_before = ?, last_synced_at = ?, last_error = ? WHERE username = ?`, timingTimestamp(state.BackfillBefore), nullableTimingTimestamp(state.LastSyncedAt), state.LastError, strings.ToLower(username))
	return err
}

func (d *DB) UpsertReviewTiming(username string, sample ReviewTiming) error {
	if sample.Repo == "" || sample.PRNumber <= 0 || sample.OpenedAt.IsZero() || sample.UpdatedAt.IsZero() || (sample.Direction != "given" && sample.Direction != "received") {
		return fmt.Errorf("invalid review timing sample")
	}
	if sample.ReviewedAt != nil && sample.ReviewedAt.Before(sample.OpenedAt) {
		return fmt.Errorf("review precedes PR creation")
	}
	_, err := d.Exec(`INSERT INTO review_timing(username, repo, pr_number, direction, opened_at, reviewed_at, gh_updated_at)
 VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(username, repo, pr_number, direction) DO UPDATE SET
 opened_at = excluded.opened_at, reviewed_at = excluded.reviewed_at, gh_updated_at = excluded.gh_updated_at`,
		strings.ToLower(username), strings.ToLower(sample.Repo), sample.PRNumber, sample.Direction, timingTimestamp(sample.OpenedAt), nullableTimingTimestamp(sample.ReviewedAt), timingTimestamp(sample.UpdatedAt))
	return err
}

func (d *DB) ReviewTimingCurrent(username, repo string, number int, direction string, updated time.Time) (bool, error) {
	var stored string
	err := d.QueryRow(`SELECT gh_updated_at FROM review_timing WHERE username = ? AND repo = ? AND pr_number = ? AND direction = ? AND reviewed_at IS NOT NULL`, strings.ToLower(username), strings.ToLower(repo), number, direction).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return stored == timingTimestamp(updated), err
}

func timingTimestamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") }
func nullableTimingTimestamp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return timingTimestamp(*t)
}

type TimingBucket struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

type TimingDay struct {
	Date         string   `json:"date"`
	Count        int      `json:"count"`
	AverageHours *float64 `json:"average_hours"`
	MedianHours  *float64 `json:"median_hours"`
}

type TimingStats struct {
	Count        int            `json:"count"`
	Awaiting     int            `json:"awaiting"`
	AverageHours *float64       `json:"average_hours"`
	MedianHours  *float64       `json:"median_hours"`
	Histogram    []TimingBucket `json:"histogram"`
	Daily        []TimingDay    `json:"daily"`
}

type ReviewTimingAnalytics struct {
	Given    TimingStats     `json:"given"`
	Received TimingStats     `json:"received"`
	Sync     TimingSyncState `json:"sync"`
}

// ReviewTimingAnalytics groups completed PRs by the UTC day of their first review; awaiting PRs use their creation date.
func (d *DB) ReviewTimingAnalytics(username string, since, until time.Time) (ReviewTimingAnalytics, error) {
	result := ReviewTimingAnalytics{}
	var err error
	result.Sync, err = d.GetTimingSyncState(username)
	if err != nil {
		return result, err
	}
	values := map[string][]float64{"given": {}, "received": {}}
	daily := map[string]map[string][]float64{"given": {}, "received": {}}
	stats := map[string]*TimingStats{"given": &result.Given, "received": &result.Received}
	for _, s := range stats {
		for _, label := range []string{"<1h", "1–4h", "4–12h", "12–24h", "1–2d", "2–7d", "≥7d"} {
			s.Histogram = append(s.Histogram, TimingBucket{Label: label})
		}
	}
	rows, err := d.Query(`SELECT direction, opened_at, reviewed_at FROM review_timing WHERE username = ? AND COALESCE(reviewed_at, opened_at) >= ? AND COALESCE(reviewed_at, opened_at) <= ?`, strings.ToLower(username), timingTimestamp(since), timingTimestamp(until))
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var direction string
		var opened scanTime
		var reviewed nullScanTime
		if err := rows.Scan(&direction, &opened, &reviewed); err != nil {
			return result, err
		}
		s := stats[direction]
		if !reviewed.Valid {
			s.Awaiting++
			continue
		}
		hours := reviewed.Time.Sub(time.Time(opened)).Hours()
		values[direction] = append(values[direction], hours)
		day := reviewed.Time.UTC().Format("2006-01-02")
		daily[direction][day] = append(daily[direction][day], hours)
		index := 0
		for _, boundary := range []float64{1, 4, 12, 24, 48, 168} {
			if hours >= boundary {
				index++
			}
		}
		s.Histogram[index].Count++
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	for direction, s := range stats {
		s.Count = len(values[direction])
		s.AverageHours, s.MedianHours = timingSummary(values[direction])
		start := since
		if start.Before(result.Sync.BackfillSince) {
			start = result.Sync.BackfillSince
		}
		if result.Sync.BackfillSince.IsZero() && until.Sub(start) > 365*24*time.Hour {
			start = until.AddDate(0, 0, -365)
		}
		for day := truncateUTCDay(start); !day.After(truncateUTCDay(until)); day = day.AddDate(0, 0, 1) {
			key := day.Format("2006-01-02")
			samples := daily[direction][key]
			average, median := timingSummary(samples)
			s.Daily = append(s.Daily, TimingDay{Date: key, Count: len(samples), AverageHours: average, MedianHours: median})
		}
	}
	return result, nil
}

func timingSummary(values []float64) (*float64, *float64) {
	if len(values) == 0 {
		return nil, nil
	}
	sort.Float64s(values)
	var sum float64
	for _, v := range values {
		sum += v
	}
	average := sum / float64(len(values))
	middle := len(values) / 2
	median := values[middle]
	if len(values)%2 == 0 {
		median = (values[middle-1] + median) / 2
	}
	return &average, &median
}
