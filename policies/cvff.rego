# CVFF policy pack: route authorization, four-party segregation of duties and
# classification-based clearance gating for the financial-controls API.
# Deny by default; every allow path requires role, clearance and tenant checks.
package cvff

import rego.v1

default allow := false

# Platform classification ladder (envelope v1.0 classification enum).
classification_rank := {
	"PUBLIC": 0,
	"INTERNAL": 1,
	"CONFIDENTIAL": 2,
	"RESTRICTED": 3,
	"FIDUCIARY_SEGREGATED": 4,
}

# Realm roles that operate on fiduciary CVFF data carry an implicit
# FIDUCIARY_SEGREGATED clearance floor. A token may assert an explicit
# clearance claim; the higher of the two governs.
role_clearance := {
	"beneficiary": "FIDUCIARY_SEGREGATED",
	"auditor": "FIDUCIARY_SEGREGATED",
	"cvff-officer": "FIDUCIARY_SEGREGATED",
	"reconciliation-officer": "FIDUCIARY_SEGREGATED",
	"underwriter": "FIDUCIARY_SEGREGATED",
	"nimasa-approver": "FIDUCIARY_SEGREGATED",
	"receiving-bank": "FIDUCIARY_SEGREGATED",
	"intent-maker": "FIDUCIARY_SEGREGATED",
	"intent-checker": "FIDUCIARY_SEGREGATED",
	"financial-controller": "FIDUCIARY_SEGREGATED",
}

principal_clearances contains clearance if {
	some role in input.principal.roles
	clearance := role_clearance[role]
}

principal_clearances contains clearance if {
	clearance := input.principal.clearance
	classification_rank[clearance]
}

# The governing clearance is the highest-ranked of the principal's
# clearances (rank order, not lexicographic).
effective_clearance := clearance if {
	some clearance in principal_clearances
	classification_rank[clearance] == max({classification_rank[candidate] | some candidate in principal_clearances})
}

# Classification gating: the principal's effective clearance must dominate the
# resource classification. An unknown classification or a principal with no
# recognized clearance leaves this undefined, which denies.
clearance_ok if {
	classification_rank[effective_clearance] >= classification_rank[input.classification]
}

# Tenant binding: the CVFF API is single-tenant, so an asserted tenant must
# match the principal's tenant; cross-tenant access denies.
tenant_ok if {
	input.tenant_id == ""
}

tenant_ok if {
	input.tenant_id != ""
	input.tenant_id == input.principal.tenant_id
}

has_role(role) if {
	some held in input.principal.roles
	held == role
}

has_any_role(roles) if {
	some role in roles
	has_role(role)
}

allow if {
	clearance_ok
	tenant_ok
	route_ok
}

# Beneficiary self-service: own applications, events and document uploads.
route_ok if {
	input.resource == "cvff.applications"
	input.action in {"read", "create", "upload"}
	has_role("beneficiary")
}

# Four-party decisions: any chain party may signal; the per-application role
# binding (who decides the current state) is enforced against the durable
# role assignments by the API and again by the workflow activity.
route_ok if {
	input.resource == "cvff.application"
	input.action == "decide"
	has_any_role(["beneficiary", "underwriter", "nimasa-approver", "receiving-bank"])
}

# Role assignment: cvff officers only, and the proposed binding must satisfy
# the four-party segregation of duties.
route_ok if {
	input.resource == "cvff.application.roles"
	input.action == "assign"
	has_role("cvff-officer")
	assignments_ok
}

# Reconciliation resolution: reconciliation officers only; the officer must
# additionally not be a chain party on the application (enforced against the
# durable assignments by the API/store layer).
route_ok if {
	input.resource == "cvff.application.reconciliation"
	input.action == "resolve"
	has_role("reconciliation-officer")
}

# Auditor dual-ledger report: read-only auditor role.
route_ok if {
	input.resource == "cvff.reports.dual-ledger"
	input.action == "read"
	has_role("auditor")
}

# Financial intents (TigerBeetle money movement): the maker creates, a
# distinct checker approves (maker != checker is enforced server-side against
# the verified token subjects, never against body fields), the maker may void
# only own DRAFT intents, and only a financial controller may resolve an
# AMBIGUOUS intent.
route_ok if {
	input.resource == "financial.intents"
	input.action == "create"
	has_role("intent-maker")
}

route_ok if {
	input.resource == "financial.intents"
	input.action == "approve"
	has_role("intent-checker")
}

route_ok if {
	input.resource == "financial.intents"
	input.action == "void"
	has_role("intent-maker")
}

route_ok if {
	input.resource == "financial.intents"
	input.action == "resolve"
	has_role("financial-controller")
}

# Four-party segregation of duties: every chain role is bound exactly once,
# the six bindings are six distinct principals (proposer != approver !=
# disburser != beneficiary on the same application), and the assigning
# officer is never one of the parties.
chain_roles := [
	"UNDERWRITER_PRIMARY",
	"UNDERWRITER_SECONDARY",
	"UNDERWRITER_TERTIARY",
	"NIMASA_APPROVER",
	"RECEIVING_BANK",
	"BENEFICIARY",
]

assignments_ok if {
	principals := [input.assignments[role] | some role in chain_roles]
	count(principals) == count(chain_roles)
	count({principal | some principal in principals}) == count(chain_roles)
	not input.principal.subject in principals
}
