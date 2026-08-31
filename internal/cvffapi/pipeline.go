package cvffapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvff"
	"github.com/munisp/blueeconomy-financial-controls/internal/pbac"
	"github.com/munisp/blueeconomy-financial-controls/internal/workflow"
)

// ClassificationFiduciarySegregated is the envelope classification every CVFF
// route serves; the PBAC policy gates it against the caller's clearance.
const ClassificationFiduciarySegregated = "FIDUCIARY_SEGREGATED"

// principalPolicyInput builds the PBAC evaluation input for the caller.
func principalPolicyInput(principal Principal, resource, action string) pbac.Input {
	return pbac.Input{
		Principal: pbac.Principal{
			Subject:   principal.Subject,
			Roles:     principal.Roles,
			Clearance: principal.Clearance,
			TenantID:  principal.TenantID,
		},
		TenantID:       principal.TenantID,
		Resource:       resource,
		Action:         action,
		Classification: ClassificationFiduciarySegregated,
	}
}

// requirePolicy wraps one route with the PBAC authorization decision.
// Deny-by-default: a denied or unevaluable policy result maps to 403 and the
// response never leaks policy detail.
func (handler *Handler) requirePolicy(resource, action string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		principal := principalFrom(request.Context())
		if !handler.policy.Allow(request.Context(), principalPolicyInput(principal, resource, action)) {
			writeProblem(writer, http.StatusForbidden, "The request was denied by the authorization policy.", nil)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

type assignRolesRequest struct {
	Assignments map[string]string `json:"assignments"`
}

// assignRoles binds the four-party chain principals to one existing
// application (PUT /v1/cvff/admin/applications/{id}/roles). The PBAC policy
// enforces segregation of duties over the proposed binding; the store write
// is idempotent (identical replay succeeds, divergence conflicts) and the
// disbursement workflow is (re-)started on the deterministic workflow ID.
func (handler *Handler) assignRoles(writer http.ResponseWriter, request *http.Request) {
	principal := principalFrom(request.Context())
	applicationID := request.PathValue("application_id")
	if err := cvff.ValidateIdentifier("application_id", applicationID); err != nil {
		writeProblem(writer, http.StatusNotFound, "The application was not found.", nil)
		return
	}
	var payload assignRolesRequest
	if err := decodeJSON(request, &payload); err != nil {
		writeProblem(writer, http.StatusUnprocessableEntity, "The request body is not a valid role-assignment payload.", nil)
		return
	}
	assignments := make(map[cvff.Role]string, len(payload.Assignments))
	for role, party := range payload.Assignments {
		assignments[cvff.Role(strings.ToUpper(strings.TrimSpace(role)))] = strings.TrimSpace(party)
	}
	if err := cvff.ValidateRoleAssignments(assignments); err != nil {
		writeProblem(writer, http.StatusUnprocessableEntity, "The assignments must bind each chain role to a distinct principal.", nil)
		return
	}
	for _, party := range assignments {
		if err := cvff.ValidateIdentifier("principal_id", party); err != nil {
			writeProblem(writer, http.StatusUnprocessableEntity, "Every assigned principal must be canonical identifier text.", nil)
			return
		}
	}
	// Policy evaluation with the full proposed binding: segregation of duties
	// (six distinct parties, officer never a party) is enforced declaratively.
	input := principalPolicyInput(principal, "cvff.application.roles", "assign")
	input.Assignments = make(map[string]string, len(assignments))
	for role, party := range assignments {
		input.Assignments[string(role)] = party
	}
	if !handler.policy.Allow(request.Context(), input) {
		writeProblem(writer, http.StatusForbidden, "The request was denied by the authorization policy.", nil)
		return
	}
	retained, err := handler.store.AssignRoles(request.Context(), applicationID, principal.Subject, assignments)
	if err != nil {
		switch {
		case errors.Is(err, cvff.ErrNotFound):
			writeProblem(writer, http.StatusNotFound, "The application was not found.", nil)
		case errors.Is(err, cvff.ErrRoleSeparation), errors.Is(err, cvff.ErrRoleAssignmentInvalid):
			writeProblem(writer, http.StatusUnprocessableEntity, "The assignments violate the four-party segregation of duties.", nil)
		case errors.Is(err, cvff.ErrConflict):
			writeProblem(writer, http.StatusConflict, "The application already has divergent role assignments.", nil)
		case errors.Is(err, cvff.ErrTerminalState):
			writeProblem(writer, http.StatusConflict, "The application is closed.", nil)
		default:
			writeProblem(writer, http.StatusInternalServerError, "The role assignments could not be recorded.", nil)
		}
		return
	}
	// Enter or re-drive the disbursement rail on the deterministic workflow
	// ID. Idempotent: an already-running workflow is success, so replayed
	// assignments heal a lost start.
	if err := handler.starter.StartDisbursement(request.Context(), applicationID); err != nil {
		writeProblem(writer, http.StatusServiceUnavailable, "The roles were assigned but the disbursement workflow could not be started; retry the same request.", nil)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"application_id": applicationID, "assignments": retained})
}

type recordDecisionRequest struct {
	Decision string `json:"decision"`
}

// decisionSignalForState maps a decision state to the workflow signal that
// carries its party decision.
func decisionSignalForState(state cvff.State) (string, bool) {
	switch state {
	case cvff.StateUnderwritingPrimary, cvff.StateUnderwritingSecondary, cvff.StateUnderwritingTertiary:
		return workflow.SignalUnderwritingDecision, true
	case cvff.StateNIMASAApproval:
		return workflow.SignalNIMASADecision, true
	case cvff.StateBankConfirmation:
		return workflow.SignalBankConfirmation, true
	case cvff.StateDisbursementPending:
		return workflow.SignalBeneficiaryConfirmation, true
	default:
		return "", false
	}
}

// recordDecision delivers one party decision to the application's
// disbursement workflow (POST /v1/cvff/applications/{id}/decisions). The
// caller must be the principal durably bound to the role the current state
// awaits; the workflow activity re-enforces the same binding against the
// database, so a replayed or forged signal cannot advance the chain.
func (handler *Handler) recordDecision(writer http.ResponseWriter, request *http.Request) {
	principal := principalFrom(request.Context())
	applicationID := request.PathValue("application_id")
	if err := cvff.ValidateIdentifier("application_id", applicationID); err != nil {
		writeProblem(writer, http.StatusNotFound, "The application was not found.", nil)
		return
	}
	var payload recordDecisionRequest
	if err := decodeJSON(request, &payload); err != nil {
		writeProblem(writer, http.StatusUnprocessableEntity, "The request body is not a valid decision payload.", nil)
		return
	}
	decision := cvff.Decision(strings.ToUpper(strings.TrimSpace(payload.Decision)))
	if decision != cvff.DecisionApprove && decision != cvff.DecisionReject {
		writeProblem(writer, http.StatusUnprocessableEntity, "The decision must be APPROVE or REJECT.", nil)
		return
	}
	application, err := handler.store.Get(request.Context(), applicationID)
	if err != nil {
		if errors.Is(err, cvff.ErrNotFound) {
			writeProblem(writer, http.StatusNotFound, "The application was not found.", nil)
			return
		}
		writeProblem(writer, http.StatusInternalServerError, "The application could not be loaded.", nil)
		return
	}
	signalName, ok := decisionSignalForState(application.State)
	if !ok {
		writeProblem(writer, http.StatusConflict, "The application is not awaiting a party decision.", nil)
		return
	}
	required, _ := cvff.RequiredRoleForState(application.State)
	assignments, err := handler.store.RoleAssignments(request.Context(), applicationID)
	if err != nil {
		if errors.Is(err, cvff.ErrNotFound) {
			writeProblem(writer, http.StatusConflict, "The four-party roles have not been assigned yet.", nil)
			return
		}
		writeProblem(writer, http.StatusInternalServerError, "The role assignments could not be loaded.", nil)
		return
	}
	if assignments[required] != principal.Subject {
		writeProblem(writer, http.StatusForbidden, "The authenticated identity is not the assigned party for the current state.", nil)
		return
	}
	signal := workflow.DecisionSignal{PrincipalID: principal.Subject, Decision: decision}
	if err := handler.signaler.SignalDecision(request.Context(), applicationID, signalName, signal); err != nil {
		writeProblem(writer, http.StatusServiceUnavailable, "The decision could not be delivered to the disbursement workflow; retry the same request.", nil)
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{"application_id": applicationID, "state": application.State, "signal": signalName})
}

type resolveReconciliationRequest struct {
	Resolution string `json:"resolution"`
}

// resolveReconciliation delivers a reconciliation officer's resolution for an
// application parked in RECONCILIATION_REQUIRED
// (POST /v1/cvff/admin/applications/{id}/reconciliation). The state write is
// performed by the workflow's resolution activity against the database
// (single writer); the API validates, authorizes and signals.
func (handler *Handler) resolveReconciliation(writer http.ResponseWriter, request *http.Request) {
	principal := principalFrom(request.Context())
	applicationID := request.PathValue("application_id")
	if err := cvff.ValidateIdentifier("application_id", applicationID); err != nil {
		writeProblem(writer, http.StatusNotFound, "The application was not found.", nil)
		return
	}
	var payload resolveReconciliationRequest
	if err := decodeJSON(request, &payload); err != nil {
		writeProblem(writer, http.StatusUnprocessableEntity, "The request body is not a valid resolution payload.", nil)
		return
	}
	resolution := cvff.ReconciliationResolution(strings.ToUpper(strings.TrimSpace(payload.Resolution)))
	if resolution != cvff.ResolutionResumeDisbursement && resolution != cvff.ResolutionReject {
		writeProblem(writer, http.StatusUnprocessableEntity, "The resolution must be RESUME_DISBURSEMENT or REJECT.", nil)
		return
	}
	application, err := handler.store.Get(request.Context(), applicationID)
	if err != nil {
		if errors.Is(err, cvff.ErrNotFound) {
			writeProblem(writer, http.StatusNotFound, "The application was not found.", nil)
			return
		}
		writeProblem(writer, http.StatusInternalServerError, "The application could not be loaded.", nil)
		return
	}
	if application.State != cvff.StateReconciliationRequired {
		writeProblem(writer, http.StatusConflict, "The application is not awaiting reconciliation resolution.", nil)
		return
	}
	signal := workflow.ResolutionSignal{PrincipalID: principal.Subject, Resolution: resolution}
	if err := handler.signaler.SignalResolution(request.Context(), applicationID, signal); err != nil {
		if errors.Is(err, ErrWorkflowNotRunning) {
			writeProblem(writer, http.StatusServiceUnavailable, "The disbursement workflow is not parked for this application; escalate to operations.", nil)
			return
		}
		writeProblem(writer, http.StatusServiceUnavailable, "The resolution could not be delivered to the disbursement workflow; retry the same request.", nil)
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{"application_id": applicationID, "state": application.State, "resolution": resolution})
}
