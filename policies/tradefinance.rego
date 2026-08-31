# WP-6 trade-finance consent-scope enforcement (NTP TFC pattern).
# The bank-facing data-sharing surface is fail-closed: a bank reads a
# dataset only when an ACTIVE, unexpired consent covers exactly
# (trader, bank, scope). Every other combination is denied.
package tradefinance

import rego.v1

default allow := false

# Consented scopes are the only shareable dataset classes.
approved_scopes := {"DECLARATION_DIGESTS", "DUTY_PAYMENT_HISTORY", "TAX_STAMP_STATUS"}

allow if {
	input.bank_id == input.consent.bank_id
	input.trader_id == input.consent.trader_id
	input.consent.state == "ACTIVE"
	input.scope in approved_scopes
	input.scope in input.consent.scopes
	time.parse_rfc3339_ns(input.consent.expires_at) > time.parse_rfc3339_ns(input.now)
	# The consent must carry digest evidence for the requested scope.
	count(object.get(input.consent.dataset_refs, input.scope, [])) > 0
}

deny_reason := "bank is not the consent grantee" if {
	input.bank_id != input.consent.bank_id
}

deny_reason := "consent is not active" if {
	input.consent.state != "ACTIVE"
}

deny_reason := "scope is not consented" if {
	not input.scope in input.consent.scopes
}

deny_reason := "scope is not an approved dataset class" if {
	not input.scope in approved_scopes
}

deny_reason := "consent is expired" if {
	time.parse_rfc3339_ns(input.consent.expires_at) <= time.parse_rfc3339_ns(input.now)
}
