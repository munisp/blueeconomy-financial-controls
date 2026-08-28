package cvff

import (
	"strings"
	"testing"
)

func validIntake() Intake {
	return Intake{
		ApplicationID:    "cvff-intake-001",
		IdempotencyKey:   "7f3a1c2e-9b0d-4e5f-a1b2-c3d4e5f60718",
		BeneficiaryID:    "kc-beneficiary-001",
		VesselName:       "MV Adaeze",
		IMONumber:        "9074729",
		OfficialNumber:   "NIMASA-4421",
		VesselClass:      VesselClassCargoCoaster,
		CabotageRoute:    RouteLagosOnne,
		Amount:           750_000_000,
		Currency:         "NGN",
		BusinessName:     "Adaeze Coastal Logistics Ltd",
		BusinessRCNumber: "RC123456",
		BusinessAddress:  "14 Marina Road, Lagos Island, Lagos",
	}
}

func fieldSet(errs FieldErrors) map[string]string {
	fields := map[string]string{}
	for _, fieldError := range errs {
		fields[fieldError.Field] = fieldError.Message
	}
	return fields
}

func TestValidIMONumberCheckDigit(t *testing.T) {
	// 9074729: (7*9 + 6*0 + 5*7 + 4*4 + 3*7 + 2*2) mod 10 = 139 mod 10 = 9.
	if !ValidIMONumber("9074729") {
		t.Fatal("valid IMO number rejected")
	}
	for _, invalid := range []string{"9074728", "907472", "90747290", "90747A9", ""} {
		if ValidIMONumber(invalid) {
			t.Fatalf("invalid IMO number %q accepted", invalid)
		}
	}
}

func TestIntakeValidateAcceptsApprovedPayload(t *testing.T) {
	if errs := validIntake().Validate(); len(errs) != 0 {
		t.Fatalf("valid intake rejected: %v", errs)
	}
}

func TestIntakeValidateReportsEveryField(t *testing.T) {
	intake := Intake{}
	fields := fieldSet(intake.Validate())
	for _, field := range []string{
		"application_id", "idempotency_key", "beneficiary_id", "vessel_name", "imo_number",
		"official_number", "vessel_class", "cabotage_route", "amount", "currency",
		"business_name", "business_rc_number", "business_address",
	} {
		if _, ok := fields[field]; !ok {
			t.Fatalf("field %s missing from rejection set %v", field, fields)
		}
	}
}

func TestIntakeValidateAmountBounds(t *testing.T) {
	for _, amount := range []uint64{0, MinIntakeAmountKobo - 1, MaxIntakeAmountKobo + 1} {
		intake := validIntake()
		intake.Amount = amount
		if _, ok := fieldSet(intake.Validate())["amount"]; !ok {
			t.Fatalf("amount %d accepted", amount)
		}
	}
	for _, amount := range []uint64{MinIntakeAmountKobo, MaxIntakeAmountKobo} {
		intake := validIntake()
		intake.Amount = amount
		if _, ok := fieldSet(intake.Validate())["amount"]; ok {
			t.Fatalf("boundary amount %d rejected", amount)
		}
	}
}

func TestIntakeValidateCurrencyFailClosed(t *testing.T) {
	intake := validIntake()
	intake.Currency = "USD"
	if _, ok := fieldSet(intake.Validate())["currency"]; !ok {
		t.Fatal("USD intake accepted; beneficiary intake is NGN-denominated")
	}
}

func TestIntakeValidateEnumerations(t *testing.T) {
	intake := validIntake()
	intake.VesselClass = "SUPERYACHT"
	intake.CabotageRoute = "INTERNATIONAL"
	fields := fieldSet(intake.Validate())
	if _, ok := fields["vessel_class"]; !ok {
		t.Fatal("unapproved vessel class accepted")
	}
	if _, ok := fields["cabotage_route"]; !ok {
		t.Fatal("unapproved cabotage route accepted")
	}
}

func TestIntakeValidateRCNumber(t *testing.T) {
	intake := validIntake()
	intake.BusinessRCNumber = "123456"
	if _, ok := fieldSet(intake.Validate())["business_rc_number"]; !ok {
		t.Fatal("RC number without RC prefix accepted")
	}
	intake = validIntake()
	intake.BusinessRCNumber = "rc123456"
	if errs := intake.Validate(); len(errs) != 0 {
		t.Fatalf("lowercase RC prefix rejected: %v", errs)
	}
}

func validDocument() Document {
	return Document{
		ApplicationID:  "cvff-intake-001",
		BeneficiaryID:  "kc-beneficiary-001",
		DocumentType:   DocumentTypeVesselRegistration,
		FileName:       "vessel-registration.pdf",
		ContentType:    "application/pdf",
		SizeBytes:      4096,
		SHA256Hex:      strings.Repeat("a", 64),
		StorageBackend: "s3",
		StorageKey:     "cvff-documents/cvff-intake-001/" + strings.Repeat("a", 64),
		IdempotencyKey: "7f3a1c2e-9b0d-4e5f-a1b2-c3d4e5f60718",
	}
}

func TestDocumentValidateAcceptsApprovedMetadata(t *testing.T) {
	if err := validDocument().Validate(); err != nil {
		t.Fatalf("valid document rejected: %v", err)
	}
}

func TestDocumentValidateFailClosed(t *testing.T) {
	cases := map[string]func(*Document){
		"document_type":   func(document *Document) { document.DocumentType = "PASSPORT" },
		"file_name":       func(document *Document) { document.FileName = "../../etc/passwd" },
		"size_bytes":      func(document *Document) { document.SizeBytes = 0 },
		"sha256_hex":      func(document *Document) { document.SHA256Hex = "not-hex" },
		"storage_backend": func(document *Document) { document.StorageBackend = "local" },
		"storage_key":     func(document *Document) { document.StorageKey = "/absolute/path" },
	}
	for name, mutate := range cases {
		document := validDocument()
		mutate(&document)
		if err := document.Validate(); err == nil {
			t.Fatalf("case %s: invalid document accepted", name)
		}
	}
}
