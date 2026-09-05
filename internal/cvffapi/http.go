// Package cvffapi implements the beneficiary-facing CVFF HTTP API: own
// applications, the immutable decision timeline and supporting-document
// uploads. Beneficiaries see only their own applications; other owners' IDs
// fail closed with 404. Every dependency is injected and required.
package cvffapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/munisp/blueeconomy-financial-controls/internal/cvff"
	"github.com/munisp/blueeconomy-financial-controls/internal/pbac"
)

// Store is the persistence boundary used by the CVFF API.
type Store interface {
	SubmitIntake(ctx context.Context, intake cvff.Intake) (cvff.ApplicationDetail, error)
	GetForBeneficiary(ctx context.Context, applicationID string, beneficiaryID string) (cvff.ApplicationDetail, error)
	ListForBeneficiary(ctx context.Context, beneficiaryID string) ([]cvff.ApplicationDetail, error)
	ListApprovals(ctx context.Context, applicationID string) ([]cvff.Approval, error)
	CreateDocument(ctx context.Context, document cvff.Document) (cvff.Document, error)
	ListDocuments(ctx context.Context, applicationID string) ([]cvff.Document, error)
	// DualLedgerReport is the auditor-facing NGN/USD disbursement report over
	// the mandatory half-open window [from, to).
	DualLedgerReport(ctx context.Context, from, to time.Time) ([]cvff.DualLedgerReport, error)
	// Get loads one application by ID for the four-party pipeline routes.
	Get(ctx context.Context, applicationID string) (cvff.Application, error)
	// RoleAssignments returns the durable four-party bindings of one
	// application.
	RoleAssignments(ctx context.Context, applicationID string) (map[cvff.Role]string, error)
	// AssignRoles binds the four-party roles on one application
	// (idempotently; divergence conflicts).
	AssignRoles(ctx context.Context, applicationID string, officerPrincipal string, assignments map[cvff.Role]string) (map[cvff.Role]string, error)
	// ResolveReconciliation applies an officer resolution to the
	// RECONCILIATION_REQUIRED branch.
	ResolveReconciliation(ctx context.Context, applicationID string, expectedVersion int64, officerPrincipal string, resolution cvff.ReconciliationResolution) (cvff.Application, error)
}

// Limits carries the approved upload quota and content rules. Every value is
// mandatory: an unset limit is a startup error, never a silent skip.
type Limits struct {
	MaxDocumentBytes           int64
	MaxDocumentsPerApplication int
	DocumentContentTypes       []string
}

func (limits Limits) validate() error {
	if limits.MaxDocumentBytes < 1 || limits.MaxDocumentBytes > 64*1024*1024 {
		return errors.New("max document bytes must be between 1 and 67108864")
	}
	if limits.MaxDocumentsPerApplication < 1 || limits.MaxDocumentsPerApplication > 1000 {
		return errors.New("max documents per application must be between 1 and 1000")
	}
	if len(limits.DocumentContentTypes) == 0 {
		return errors.New("at least one approved document content type is required")
	}
	for _, contentType := range limits.DocumentContentTypes {
		if !contentTypePattern.MatchString(contentType) {
			return fmt.Errorf("document content type %q is not a canonical MIME type", contentType)
		}
	}
	return nil
}

var contentTypePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.+-]*/[a-z0-9][a-z0-9.+-]*$`)

// Handler implements the CVFF contract under /v1/cvff: the beneficiary
// portal routes, the four-party pipeline routes (role assignment, party
// decisions, reconciliation resolution) and the auditor-facing dual-ledger
// report. Every route is gated by realm-role authentication and the PBAC
// policy layer.
type Handler struct {
	store    Store
	blobs    BlobStore
	scanner  Scanner
	limits   Limits
	starter  WorkflowStarter
	signaler WorkflowSignaler
	policy   *pbac.Enforcer
	mux      *http.ServeMux
}

// NewHandler fails closed when any dependency is absent: there is no
// default store, storage backend, scanner, quota, workflow starter,
// workflow signaler or authorization policy.
func NewHandler(store Store, authenticator Authenticator, blobs BlobStore, scanner Scanner, limits Limits, starter WorkflowStarter, signaler WorkflowSignaler, policy *pbac.Enforcer) (*Handler, error) {
	if store == nil {
		return nil, errors.New("cvff store is required")
	}
	if authenticator == nil {
		return nil, errors.New("keycloak authenticator is required")
	}
	if blobs == nil {
		return nil, errors.New("object-storage backend is required")
	}
	if scanner == nil {
		return nil, errors.New("malware scanner is required; uploads are rejected without one")
	}
	if starter == nil {
		return nil, errors.New("workflow starter is required; applications must enter the disbursement rail")
	}
	if signaler == nil {
		return nil, errors.New("workflow signaler is required; party decisions must reach the disbursement rail")
	}
	if policy == nil {
		return nil, errors.New("authorization policy enforcer is required; requests are denied without one")
	}
	if err := limits.validate(); err != nil {
		return nil, err
	}
	handler := &Handler{store: store, blobs: blobs, scanner: scanner, limits: limits, starter: starter, signaler: signaler, policy: policy, mux: http.NewServeMux()}
	api := http.NewServeMux()
	api.Handle("GET /v1/cvff/applications", handler.requirePolicy("cvff.applications", "read", http.HandlerFunc(handler.listApplications)))
	api.Handle("POST /v1/cvff/applications", handler.requirePolicy("cvff.applications", "create", http.HandlerFunc(handler.createApplication)))
	api.Handle("GET /v1/cvff/applications/{application_id}", handler.requirePolicy("cvff.applications", "read", http.HandlerFunc(handler.getApplication)))
	api.Handle("GET /v1/cvff/applications/{application_id}/events", handler.requirePolicy("cvff.applications", "read", http.HandlerFunc(handler.listEvents)))
	api.Handle("GET /v1/cvff/applications/{application_id}/documents", handler.requirePolicy("cvff.applications", "read", http.HandlerFunc(handler.listDocuments)))
	api.Handle("POST /v1/cvff/applications/{application_id}/documents", handler.requirePolicy("cvff.applications", "upload", http.HandlerFunc(handler.uploadDocument)))
	api.Handle("GET /v1/cvff/applications/{application_id}/documents/{document_id}/content", handler.requirePolicy("cvff.applications", "read", http.HandlerFunc(handler.downloadDocument)))
	handler.mux.HandleFunc("GET /healthz", handler.health)
	handler.mux.Handle("GET /v1/cvff/reports/dual-ledger", RequireRole(authenticator, AuditorRole,
		handler.requirePolicy("cvff.reports.dual-ledger", "read", http.HandlerFunc(handler.dualLedgerReport))))
	// Four-party pipeline routes: role assignment and reconciliation are
	// officer routes; decisions are open to any authenticated identity whose
	// realm role the policy admits, with the per-application role binding
	// enforced by the handler and again by the workflow activity.
	handler.mux.Handle("PUT /v1/cvff/admin/applications/{application_id}/roles", RequireRole(authenticator, OfficerRole,
		http.HandlerFunc(handler.assignRoles)))
	handler.mux.Handle("POST /v1/cvff/admin/applications/{application_id}/reconciliation", RequireRole(authenticator, ReconciliationOfficerRole,
		handler.requirePolicy("cvff.application.reconciliation", "resolve", http.HandlerFunc(handler.resolveReconciliation))))
	handler.mux.Handle("POST /v1/cvff/applications/{application_id}/decisions", RequireAuthenticated(authenticator,
		handler.requirePolicy("cvff.application", "decide", http.HandlerFunc(handler.recordDecision))))
	handler.mux.Handle("/", RequireAuth(authenticator, api))
	return handler, nil
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	// Baseline security headers on every response, including healthz and
	// router-level errors.
	header := writer.Header()
	header.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Cache-Control", "no-store")
	handler.mux.ServeHTTP(&problemNormalizingWriter{ResponseWriter: writer}, request)
}

// problemNormalizingWriter converts the ServeMux default text/plain 404/405
// responses to the RFC 9457 problem+json shape every application route uses,
// so clients see one error contract. Application responses (which always set
// an application/* content type before WriteHeader) pass through untouched.
type problemNormalizingWriter struct {
	http.ResponseWriter
	normalized bool
}

func (writer *problemNormalizingWriter) WriteHeader(status int) {
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		if contentType := writer.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/") {
			writer.normalized = true
			title := "The requested resource was not found."
			if status == http.StatusMethodNotAllowed {
				title = "The method is not allowed for this resource."
			}
			writeProblem(writer.ResponseWriter, status, title, nil)
			return
		}
	}
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *problemNormalizingWriter) Write(content []byte) (int, error) {
	if writer.normalized {
		// The mux default body is discarded; the problem document was
		// already written by WriteHeader.
		return len(content), nil
	}
	return writer.ResponseWriter.Write(content)
}

func (handler *Handler) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

// Response shapes. Field names are bound to the beneficiary-portal and
// mobile TypeScript contracts; do not rename without a coordinated client
// change.

type applicationSummaryDTO struct {
	ApplicationID  string     `json:"application_id"`
	VesselName     string     `json:"vessel_name"`
	Amount         uint64     `json:"amount"`
	Currency       string     `json:"currency"`
	State          cvff.State `json:"state"`
	StateEnteredAt time.Time  `json:"state_entered_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type applicationDetailDTO struct {
	applicationSummaryDTO
	IMONumber        string `json:"imo_number"`
	OfficialNumber   string `json:"official_number"`
	VesselClass      string `json:"vessel_class"`
	CabotageRoute    string `json:"cabotage_route"`
	BusinessName     string `json:"business_name"`
	BusinessRCNumber string `json:"business_rc_number"`
	BusinessAddress  string `json:"business_address"`
}

type documentDTO struct {
	DocumentID    string    `json:"document_id"`
	ApplicationID string    `json:"application_id"`
	DocumentType  string    `json:"document_type"`
	FileName      string    `json:"file_name"`
	ContentType   string    `json:"content_type"`
	SizeBytes     int64     `json:"size_bytes"`
	UploadedAt    time.Time `json:"uploaded_at"`
}

func summaryOf(application cvff.ApplicationDetail) applicationSummaryDTO {
	return applicationSummaryDTO{
		ApplicationID:  application.ApplicationID,
		VesselName:     application.VesselName,
		Amount:         application.Amount,
		Currency:       application.Currency,
		State:          application.State,
		StateEnteredAt: application.StateEnteredAt.UTC(),
		CreatedAt:      application.CreatedAt.UTC(),
		UpdatedAt:      application.UpdatedAt.UTC(),
	}
}

func detailOf(application cvff.ApplicationDetail) applicationDetailDTO {
	return applicationDetailDTO{
		applicationSummaryDTO: summaryOf(application),
		IMONumber:             application.IMONumber,
		OfficialNumber:        application.OfficialNumber,
		VesselClass:           application.VesselClass,
		CabotageRoute:         application.CabotageRoute,
		BusinessName:          application.BusinessName,
		BusinessRCNumber:      application.BusinessRCNumber,
		BusinessAddress:       application.BusinessAddress,
	}
}

func documentOf(document cvff.Document) documentDTO {
	return documentDTO{
		DocumentID:    document.DocumentID,
		ApplicationID: document.ApplicationID,
		DocumentType:  document.DocumentType,
		FileName:      document.FileName,
		ContentType:   document.ContentType,
		SizeBytes:     document.SizeBytes,
		UploadedAt:    document.CreatedAt.UTC(),
	}
}

func (handler *Handler) listApplications(writer http.ResponseWriter, request *http.Request) {
	principal := principalFrom(request.Context())
	applications, err := handler.store.ListForBeneficiary(request.Context(), principal.Subject)
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "The applications could not be listed.", nil)
		return
	}
	summaries := make([]applicationSummaryDTO, 0, len(applications))
	for _, application := range applications {
		summaries = append(summaries, summaryOf(application))
	}
	writeJSON(writer, http.StatusOK, summaries)
}

type createApplicationRequest struct {
	VesselName       string `json:"vessel_name"`
	IMONumber        string `json:"imo_number"`
	OfficialNumber   string `json:"official_number"`
	VesselClass      string `json:"vessel_class"`
	CabotageRoute    string `json:"cabotage_route"`
	Amount           uint64 `json:"amount"`
	Currency         string `json:"currency"`
	BusinessName     string `json:"business_name"`
	BusinessRCNumber string `json:"business_rc_number"`
	BusinessAddress  string `json:"business_address"`
}

func (handler *Handler) createApplication(writer http.ResponseWriter, request *http.Request) {
	principal := principalFrom(request.Context())
	var payload createApplicationRequest
	if err := decodeJSON(request, &payload); err != nil {
		writeProblem(writer, http.StatusUnprocessableEntity, "The request body is not a valid application payload.", nil)
		return
	}
	intake := cvff.Intake{
		ApplicationID:    uuid.NewString(),
		IdempotencyKey:   strings.TrimSpace(request.Header.Get("Idempotency-Key")),
		BeneficiaryID:    principal.Subject,
		VesselName:       strings.TrimSpace(payload.VesselName),
		IMONumber:        strings.TrimSpace(payload.IMONumber),
		OfficialNumber:   strings.TrimSpace(payload.OfficialNumber),
		VesselClass:      payload.VesselClass,
		CabotageRoute:    payload.CabotageRoute,
		Amount:           payload.Amount,
		Currency:         payload.Currency,
		BusinessName:     strings.TrimSpace(payload.BusinessName),
		BusinessRCNumber: strings.ToUpper(strings.TrimSpace(payload.BusinessRCNumber)),
		BusinessAddress:  strings.TrimSpace(payload.BusinessAddress),
	}
	if errs := intake.Validate(); len(errs) > 0 {
		writeProblem(writer, http.StatusUnprocessableEntity, "The application was rejected; correct the highlighted fields and resubmit with the same Idempotency-Key.", fieldErrorMap(errs))
		return
	}
	retained, err := handler.store.SubmitIntake(request.Context(), intake)
	if err != nil {
		var fieldErrs cvff.FieldErrors
		switch {
		case errors.As(err, &fieldErrs):
			writeProblem(writer, http.StatusUnprocessableEntity, "The application was rejected; correct the highlighted fields and resubmit with the same Idempotency-Key.", fieldErrorMap(fieldErrs))
		case errors.Is(err, cvff.ErrConflict):
			writeProblem(writer, http.StatusConflict, "This Idempotency-Key was already used with different content.", nil)
		default:
			writeProblem(writer, http.StatusInternalServerError, "The application could not be recorded.", nil)
		}
		return
	}
	// Enter the disbursement rail. The starter is idempotent (an already
	// running workflow for the application is success), so idempotent intake
	// replays also re-drive the start: a submission that failed to start its
	// workflow heals on the client's retry with the same Idempotency-Key.
	if err := handler.starter.StartDisbursement(request.Context(), retained.ApplicationID); err != nil {
		writeProblem(writer, http.StatusServiceUnavailable, "The application was recorded but the disbursement workflow could not be started; retry with the same Idempotency-Key.", nil)
		return
	}
	writeJSON(writer, http.StatusCreated, detailOf(retained))
}

// fieldErrorMap flattens per-field rejections to the problem `errors` object
// the portal wizard maps back onto its draft fields.
func fieldErrorMap(errs cvff.FieldErrors) map[string]string {
	mapped := make(map[string]string, len(errs))
	for _, fieldError := range errs {
		mapped[fieldError.Field] = fieldError.Message
	}
	return mapped
}

// ownedApplication resolves one application for the caller, failing closed
// with 404 for missing and for other owners' applications alike.
func (handler *Handler) ownedApplication(writer http.ResponseWriter, request *http.Request) (cvff.ApplicationDetail, bool) {
	applicationID := request.PathValue("application_id")
	if err := cvff.ValidateIdentifier("application_id", applicationID); err != nil {
		writeProblem(writer, http.StatusNotFound, "The application was not found.", nil)
		return cvff.ApplicationDetail{}, false
	}
	principal := principalFrom(request.Context())
	application, err := handler.store.GetForBeneficiary(request.Context(), applicationID, principal.Subject)
	if err != nil {
		if errors.Is(err, cvff.ErrNotFound) {
			writeProblem(writer, http.StatusNotFound, "The application was not found.", nil)
			return cvff.ApplicationDetail{}, false
		}
		writeProblem(writer, http.StatusInternalServerError, "The application could not be loaded.", nil)
		return cvff.ApplicationDetail{}, false
	}
	return application, true
}

func (handler *Handler) getApplication(writer http.ResponseWriter, request *http.Request) {
	application, ok := handler.ownedApplication(writer, request)
	if !ok {
		return
	}
	writeJSON(writer, http.StatusOK, detailOf(application))
}

// listEvents returns the immutable decision trail for one owned application:
// one entry per recorded party decision, in recording order.
func (handler *Handler) listEvents(writer http.ResponseWriter, request *http.Request) {
	application, ok := handler.ownedApplication(writer, request)
	if !ok {
		return
	}
	approvals, err := handler.store.ListApprovals(request.Context(), application.ApplicationID)
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "The decision trail could not be loaded.", nil)
		return
	}
	writeJSON(writer, http.StatusOK, approvals)
}

func (handler *Handler) listDocuments(writer http.ResponseWriter, request *http.Request) {
	application, ok := handler.ownedApplication(writer, request)
	if !ok {
		return
	}
	documents, err := handler.store.ListDocuments(request.Context(), application.ApplicationID)
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "The documents could not be listed.", nil)
		return
	}
	result := make([]documentDTO, 0, len(documents))
	for _, document := range documents {
		result = append(result, documentOf(document))
	}
	writeJSON(writer, http.StatusOK, result)
}

// downloadDocument streams one stored document body. Authorization is the
// parent application's: ownedApplication fails closed with 404 for other
// beneficiaries' applications, and an unknown document id under an owned
// application is likewise a 404 — never a 403 ownership leak. There is no
// DELETE by design: documents are an immutable audit trail.
func (handler *Handler) downloadDocument(writer http.ResponseWriter, request *http.Request) {
	application, ok := handler.ownedApplication(writer, request)
	if !ok {
		return
	}
	documentID := request.PathValue("document_id")
	documents, err := handler.store.ListDocuments(request.Context(), application.ApplicationID)
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "The document could not be loaded.", nil)
		return
	}
	var document *cvff.Document
	for index := range documents {
		if documents[index].DocumentID == documentID {
			document = &documents[index]
			break
		}
	}
	if document == nil {
		writeProblem(writer, http.StatusNotFound, "The document was not found.", nil)
		return
	}
	// Defence in depth: the stored metadata row must still reference the
	// owning application's content-addressed namespace.
	if !strings.HasPrefix(document.StorageKey, "cvff-documents/"+application.ApplicationID+"/") {
		writeProblem(writer, http.StatusNotFound, "The document was not found.", nil)
		return
	}
	object, err := handler.blobs.Get(request.Context(), document.StorageKey)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			writeProblem(writer, http.StatusNotFound, "The document was not found.", nil)
			return
		}
		writeProblem(writer, http.StatusBadGateway, "The document content could not be read; retry later.", nil)
		return
	}
	defer object.Close()
	contentType := document.ContentType
	if parsed, _, err := mime.ParseMediaType(contentType); err != nil || parsed == "" {
		contentType = "application/octet-stream"
	}
	writer.Header().Set("Content-Type", contentType)
	writer.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(strings.ReplaceAll(document.FileName, "\\", "/"))}))
	writer.Header().Set("Content-Length", strconv.FormatInt(document.SizeBytes, 10))
	writer.WriteHeader(http.StatusOK)
	_, _ = io.Copy(writer, object)
}

// uploadDocument stores one supporting document: ownership check, approved
// type and quota enforcement, mandatory malware scan, content-addressed
// object put, then the metadata row with idempotent replay.
func (handler *Handler) uploadDocument(writer http.ResponseWriter, request *http.Request) {
	application, ok := handler.ownedApplication(writer, request)
	if !ok {
		return
	}
	principal := principalFrom(request.Context())
	idempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if err := cvff.ValidateIdentifier("idempotency_key", idempotencyKey); err != nil {
		writeProblem(writer, http.StatusUnprocessableEntity, "A canonical Idempotency-Key header is required for uploads.", nil)
		return
	}
	// Multipart overhead allowance above the approved byte ceiling.
	request.Body = http.MaxBytesReader(writer, request.Body, handler.limits.MaxDocumentBytes+(1<<20))
	reader, err := request.MultipartReader()
	if err != nil {
		writeProblem(writer, http.StatusUnprocessableEntity, "The request is not a valid multipart upload.", nil)
		return
	}
	documentType := ""
	var content []byte
	fileName := ""
	partContentType := ""
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeProblem(writer, http.StatusUnprocessableEntity, "The multipart upload could not be read.", nil)
			return
		}
		switch part.FormName() {
		case "document_type":
			value, _ := io.ReadAll(io.LimitReader(part, 256))
			documentType = strings.TrimSpace(string(value))
		case "file":
			fileName = part.FileName()
			partContentType = part.Header.Get("Content-Type")
			content, err = io.ReadAll(part)
			if err != nil {
				writeProblem(writer, http.StatusRequestEntityTooLarge, "The file exceeds the approved size limit.", nil)
				return
			}
		default:
			_, _ = io.Copy(io.Discard, io.LimitReader(part, 4096))
		}
		part.Close()
	}
	if documentType == "" || content == nil {
		writeProblem(writer, http.StatusUnprocessableEntity, "The upload must contain document_type and file parts.", nil)
		return
	}
	switch documentType {
	case cvff.DocumentTypeVesselRegistration, cvff.DocumentTypeCabotageLicense, cvff.DocumentTypeBankDetails:
	default:
		writeProblem(writer, http.StatusUnprocessableEntity, "The document_type is not an approved CVFF document type.", nil)
		return
	}
	if len(content) == 0 {
		writeProblem(writer, http.StatusUnprocessableEntity, "The selected file is empty.", nil)
		return
	}
	if int64(len(content)) > handler.limits.MaxDocumentBytes {
		writeProblem(writer, http.StatusUnprocessableEntity, fmt.Sprintf("The file exceeds the approved %.1f MB limit.", float64(handler.limits.MaxDocumentBytes)/(1024*1024)), nil)
		return
	}
	if !handler.contentTypeApproved(partContentType) {
		writeProblem(writer, http.StatusUnprocessableEntity, "Files of this type are not accepted.", nil)
		return
	}
	// The declared content type is client input and proves nothing: the
	// bytes themselves must match it, and known-dangerous signatures are
	// rejected regardless of what the client declared.
	if dangerous := dangerousContentSignature(content); dangerous != "" {
		writeProblem(writer, http.StatusUnprocessableEntity, "The file content is a "+dangerous+"; executable and script content is never accepted.", nil)
		return
	}
	parsedType, _, _ := mime.ParseMediaType(partContentType)
	if !contentMatchesDeclaredType(parsedType, content) {
		writeProblem(writer, http.StatusUnprocessableEntity, "The file content does not match the declared type "+parsedType+".", nil)
		return
	}
	documents, err := handler.store.ListDocuments(request.Context(), application.ApplicationID)
	if err != nil {
		writeProblem(writer, http.StatusInternalServerError, "The document quota could not be verified.", nil)
		return
	}
	// Idempotent replay: the same key with the same content returns the
	// original record; the same key with different content is a hard
	// conflict. Replay is resolved before quota and scanning.
	digest := sha256.Sum256(content)
	digestHex := hex.EncodeToString(digest[:])
	for _, existing := range documents {
		if existing.IdempotencyKey != idempotencyKey {
			continue
		}
		if existing.SHA256Hex == digestHex && existing.DocumentType == documentType {
			writeJSON(writer, http.StatusOK, documentOf(existing))
			return
		}
		writeProblem(writer, http.StatusConflict, "This Idempotency-Key was already used with different content.", nil)
		return
	}
	if len(documents) >= handler.limits.MaxDocumentsPerApplication {
		writeProblem(writer, http.StatusConflict, "The application has reached the approved document quota.", nil)
		return
	}
	if err := handler.scanner.Scan(request.Context(), fileName, content); err != nil {
		if errors.Is(err, ErrDocumentInfected) {
			writeProblem(writer, http.StatusUnprocessableEntity, "The document was rejected by the malware scanner.", nil)
			return
		}
		writeProblem(writer, http.StatusServiceUnavailable, "The malware scanner is unavailable; the upload was refused. Retry later with the same file.", nil)
		return
	}
	document := cvff.Document{
		ApplicationID:  application.ApplicationID,
		BeneficiaryID:  principal.Subject,
		DocumentType:   documentType,
		FileName:       fileName,
		ContentType:    partContentType,
		SizeBytes:      int64(len(content)),
		SHA256Hex:      digestHex,
		StorageBackend: handler.blobs.Backend(),
		StorageKey:     DocumentStorageKey(application.ApplicationID, digest),
		IdempotencyKey: idempotencyKey,
	}
	if err := document.Validate(); err != nil {
		writeProblem(writer, http.StatusUnprocessableEntity, "The document metadata is outside the approved contract.", nil)
		return
	}
	if err := handler.blobs.Put(request.Context(), document.StorageKey, document.ContentType, strings.NewReader(string(content)), document.SizeBytes); err != nil {
		writeProblem(writer, http.StatusBadGateway, "The document could not be stored; retry with the same file and Idempotency-Key.", nil)
		return
	}
	retained, err := handler.store.CreateDocument(request.Context(), document)
	if err != nil {
		switch {
		case errors.Is(err, cvff.ErrConflict):
			writeProblem(writer, http.StatusConflict, "This Idempotency-Key was already used with different content.", nil)
		case errors.Is(err, cvff.ErrIntakeInvalid):
			writeProblem(writer, http.StatusUnprocessableEntity, "The document metadata is outside the approved contract.", nil)
		default:
			writeProblem(writer, http.StatusInternalServerError, "The document could not be recorded.", nil)
		}
		return
	}
	writeJSON(writer, http.StatusCreated, documentOf(retained))
}

// knownSignatures binds the approved content types to their mandatory magic
// prefixes. A listed type is only accepted when the bytes carry its
// signature.
var knownSignatures = map[string][][]byte{
	"application/pdf": {[]byte("%PDF-")},
	"image/png":       {[]byte("\x89PNG\r\n\x1a\n")},
	"image/jpeg":      {[]byte("\xFF\xD8\xFF")},
}

// dangerousPrefixes are executable and script signatures that are rejected
// no matter which content type the client declared.
var dangerousPrefixes = []struct {
	name   string
	prefix []byte
}{
	{"Windows executable (MZ)", []byte("MZ")},
	{"ELF executable", []byte("\x7FELF")},
	{"script (shebang)", []byte("#!")},
	{"Mach-O executable (32-bit)", []byte("\xFE\xED\xFA\xCE")},
	{"Mach-O executable (64-bit)", []byte("\xFE\xED\xFA\xCF")},
	{"Mach-O executable (reverse 32-bit)", []byte("\xCE\xFA\xED\xFE")},
	{"Mach-O executable (reverse 64-bit)", []byte("\xCF\xFA\xED\xFE")},
}

// dangerousContentSignature names the matched dangerous signature, or "".
func dangerousContentSignature(content []byte) string {
	for _, dangerous := range dangerousPrefixes {
		if bytes.HasPrefix(content, dangerous.prefix) {
			return dangerous.name
		}
	}
	return ""
}

// contentMatchesDeclaredType verifies the magic bytes against the declared
// type. Types without a known signature in the approved set cannot be
// byte-verified; the dangerous-signature screen above still applies.
func contentMatchesDeclaredType(declaredType string, content []byte) bool {
	signatures, known := knownSignatures[declaredType]
	if !known {
		return true
	}
	for _, signature := range signatures {
		if bytes.HasPrefix(content, signature) {
			return true
		}
	}
	return false
}

func (handler *Handler) contentTypeApproved(contentType string) bool {
	parsed, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	for _, approved := range handler.limits.DocumentContentTypes {
		if parsed == approved {
			return true
		}
	}
	return false
}

// problemDocument is the RFC 9457 problem shape. `errors` carries per-field
// rejections keyed by payload field name, which the portal wizard maps back
// onto its draft fields.
type problemDocument struct {
	Type   string            `json:"type"`
	Title  string            `json:"title"`
	Status int               `json:"status"`
	Errors map[string]string `json:"errors,omitempty"`
}

func writeProblem(writer http.ResponseWriter, status int, title string, fieldErrors map[string]string) {
	writer.Header().Set("Content-Type", "application/problem+json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(problemDocument{
		Type:   "about:blank",
		Title:  title,
		Status: status,
		Errors: fieldErrors,
	})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func decodeJSON(request *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(nil, request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("request body is not valid contract JSON")
	}
	if decoder.More() {
		return errors.New("request body must contain exactly one JSON document")
	}
	return nil
}
