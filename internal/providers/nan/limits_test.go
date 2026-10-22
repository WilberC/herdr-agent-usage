package nan

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// stubFetch installs a fetch stub and returns its restore, its recorded key,
// and a call counter pointer.
func stubFetch(t *testing.T, quota Quota, err error) (restore func(), key *string, calls *int) {
	t.Helper()
	Reset()
	count := 0
	seen := ""
	origFetch, origGetenv := fetcher, getenv
	fetcher = func(apiKey string, nowMs int64, _ *http.Client) (Quota, error) {
		count++
		seen = apiKey
		return quota, err
	}
	getenv = func(name string) string {
		if name == "NAN_API_KEY" {
			return "test-key"
		}
		return ""
	}
	return func() { fetcher, getenv = origFetch, origGetenv }, &seen, &count
}

func TestLimits_OneRowPerMeteredModelGroupedUnderNan(t *testing.T) {
	quota, err := ParseQuota([]byte(liveShapePayload), 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	restore, key, _ := stubFetch(t, quota, nil)
	defer restore()

	rows := Limits(0)
	if len(rows) != 3 {
		t.Fatalf("one row per model, got %d", len(rows))
	}
	if *key != "test-key" {
		t.Fatalf("the resolved key authenticates the fetch: %q", *key)
	}
	for _, row := range rows {
		if row.ProviderID != "nan" || row.GroupLabel != "Nan" || row.AccountLabel == "" {
			t.Fatalf("each row names its model under the shared group: %+v", row)
		}
		if row.Primary == nil {
			t.Fatalf("each row carries its period percentage: %+v", row.Primary)
		}
	}
}

func TestLimits_RollingWindowRidesAsSecondary(t *testing.T) {
	quota, err := ParseQuota([]byte(liveShapePayload), 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	restore, _, _ := stubFetch(t, quota, nil)
	defer restore()

	for _, row := range Limits(0) {
		if row.AccountLabel != "glm5.3" {
			continue
		}
		if row.Secondary == nil || *row.Secondary.WindowMinutes != 4*60 {
			t.Fatalf("the 4h window rides as secondary: %+v", row.Secondary)
		}
	}
}

func TestLimits_ThrottlesFetches(t *testing.T) {
	quota, err := ParseQuota([]byte(liveShapePayload), 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	restore, _, calls := stubFetch(t, quota, nil)
	defer restore()

	Limits(0)
	Limits(0)
	Limits(MinFetchIntervalMs - 1)
	if *calls != 1 {
		t.Fatalf("calls inside the throttle window reuse the snapshot: %d", *calls)
	}
	Limits(MinFetchIntervalMs)
	if *calls != 2 {
		t.Fatalf("a fetch past the window runs again: %d", *calls)
	}
}

func TestLimits_FailureKeepsLastValidSnapshot(t *testing.T) {
	quota, err := ParseQuota([]byte(liveShapePayload), 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	restore, _, _ := stubFetch(t, quota, nil)
	defer restore()

	if rows := Limits(0); len(rows) != 3 {
		t.Fatalf("the first fetch populates the block: %d rows", len(rows))
	}
	count := 0
	origFetch := fetcher
	fetcher = func(string, int64, *http.Client) (Quota, error) {
		count++
		return Quota{}, errors.New("HTTP 503")
	}
	rows := Limits(MinFetchIntervalMs)
	fetcher = origFetch
	if count != 1 {
		t.Fatalf("the failed fetch ran once past the throttle window: %d", count)
	}
	if len(rows) != 3 {
		t.Fatalf("a failed fetch keeps the last valid snapshot: %d rows", len(rows))
	}
}

func TestLimits_NoCredentialMeansNoBlock(t *testing.T) {
	Reset()
	origGetenv := getenv
	getenv = func(string) string { return "" }
	defer func() { getenv = origGetenv }()

	if rows := Limits(0); rows != nil {
		t.Fatalf("a machine without nan credentials shows no nan block: %+v", rows)
	}
}

func TestDisplayRows_PeriodWindowSpansTheBillingPeriod(t *testing.T) {
	quota, err := ParseQuota([]byte(liveShapePayload), 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	rows := displayRows(quota, now.UnixMilli())
	deepseek := rows[0]
	if deepseek.Primary.WindowMinutes == nil || *deepseek.Primary.WindowMinutes != 31*24*60 {
		t.Fatalf("the period window spans Oct 1 to Nov 1: %+v", deepseek.Primary.WindowMinutes)
	}
	if deepseek.Primary.ResetsAt == nil || *deepseek.Primary.ResetsAt != time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("the reset is the period end: %+v", deepseek.Primary.ResetsAt)
	}
}
