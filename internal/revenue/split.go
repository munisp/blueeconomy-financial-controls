package revenue

import (
	"sort"
	"time"
)

// ComputeSplit is the pure, deterministic TSA split: given the ACTIVE rule
// rows selected by the store for one revenue line and asOf window, it
// divides amountMinor exactly across beneficiaries. The rules must sum to
// exactly 10000 bps or the computation fails closed (ErrSplitIncomplete) —
// a partial rule window must never leak revenue into an unstated bucket.
//
// Shares are rounded half-up per leg; the rounding delta (at most a few
// minor units) is assigned to the leg with the largest share, ties broken
// by beneficiary name — a documented, deterministic residual rule. The same
// (amount, rules, asOf) triple always yields byte-identical allocations.
func ComputeSplit(amountMinor int64, rules []SplitRuleRow, asOf time.Time) ([]Allocation, error) {
	selected := make([]SplitRuleRow, 0, len(rules))
	for _, rule := range rules {
		if rule.State != RuleActive {
			continue
		}
		if rule.EffectiveFrom.After(asOf) {
			continue
		}
		if rule.EffectiveTo != nil && rule.EffectiveTo.Before(asOf) {
			continue
		}
		selected = append(selected, rule)
	}
	if len(selected) == 0 {
		return nil, ErrSplitIncomplete
	}
	// Deterministic leg order: share descending, then beneficiary name.
	sort.Slice(selected, func(i, j int) bool {
		if selected[i].ShareBps != selected[j].ShareBps {
			return selected[i].ShareBps > selected[j].ShareBps
		}
		return selected[i].Beneficiary < selected[j].Beneficiary
	})
	var totalBps int64
	for _, rule := range selected {
		totalBps += rule.ShareBps
	}
	if totalBps != 10000 {
		return nil, ErrSplitIncomplete
	}
	allocations := make([]Allocation, 0, len(selected))
	var distributed int64
	for _, rule := range selected {
		share := roundHalfUp(amountMinor*rule.ShareBps, 10000)
		allocations = append(allocations, Allocation{
			Beneficiary: rule.Beneficiary,
			ShareBps:    rule.ShareBps,
			AmountMinor: share,
		})
		distributed += share
	}
	// Residual: deterministic adjustment of the largest-share leg (already
	// first after the sort).
	if delta := amountMinor - distributed; delta != 0 {
		allocations[0].AmountMinor += delta
		if allocations[0].AmountMinor < 0 {
			return nil, ErrSplitIncomplete
		}
	}
	return allocations, nil
}

// roundHalfUp divides numerator by denominator rounding half away from zero
// (same statutory rounding as the tariff engine).
func roundHalfUp(numerator, denominator int64) int64 {
	if denominator <= 0 || numerator < 0 {
		return 0
	}
	return (2*numerator/denominator + 1) / 2
}
