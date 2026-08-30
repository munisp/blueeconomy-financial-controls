package revenue

import (
	"testing"
	"time"
)

func rule(beneficiary string, bps int64) SplitRuleRow {
	return SplitRuleRow{
		RuleID: "r-" + beneficiary, RevenueLine: "NPA_SHIP_DUES", Agency: "NPA",
		Beneficiary: beneficiary, ShareBps: bps, State: RuleActive,
		EffectiveFrom: time.Date(2007, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func asOf(year int) time.Time {
	return time.Date(year, 6, 1, 0, 0, 0, 0, time.UTC)
}

// TestComputeSplitDeterministic: same inputs, byte-identical outputs.
func TestComputeSplitDeterministic(t *testing.T) {
	rules := []SplitRuleRow{rule(BeneficiaryAgencyRetained, 7000), rule(BeneficiaryFGN, 3000)}
	first, err := ComputeSplit(1000000, rules, asOf(2026))
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	second, err := ComputeSplit(1000000, rules, asOf(2026))
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("allocations: %+v", first)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("non-deterministic: %+v vs %+v", first, second)
		}
	}
	var total int64
	for _, allocation := range first {
		total += allocation.AmountMinor
	}
	if total != 1000000 {
		t.Fatalf("allocations sum %d, want 1000000", total)
	}
	if first[0].Beneficiary != BeneficiaryAgencyRetained || first[0].AmountMinor != 700000 {
		t.Fatalf("agency leg: %+v", first[0])
	}
	if first[1].Beneficiary != BeneficiaryFGN || first[1].AmountMinor != 300000 {
		t.Fatalf("fgn leg: %+v", first[1])
	}
}

// TestComputeSplitRoundingResidual: half-up rounding with the deterministic
// residual assigned to the largest-share leg.
func TestComputeSplitRoundingResidual(t *testing.T) {
	rules := []SplitRuleRow{rule(BeneficiaryAgencyRetained, 3333), rule(BeneficiaryFGN, 3333),
		rule(BeneficiaryCVFFFiduciary, 3334)}
	allocations, err := ComputeSplit(100, rules, asOf(2026))
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	var total int64
	for _, allocation := range allocations {
		total += allocation.AmountMinor
	}
	if total != 100 {
		t.Fatalf("allocations sum %d, want 100: %+v", total, allocations)
	}
	// Largest share (3334) is first and absorbs any residual.
	if allocations[0].Beneficiary != BeneficiaryCVFFFiduciary {
		t.Fatalf("largest-share leg first: %+v", allocations)
	}
}

// TestComputeSplitFailsClosedOnIncompleteWindow: rules not summing to 10000
// must refuse, never leak into an unstated bucket.
func TestComputeSplitFailsClosedOnIncompleteWindow(t *testing.T) {
	if _, err := ComputeSplit(1000, []SplitRuleRow{rule(BeneficiaryAgencyRetained, 7000)}, asOf(2026)); err != ErrSplitIncomplete {
		t.Fatalf("want ErrSplitIncomplete, got %v", err)
	}
	if _, err := ComputeSplit(1000, nil, asOf(2026)); err != ErrSplitIncomplete {
		t.Fatalf("want ErrSplitIncomplete for empty rules, got %v", err)
	}
	over := []SplitRuleRow{rule(BeneficiaryAgencyRetained, 7000), rule(BeneficiaryFGN, 4000)}
	if _, err := ComputeSplit(1000, over, asOf(2026)); err != ErrSplitIncomplete {
		t.Fatalf("want ErrSplitIncomplete for >10000, got %v", err)
	}
}

// TestComputeSplitEffectiveWindows: rules outside the asOf window are not
// selected (deterministic asOf before/after a rule change).
func TestComputeSplitEffectiveWindows(t *testing.T) {
	future := rule(BeneficiaryFGN, 3000)
	future.EffectiveFrom = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	current := rule(BeneficiaryAgencyRetained, 10000)
	rules := []SplitRuleRow{current, future}
	// Before the FGN rule opens: only the 100% agency rule applies.
	allocations, err := ComputeSplit(500, rules, asOf(2026))
	if err != nil {
		t.Fatalf("compute 2026: %v", err)
	}
	if len(allocations) != 1 || allocations[0].Beneficiary != BeneficiaryAgencyRetained || allocations[0].AmountMinor != 500 {
		t.Fatalf("2026 allocations: %+v", allocations)
	}
	// After: both rules are in-window but sum to 130% -> fail closed.
	if _, err := ComputeSplit(500, rules, asOf(2027)); err != ErrSplitIncomplete {
		t.Fatalf("want ErrSplitIncomplete for overlapping rules, got %v", err)
	}
	// Retire the 100% rule at 2027: closed window restores a valid 100%.
	closed := rule(BeneficiaryAgencyRetained, 7000)
	to := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	closed.EffectiveTo = &to
	windowed := []SplitRuleRow{closed, future, rule(BeneficiaryFGN, 3000)}
	_ = windowed
	before, err := ComputeSplit(1000, []SplitRuleRow{closed, rule(BeneficiaryFGN, 3000)}, asOf(2026))
	if err != nil {
		t.Fatalf("compute before close: %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("before allocations: %+v", before)
	}
	if _, err := ComputeSplit(1000, []SplitRuleRow{closed, rule(BeneficiaryFGN, 3000)}, asOf(2027)); err != ErrSplitIncomplete {
		t.Fatalf("want incomplete after agency rule closes (7000 closed + 3000 open), got %v", err)
	}
}
