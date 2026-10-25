#!/usr/bin/env bash
# Walks through ledgerd against a running server: a card payment (authorize,
# partial capture, refund), an idempotent retry, a refused overdraft and a
# reconciliation run.
#   LEDGERD_URL (default http://localhost:8080), LEDGERD_API_KEY (default local-dev-key-0123456),
#   LEDGERD_ADMIN_TOKEN (needed for step 10 when the server has one, e.g. in Docker)
set -euo pipefail

URL="${LEDGERD_URL:-http://localhost:8080}"
KEY="${LEDGERD_API_KEY:-local-dev-key-0123456}"
RUN="$(date -u +%s)"  # suffix so the demo can run more than once against one ledger

# call METHOD PATH [JSON] [extra curl args...] -> prints the body, fails on 5xx
call() {
  local method=$1 path=$2 body=${3:-}
  shift 3 || shift $#
  local out status
  out=$(curl -sS -w '\n%{http_code}' -X "$method" "$URL$path" -H "Authorization: Bearer $KEY" \
    -H 'Content-Type: application/json' ${body:+-d "$body"} "$@")
  status=${out##*$'\n'}
  if [[ $status -ge 500 ]]; then echo "server error $status: ${out%$'\n'*}" >&2; exit 1; fi
  printf '%s' "${out%$'\n'*}"
}
field() { python3 -c "import sys, json; d = json.load(sys.stdin); print(eval('d' + sys.argv[1]))" "$1"; }
balance() {
  call GET "/v1/accounts/$1" | python3 -c "import sys, json; b = json.load(sys.stdin)['balance']
print(f\"   $1: posted {b['posted']/100:.2f}  pending {b['pending']/100:.2f}  available {b['available']/100:.2f}\")"
}

bank="bank-$RUN" alice="alice-$RUN" shop="shop-$RUN" fees="fees-$RUN"

echo "1. Open accounts (amounts are in cents)"
call POST /v1/accounts "{\"id\":\"$bank\",\"currency\":\"USD\",\"normal_balance\":\"debit\"}" > /dev/null
call POST /v1/accounts "{\"id\":\"$alice\",\"currency\":\"USD\",\"normal_balance\":\"credit\",\"non_negative\":true}" > /dev/null
call POST /v1/accounts "{\"id\":\"$shop\",\"currency\":\"USD\",\"normal_balance\":\"credit\",\"non_negative\":true}" > /dev/null
call POST /v1/accounts "{\"id\":\"$fees\",\"currency\":\"USD\",\"normal_balance\":\"credit\"}" > /dev/null
echo "   $bank (asset), $alice (wallet, may not go negative), $shop, $fees"

echo "2. Alice tops up 100.00"
call POST /v1/transfers "{\"currency\":\"USD\",\"reference\":\"topup-$RUN\",\"postings\":[
  {\"account\":\"$bank\",\"direction\":\"debit\",\"amount\":10000},
  {\"account\":\"$alice\",\"direction\":\"credit\",\"amount\":10000}]}" > /dev/null
balance "$alice"

echo "3. Authorize a 50.00 card payment (pending, 7 days): 48.50 to the shop, 1.50 fee"
auth=$(call POST /v1/transfers "{\"currency\":\"USD\",\"pending\":true,\"timeout_seconds\":604800,\"reference\":\"order-$RUN\",\"postings\":[
  {\"account\":\"$alice\",\"direction\":\"debit\",\"amount\":5000},
  {\"account\":\"$shop\",\"direction\":\"credit\",\"amount\":4850},
  {\"account\":\"$fees\",\"direction\":\"credit\",\"amount\":150}]}" | field "['id']")
echo "   $auth is pending"
balance "$alice"

echo "4. Capture 42.00 of it; the other 8.00 is released"
call POST "/v1/transfers/$auth/post" "{\"postings\":[{\"account\":\"$alice\",\"amount\":4200},
  {\"account\":\"$shop\",\"amount\":4074},{\"account\":\"$fees\",\"amount\":126}]}" | field "['state']" | sed 's/^/   state: /'
balance "$alice"

echo "5. Refund 10.00: a reversal entry linked to the payment (history is never edited)"
refund=$(call POST "/v1/transfers/$auth/reverse" "{\"reference\":\"refund-$RUN\",\"postings\":[
  {\"account\":\"$alice\",\"amount\":1000},{\"account\":\"$shop\",\"amount\":1000}]}")
echo "   $(echo "$refund" | field "['id']") reverses $(echo "$refund" | field "['reverses']")"
balance "$alice"
balance "$shop"

echo "6. Retry a payout with the same Idempotency-Key: it runs once"
payout="{\"currency\":\"USD\",\"reference\":\"payout-$RUN\",\"postings\":[
  {\"account\":\"$shop\",\"direction\":\"debit\",\"amount\":3000},{\"account\":\"$bank\",\"direction\":\"credit\",\"amount\":3000}]}"
first=$(call POST /v1/transfers "$payout" -H "Idempotency-Key: payout-$RUN" | field "['id']")
second=$(curl -sS -D - -o /dev/null -X POST "$URL/v1/transfers" -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' -H "Idempotency-Key: payout-$RUN" -d "$payout" | tr -d '\r' | grep -i '^idempotent-replayed:' || true)
again=$(call POST /v1/transfers "$payout" -H "Idempotency-Key: payout-$RUN" | field "['id']")
echo "   first $first, retry $again (${second:-no replay header})"
balance "$shop"

echo "7. Alice tries to spend 1,000.00: refused, nothing is written"
call POST /v1/transfers "{\"currency\":\"USD\",\"postings\":[
  {\"account\":\"$alice\",\"direction\":\"debit\",\"amount\":100000},
  {\"account\":\"$shop\",\"direction\":\"credit\",\"amount\":100000}]}" | field "['error']['code'] + ': ' + d['error']['message']" | sed 's/^/   /'
balance "$alice"

echo "8. Reconcile the shop's settlement file against the ledger"
today=$(date -u +%F)
csv="external_id,date,amount,currency,reference
stl-1-$RUN,$today,40.74,USD,order-$RUN
stl-2-$RUN,$today,-10.00,USD,BANK-REF-77
stl-3-$RUN,$today,12.00,USD,"
curl -sS -X POST "$URL/v1/accounts/$shop/reconcile?window_days=2" -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: text/csv' --data-binary "$csv" | python3 -c '
import sys, json
r = json.load(sys.stdin)
for m in r["matched"]:
    print("   matched           %s -> %s by %s" % (m["external_id"], m["transfer_id"], m["matched_by"]))
for m in r["missing_in_ledger"]:
    print("   not in ledger:    %s %.2f" % (m["external_id"], m["amount"] / 100))
for m in r["missing_in_statement"]:
    print("   not in statement: %s %.2f" % (m["transfer_id"], m["amount"] / 100))'

echo "9. Alice's statement with running balances"
call GET "/v1/accounts/$alice/statement" | python3 -c '
import sys, json
for l in json.load(sys.stdin)["lines"]:
    b = l["balance"]
    print("   #%-4d %-13s posted %8.2f  pending %8.2f   balance %7.2f  available %7.2f" % (
        l["seq"], l["kind"], l["posted_change"] / 100, l["pending_change"] / 100, b["posted"] / 100, b["available"] / 100))'

echo "10. Invariants"
curl -sS "$URL/v1/admin/invariants" -H "Authorization: Bearer ${LEDGERD_ADMIN_TOKEN:-$KEY}" | field "['ok']" | sed 's/^/   ok: /'
