// Package tariff implements the statutory tariff engine for Nigerian
// maritime revenue (W-FEAT-4). It replaces flat-rate declaration estimates
// with deterministic assessments over versioned, effective-dated,
// maker/checker-approved rate rows and first-class machine-checkable
// exemption rules. All rates are DATA in PostgreSQL — this package contains
// no hardcoded rate constants. All money is integer minor units.
//
// Determinism contract: the same request assessed against the same ACTIVE
// rate/exemption window rows produces byte-identical line items. Assessments
// are immutable and replay-safe by idempotency key.
package tariff

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrNotFound reports a missing record.
	ErrNotFound = errors.New("tariff record not found")
	// ErrIdempotencyConflict reports a key replay against a different request.
	ErrIdempotencyConflict = errors.New("idempotency key is bound to a different request")
	// ErrMakerChecker rejects self-approval at the service edge.
	ErrMakerChecker = errors.New("maker and checker must be distinct")
	// ErrInvalidTransition rejects a state move outside DRAFT->ACTIVE->RETIRED.
	ErrInvalidTransition = errors.New("tariff state transition is not permitted")
)

// Instruments (the statutory charge heads).
const (
	InstrumentNPAShipDues       = "NPA_SHIP_DUES"
	InstrumentNPALevyAct        = "NPA_LEVY_ACT"
	InstrumentSPL2012           = "SEA_PROTECTION_LEVY_2012"
	InstrumentCabotageSurcharge = "CABOTAGE_SURCHARGE"
	InstrumentNimasaLNGDue      = "NIMASA_LNG_CARRIER_DUE"
	InstrumentNIWAInlandCharge  = "NIWA_INLAND_CHARGE"
)

// Agencies.
const (
	AgencyNPA    = "NPA"
	AgencyNIMASA = "NIMASA"
	AgencyNIWA   = "NIWA"
	AgencyFMMBE  = "FMMBE"
)

// Band logics.
const (
	BandPerGRT       = "PER_GRT_BAND"
	BandGrossFreight = "PERCENT_GROSS_FREIGHT"
)

// Applicability outcomes on one assessment line.
const (
	LineCharged       = "CHARGED"
	LineExempt        = "EXEMPT"
	LineNotApplicable = "NOT_APPLICABLE"
	LineUnrated       = "UNRATED"
)

// Voyage/route classifications.
const (
	VoyageInternational = "INTERNATIONAL"
	VoyageCabotage      = "CABOTAGE"

	RouteSea            = "SEA"
	RouteInlandWaterway = "INLAND_WATERWAY"
)

// Vessel classes (extensible; unknown classes fail closed at validation).
const (
	VesselGeneral    = "GENERAL_CARGO"
	VesselTanker     = "TANKER"
	VesselCruise     = "CRUISE"
	VesselLNGCarrier = "LNG_CARRIER"
	VesselContainer  = "CONTAINER"
	VesselBulk       = "BULK"
	VesselBarge      = "BARGE"
	VesselPassenger  = "PASSENGER"
)

// Exemption match kinds.
const (
	MatchEntity        = "ENTITY"
	MatchCargoCategory = "CARGO_CATEGORY"
	MatchVoyageFlag    = "VOYAGE_FLAG"
	MatchCabotageTrade = "CABOTAGE_TRADE"
)

// AssessRequest is one voyage declaration for assessment. AsOf selects the
// statutory window (defaults to the service date when empty); it never
// defaults silently inside the engine — the store stamps it.
type AssessRequest struct {
	VesselGRT            int64    `json:"vesselGrt"`
	VesselClass          string   `json:"vesselClass"`
	EntityRef            string   `json:"entityRef"`
	CargoCategory        string   `json:"cargoCategory"`
	VoyageType           string   `json:"voyageType"`
	RouteKind            string   `json:"routeKind"`
	NigeriaPortCall      bool     `json:"nigeriaPortCall"`
	GrossFreightUSDMinor int64    `json:"grossFreightUsdMinor"`
	VoyageFlags          []string `json:"voyageFlags,omitempty"`
	AsOf                 string   `json:"asOf,omitempty"` // YYYY-MM-DD
}

// Validate fails closed on any malformed declaration.
func (request AssessRequest) Validate() error {
	if request.VesselGRT <= 0 {
		return errors.New("vesselGrt must be positive")
	}
	switch request.VesselClass {
	case VesselGeneral, VesselTanker, VesselCruise, VesselLNGCarrier, VesselContainer, VesselBulk, VesselBarge, VesselPassenger:
	default:
		return fmt.Errorf("vesselClass %q is not a known class", request.VesselClass)
	}
	if strings.TrimSpace(request.EntityRef) == "" || len(request.EntityRef) > 256 {
		return errors.New("entityRef is required")
	}
	if strings.TrimSpace(request.CargoCategory) == "" || len(request.CargoCategory) > 128 {
		return errors.New("cargoCategory is required")
	}
	switch request.VoyageType {
	case VoyageInternational, VoyageCabotage:
	default:
		return fmt.Errorf("voyageType must be %s or %s", VoyageInternational, VoyageCabotage)
	}
	switch request.RouteKind {
	case RouteSea, RouteInlandWaterway:
	default:
		return fmt.Errorf("routeKind must be %s or %s", RouteSea, RouteInlandWaterway)
	}
	if request.GrossFreightUSDMinor < 0 {
		return errors.New("grossFreightUsdMinor must not be negative")
	}
	if request.AsOf != "" {
		if _, err := time.Parse("2006-01-02", request.AsOf); err != nil {
			return errors.New("asOf must be YYYY-MM-DD")
		}
	}
	for _, flag := range request.VoyageFlags {
		if strings.TrimSpace(flag) == "" || len(flag) > 128 {
			return errors.New("voyage flags must be 1..128 characters")
		}
	}
	return nil
}

// RateRow is one versioned, effective-dated statutory rate.
type RateRow struct {
	RateID             string
	Instrument         string
	Agency             string
	BandLogic          string
	Currency           string
	RateMinorPerUnit   int64
	RateBps            int64
	BandFloor          int64
	BandCeiling        *int64
	StatutoryReference string
	Provisional        bool
	EffectiveFrom      time.Time
	EffectiveTo        *time.Time
	State              string
	Maker              string
	Checker            string
}

// ExemptionRow is one machine-checkable exemption rule.
type ExemptionRow struct {
	ExemptionID         string
	Instrument          string
	MatchKind           string
	MatchValue          string
	StatutoryBasis      string
	EvidenceRequirement string
	EffectiveFrom       time.Time
	EffectiveTo         *time.Time
	State               string
	Maker               string
	Checker             string
}

// AssessmentLine is one itemized instrument outcome.
type AssessmentLine struct {
	LineNo             int    `json:"lineNo"`
	Instrument         string `json:"instrument"`
	Agency             string `json:"agency"`
	Applicability      string `json:"applicability"` // CHARGED | EXEMPT | NOT_APPLICABLE | UNRATED
	Basis              string `json:"basis"`
	StatutoryReference string `json:"statutoryReference,omitempty"`
	RateDescription    string `json:"rateDescription,omitempty"`
	AmountMinor        int64  `json:"amountMinor"`
	Currency           string `json:"currency"`
	ExemptionID        string `json:"exemptionId,omitempty"`
	Provisional        bool   `json:"provisional,omitempty"`
}

// Assessment is the immutable, deterministic result.
type Assessment struct {
	AssessmentID  string           `json:"assessmentId"`
	Request       AssessRequest    `json:"request"`
	AsOf          string           `json:"asOf"`
	Lines         []AssessmentLine `json:"lines"`
	TotalUSDMinor int64            `json:"totalUsdMinor"`
	TotalNGNMinor int64            `json:"totalNgnMinor"`
	Requester     string           `json:"requester"`
	CorrelationID string           `json:"correlationId"`
	CreatedAt     time.Time        `json:"createdAt"`
}

// ExemptionAudit is one applied exemption audit record.
type ExemptionAudit struct {
	AuditID             string
	AssessmentID        string
	ExemptionID         string
	Instrument          string
	MatchKind           string
	MatchValue          string
	StatutoryBasis      string
	EvidenceRequirement string
	Requester           string
	CreatedAt           time.Time
}

// roundHalfUp divides numerator by denominator rounding half away from zero
// (the statutory rounding for money in minor units).
func roundHalfUp(numerator, denominator int64) int64 {
	if denominator <= 0 || numerator < 0 {
		return 0
	}
	return (2*numerator/denominator + 1) / 2
}
