package cvff

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Approved enumerations for beneficiary intake. They mirror the beneficiary
// portal contract; the database enforces the same values.
const (
	VesselClassFishingTrawler = "FISHING_TRAWLER"
	VesselClassCargoCoaster   = "CARGO_COASTER"
	VesselClassTug            = "TUG"
	VesselClassBarge          = "BARGE"
	VesselClassPassengerFerry = "PASSENGER_FERRY"
	VesselClassSupplyVessel   = "SUPPLY_VESSEL"
	VesselClassCrewBoat       = "CREW_BOAT"
	VesselClassOther          = "OTHER"
)

const (
	RouteLagosPortHarcourt = "LAGOS_PORT_HARCOURT"
	RouteLagosOnne         = "LAGOS_ONNE"
	RouteLagosWarri        = "LAGOS_WARRI"
	RouteLagosCalabar      = "LAGOS_CALABAR"
	RoutePortHarcourtBonny = "PORT_HARCOURT_BONNY"
	RouteWarriEscravos     = "WARRI_ESCRAVOS"
	RouteInlandWaterways   = "INLAND_WATERWAYS"
	RouteOther             = "OTHER"
)

// Approved supporting-document types for a CVFF application.
const (
	DocumentTypeVesselRegistration = "VESSEL_REGISTRATION"
	DocumentTypeCabotageLicense    = "CABOTAGE_LICENSE"
	DocumentTypeBankDetails        = "BANK_DETAILS"
)

// CVFF loan bounds in kobo: ₦5,000,000.00 minimum, ₦2,000,000,000.00 maximum.
const (
	MinIntakeAmountKobo uint64 = 500_000_000
	MaxIntakeAmountKobo uint64 = 200_000_000_000
)

var (
	officialNumberPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9/-]{1,31}$`)
	businessRCPattern     = regexp.MustCompile(`^(?i)RC[0-9]{4,10}$`)
	fileNamePattern       = regexp.MustCompile(`^[\w,.() -]{1,128}$`)
	sha256HexPattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ErrIntakeInvalid marks beneficiary intake that violates the approved field
// contract. The beneficiary API maps it to a 422 problem with field errors.
var ErrIntakeInvalid = errors.New("cvff intake is invalid")

// FieldError names one rejected intake field.
type FieldError struct {
	Field   string
	Message string
}

func (fieldError FieldError) Error() string {
	return fieldError.Field + ": " + fieldError.Message
}

// FieldErrors is the per-field rejection set for one intake payload.
type FieldErrors []FieldError

func (errs FieldErrors) Error() string {
	if len(errs) == 0 {
		return ErrIntakeInvalid.Error()
	}
	return fmt.Sprintf("%s (%d field(s))", errs[0].Error(), len(errs))
}

// Intake is one beneficiary-submitted CVFF application with the vessel and
// business detail the four-party chain underwrites. Amount is in kobo.
type Intake struct {
	ApplicationID    string
	IdempotencyKey   string
	BeneficiaryID    string
	VesselName       string
	IMONumber        string
	OfficialNumber   string
	VesselClass      string
	CabotageRoute    string
	Amount           uint64
	Currency         string
	BusinessName     string
	BusinessRCNumber string
	BusinessAddress  string
}

func validVesselClass(value string) bool {
	switch value {
	case VesselClassFishingTrawler, VesselClassCargoCoaster, VesselClassTug, VesselClassBarge,
		VesselClassPassengerFerry, VesselClassSupplyVessel, VesselClassCrewBoat, VesselClassOther:
		return true
	default:
		return false
	}
}

func validCabotageRoute(value string) bool {
	switch value {
	case RouteLagosPortHarcourt, RouteLagosOnne, RouteLagosWarri, RouteLagosCalabar,
		RoutePortHarcourtBonny, RouteWarriEscravos, RouteInlandWaterways, RouteOther:
		return true
	default:
		return false
	}
}

// ValidIMONumber verifies the seven-digit IMO number including its check
// digit: for digits d1..d7, (7*d1 + 6*d2 + 5*d3 + 4*d4 + 3*d5 + 2*d6) mod 10
// must equal d7.
func ValidIMONumber(value string) bool {
	if len(value) != 7 {
		return false
	}
	digits := make([]int, 7)
	for index, character := range value {
		if character < '0' || character > '9' {
			return false
		}
		digits[index] = int(character - '0')
	}
	sum := 0
	for index := 0; index < 6; index++ {
		sum += digits[index] * (7 - index)
	}
	return sum%10 == digits[6]
}

// Validate enforces the approved intake contract and reports every rejected
// field so the caller can return a per-field problem document.
func (intake Intake) Validate() FieldErrors {
	var errs FieldErrors
	if err := ValidateIdentifier("application_id", intake.ApplicationID); err != nil {
		errs = append(errs, FieldError{Field: "application_id", Message: err.Error()})
	}
	if err := ValidateIdentifier("idempotency_key", intake.IdempotencyKey); err != nil {
		errs = append(errs, FieldError{Field: "idempotency_key", Message: "a canonical Idempotency-Key header is required"})
	}
	if err := ValidateIdentifier("beneficiary_id", intake.BeneficiaryID); err != nil {
		errs = append(errs, FieldError{Field: "beneficiary_id", Message: err.Error()})
	}
	vesselName := strings.TrimSpace(intake.VesselName)
	if len(vesselName) < 2 || len(vesselName) > 128 {
		errs = append(errs, FieldError{Field: "vessel_name", Message: "Vessel name is required (2-128 characters)."})
	}
	if !ValidIMONumber(strings.TrimSpace(intake.IMONumber)) {
		errs = append(errs, FieldError{Field: "imo_number", Message: "Enter a valid 7-digit IMO number (check digit verified)."})
	}
	if !officialNumberPattern.MatchString(strings.TrimSpace(intake.OfficialNumber)) {
		errs = append(errs, FieldError{Field: "official_number", Message: "Official registry number is required (letters, digits, '/' or '-')."})
	}
	if !validVesselClass(intake.VesselClass) {
		errs = append(errs, FieldError{Field: "vessel_class", Message: "Select the vessel class."})
	}
	if !validCabotageRoute(intake.CabotageRoute) {
		errs = append(errs, FieldError{Field: "cabotage_route", Message: "Select the primary cabotage trade route."})
	}
	if intake.Amount < MinIntakeAmountKobo || intake.Amount > MaxIntakeAmountKobo {
		errs = append(errs, FieldError{Field: "amount", Message: "Requested amount must be between ₦5,000,000.00 and ₦2,000,000,000.00."})
	}
	if intake.Currency != "NGN" {
		errs = append(errs, FieldError{Field: "currency", Message: "CVFF applications are denominated in NGN."})
	}
	businessName := strings.TrimSpace(intake.BusinessName)
	if len(businessName) < 2 || len(businessName) > 256 {
		errs = append(errs, FieldError{Field: "business_name", Message: "Registered business name is required (2-256 characters)."})
	}
	if !businessRCPattern.MatchString(strings.TrimSpace(intake.BusinessRCNumber)) {
		errs = append(errs, FieldError{Field: "business_rc_number", Message: "Enter the CAC registration number in the form RC123456."})
	}
	businessAddress := strings.TrimSpace(intake.BusinessAddress)
	if len(businessAddress) < 8 || len(businessAddress) > 512 {
		errs = append(errs, FieldError{Field: "business_address", Message: "Business address is required (8-512 characters)."})
	}
	return errs
}

// ApplicationDetail is the beneficiary-facing read model: the durable
// application, its intake detail and the time the current state was entered.
type ApplicationDetail struct {
	Application
	VesselName       string    `json:"vessel_name"`
	IMONumber        string    `json:"imo_number"`
	OfficialNumber   string    `json:"official_number"`
	VesselClass      string    `json:"vessel_class"`
	CabotageRoute    string    `json:"cabotage_route"`
	BusinessName     string    `json:"business_name"`
	BusinessRCNumber string    `json:"business_rc_number"`
	BusinessAddress  string    `json:"business_address"`
	StateEnteredAt   time.Time `json:"state_entered_at"`
}

// Document is the metadata record for one beneficiary-uploaded supporting
// document. Bytes live in the configured object-storage backend under the
// content-addressed StorageKey; SHA256Hex is the recorded integrity digest.
type Document struct {
	DocumentID     string    `json:"document_id"`
	ApplicationID  string    `json:"application_id"`
	BeneficiaryID  string    `json:"beneficiary_id"`
	DocumentType   string    `json:"document_type"`
	FileName       string    `json:"file_name"`
	ContentType    string    `json:"content_type"`
	SizeBytes      int64     `json:"size_bytes"`
	SHA256Hex      string    `json:"sha256_hex"`
	StorageBackend string    `json:"storage_backend"`
	StorageKey     string    `json:"storage_key"`
	IdempotencyKey string    `json:"idempotency_key"`
	CreatedAt      time.Time `json:"created_at"`
}

// Validate enforces the document metadata contract before persistence.
func (document Document) Validate() error {
	if err := ValidateIdentifier("application_id", document.ApplicationID); err != nil {
		return err
	}
	if err := ValidateIdentifier("beneficiary_id", document.BeneficiaryID); err != nil {
		return err
	}
	if err := ValidateIdentifier("idempotency_key", document.IdempotencyKey); err != nil {
		return err
	}
	switch document.DocumentType {
	case DocumentTypeVesselRegistration, DocumentTypeCabotageLicense, DocumentTypeBankDetails:
	default:
		return fmt.Errorf("%w: document_type %q is not an approved CVFF document type", ErrIntakeInvalid, document.DocumentType)
	}
	if !fileNamePattern.MatchString(document.FileName) {
		return fmt.Errorf("%w: file name contains characters outside the approved set", ErrIntakeInvalid)
	}
	if document.SizeBytes <= 0 {
		return fmt.Errorf("%w: document content must be non-empty", ErrIntakeInvalid)
	}
	if !sha256HexPattern.MatchString(document.SHA256Hex) {
		return fmt.Errorf("%w: sha256 digest must be 64 lowercase hex characters", ErrIntakeInvalid)
	}
	switch document.StorageBackend {
	case "adls", "s3", "local-gated":
	default:
		return fmt.Errorf("%w: storage backend %q is not approved", ErrIntakeInvalid, document.StorageBackend)
	}
	if strings.TrimSpace(document.StorageKey) == "" || strings.HasPrefix(document.StorageKey, "/") {
		return fmt.Errorf("%w: storage key must be a canonical relative object key", ErrIntakeInvalid)
	}
	return nil
}
