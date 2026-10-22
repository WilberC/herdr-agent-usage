/**
 * The panel-facing view of the NaN quota.
 *
 * One display row per metered model, grouped under "Nan" once two or more
 * models report allowances (the same grouping multiple Claude accounts use).
 * The fetch is throttled well below the panel's 15s refresh ticker, and a
 * failed fetch keeps the last valid snapshot rather than blanking the block.
 */
package nan

import (
	"os"
	"sync"
	"time"

	"github.com/senna-lang/herdr-agent-usage/internal/limitscore"
)

// getenv is the indirection tests use instead of touching the process env.
var getenv = os.Getenv

// MinFetchIntervalMs is the smallest gap between two quota fetches. The
// endpoint is undocumented and the dashboard reads it on page load, not on a
// ticker, so this stays conservative.
const MinFetchIntervalMs = 5 * 60 * 1000

// snapshot is the latest successfully parsed quota, guarded for the panel's
// concurrent refresh paths.
type snapshot struct {
	mu        sync.Mutex
	quota     *Quota
	fetchedAt int64
}

var current snapshot

// Reset drops the cached snapshot. Tests call this; production never does.
func Reset() {
	current.mu.Lock()
	defer current.mu.Unlock()
	current.quota = nil
	current.fetchedAt = 0
}

// fetcher is the indirection tests use instead of the network.
var fetcher = Fetch

// Limits returns one display row per metered model, or nil when no
// credential is configured. A fresh-enough cached snapshot is returned as
// is; a fetch failure keeps the previous snapshot.
func Limits(nowMs int64) []limitscore.ProviderLimits {
	key, err := ResolveAPIKey(getenv)
	if err != nil {
		return nil
	}

	current.mu.Lock()
	defer current.mu.Unlock()
	if current.quota == nil || nowMs-current.fetchedAt >= MinFetchIntervalMs {
		if quota, fetchErr := fetcher(key, nowMs, nil); fetchErr == nil {
			current.quota = &quota
			current.fetchedAt = nowMs
		}
	}
	if current.quota == nil {
		return nil
	}
	return displayRows(*current.quota, nowMs)
}

// displayRows converts one quota snapshot into panel rows. The period window
// spans the account's billing period; a model with its own period end uses
// that. The rolling window rides as Secondary.
func displayRows(quota Quota, nowMs int64) []limitscore.ProviderLimits {
	rows := make([]limitscore.ProviderLimits, 0, len(quota.Models))
	for _, model := range quota.Models {
		periodEnd := quota.PeriodStart
		if model.PeriodEnd != nil {
			periodEnd = model.PeriodEnd
		}
		row := limitscore.ProviderLimits{
			ProviderID:   "nan",
			Label:        model.Model,
			AccountLabel: model.Model,
			GroupLabel:   "Nan",
			Source:       "nan-cloud",
			FetchedAtMs:  quota.FetchedAtMs,
			Primary:      periodWindow(model, quota.PeriodStart, periodEnd, nowMs),
		}
		if model.Window != nil {
			row.Secondary = rollingWindow(*model.Window)
		}
		rows = append(rows, row)
	}
	return rows
}

// periodWindow is the billing-period allowance. The window length the
// formatter labels with is the span the period actually covers, so a
// first-period proration reads as what it is.
func periodWindow(model ModelQuota, periodStart, periodEnd *time.Time, nowMs int64) *limitscore.LimitWindow {
	window := limitscore.LimitWindow{
		UsedPercentage: (float64(model.TokensUsed) / float64(model.Allowance)) * 100,
	}
	if periodEnd != nil {
		window.ResetsAt = new(int64)
		*window.ResetsAt = periodEnd.Unix()
		if periodStart != nil && periodEnd.After(*periodStart) {
			minutes := int(periodEnd.Sub(*periodStart).Minutes())
			window.WindowMinutes = &minutes
		}
	}
	return &window
}

// rollingWindow is the per-model usage window some models apply on top of
// the period allowance.
func rollingWindow(window RollingWindow) *limitscore.LimitWindow {
	used := (float64(window.TokensUsed) / float64(window.Budget)) * 100
	out := limitscore.LimitWindow{
		UsedPercentage: used,
		WindowMinutes:  new(int),
	}
	*out.WindowMinutes = int(window.Hours) * 60
	if window.ResetsAt != nil {
		out.ResetsAt = new(int64)
		*out.ResetsAt = window.ResetsAt.Unix()
	}
	return &out
}
