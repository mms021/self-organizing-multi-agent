#!/usr/bin/env bash
# Manual walkthrough of the Core Loop against a running `cmd/server` + real
# Redis. Not a substitute for `go test ./...` (internal/httpapi/e2e_test.go
# is the self-checking version of this same scenario) — just a human sanity
# script. Requires: server running on $BASE (default localhost:8080), curl, jq.
set -euo pipefail

BASE="${BASE:-http://localhost:8080}"

echo "== health =="
curl -sf "$BASE/health" | jq .

echo "== register agent A (creator/verifier) =="
A=$(curl -sf -X POST "$BASE/agents/register" -d '{}')
A_ID=$(echo "$A" | jq -r .agent_id)
A_TOKEN=$(echo "$A" | jq -r .credential.token)
echo "agent A: $A_ID"

echo "== register agent B (claimer/result sender) =="
B=$(curl -sf -X POST "$BASE/agents/register" -d '{}')
B_ID=$(echo "$B" | jq -r .agent_id)
B_TOKEN=$(echo "$B" | jq -r .credential.token)
echo "agent B: $B_ID"

echo "== A creates a task =="
TASK=$(curl -sf -X POST "$BASE/tasks" \
  -H "Authorization: Bearer $A_TOKEN" -H "Idempotency-Key: smoke-1" \
  -d '{"objective":"run the smoke test"}')
TASK_ID=$(echo "$TASK" | jq -r .task_id)
echo "task: $TASK_ID"

echo "== B claims the task =="
CLAIM=$(curl -sf -X POST "$BASE/tasks/$TASK_ID/claim" -H "Authorization: Bearer $B_TOKEN")
echo "$CLAIM" | jq .
CLAIM_ID=$(echo "$CLAIM" | jq -r .claim_id)

echo "== B posts a RESULT message =="
curl -sf -X POST "$BASE/messages" -H "Authorization: Bearer $B_TOKEN" -d "{
  \"message_id\": \"smoke-result-1\",
  \"protocol_version\": \"1.0\",
  \"type\": \"RESULT\",
  \"sender\": \"$B_ID\",
  \"recipient\": \"$A_ID\",
  \"timestamp\": \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\",
  \"task_id\": \"$TASK_ID\",
  \"priority\": \"normal\",
  \"payload\": {\"summary\": \"done\", \"status\": \"success\", \"claim_id\": \"$CLAIM_ID\"}
}" | jq .

echo "== A long-polls for it (should return promptly, not wait the full 30s) =="
time curl -sf "$BASE/messages?wait_seconds=30" -H "Authorization: Bearer $A_TOKEN" | jq .

echo "== A verifies the RESULT =="
curl -sf -X POST "$BASE/tasks/$TASK_ID/verify" -H "Authorization: Bearer $A_TOKEN" -d '{
  "target_message_id": "smoke-result-1",
  "verdict": "verified"
}' | jq .

echo "== confirm task is COMPLETED =="
curl -sf "$BASE/tasks/$TASK_ID" -H "Authorization: Bearer $A_TOKEN" | jq .status
