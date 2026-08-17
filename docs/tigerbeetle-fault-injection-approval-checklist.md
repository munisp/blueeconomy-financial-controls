# TigerBeetle Fault-Injection Approval Checklist

A test may proceed only when all eight stages are marked `approved` and include a non-empty evidence reference.

1. **Submission** — scope, cases, expected outcomes and rollback plan approved by the S3 technical owner.
2. **Security** — least-privilege access, fault-command allowlist, audit retention and time-bound access approved by Ministry security.
3. **Finance control** — test-only accounts, amount limits and independent reconciliation reviewer approved.
4. **Operations** — six-replica roster, health, failure-domain evidence, backup health and on-call window approved.
5. **Change authorization** — unique change reference, go/no-go time and rollback authority approved by change management.
6. **Preflight** — six endpoint verification, baseline reconciliation and communications channel completed.
7. **Execution** — one authorized fault case at a time with controller audit trail and observer evidence.
8. **Closure** — every intent reconciled and finance, operations and security closure approvals recorded.

```json
{
  "change_reference": "CHG-EXAMPLE-0001",
  "stages": [
    {"id": 1, "status": "approved", "evidence_ref": "ticket://..."},
    {"id": 2, "status": "approved", "evidence_ref": "ticket://..."},
    {"id": 3, "status": "approved", "evidence_ref": "ticket://..."},
    {"id": 4, "status": "approved", "evidence_ref": "ticket://..."},
    {"id": 5, "status": "approved", "evidence_ref": "ticket://..."},
    {"id": 6, "status": "approved", "evidence_ref": "artifact://..."},
    {"id": 7, "status": "approved", "evidence_ref": "artifact://..."},
    {"id": 8, "status": "approved", "evidence_ref": "ticket://..."}
  ]
}
```
