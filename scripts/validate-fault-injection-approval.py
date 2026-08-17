#!/usr/bin/env python3
import json, sys
from pathlib import Path

if len(sys.argv) != 2:
    raise SystemExit('usage: validate-fault-injection-approval.py <approval.json>')
doc = json.loads(Path(sys.argv[1]).read_text())
if not isinstance(doc.get('change_reference'), str) or not doc['change_reference'].strip():
    raise SystemExit('change_reference is required')
stages = doc.get('stages')
if not isinstance(stages, list) or len(stages) != 8:
    raise SystemExit('exactly eight approval stages are required')
seen = set()
for stage in stages:
    if not isinstance(stage, dict): raise SystemExit('stage must be an object')
    ident = stage.get('id')
    if not isinstance(ident, int) or ident < 1 or ident > 8 or ident in seen:
        raise SystemExit('stage ids must be unique integers 1..8')
    seen.add(ident)
    if stage.get('status') != 'approved':
        raise SystemExit(f'stage {ident} is not approved')
    if not isinstance(stage.get('evidence_ref'), str) or not stage['evidence_ref'].strip():
        raise SystemExit(f'stage {ident} requires evidence_ref')
if seen != set(range(1, 9)):
    raise SystemExit('approval stages 1..8 are required')
print('FAULT_INJECTION_APPROVAL_VALID')
