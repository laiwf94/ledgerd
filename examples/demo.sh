#!/usr/bin/env bash
# Walks through ledgerd against a running server: a card payment (authorize,
# partial capture, refund), an idempotent retry, a refused overdraft, a
# reconciliation run, an account's lifecycle (open, fund, freeze, list,
# raise a limit, close), and a bank's camt.053 statement reconciled against a
# payout batch net of fees, saved, confirmed and aged until its break closes.
#   LEDGERD_URL (default http://localhost:8080), LEDGERD_API_KEY (default local-dev-key-0123456),
#   LEDGERD_ADMIN_TOKEN (needed for the last step when the server has one, e.g. in Docker)
set -euo pipefail

URL="${LEDGERD_URL:-http://localhost:8080}"
KEY="${LEDGERD_API_KEY:-local-dev-key-0123456}"
RUN="${LEDGERD_DEMO_RUN:-$(date -u +%s)}"  # suffix so the demo can run more than once against one ledger

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
    ids = m.get("transfer_id") or "+".join(p["transfer_id"] for p in m["postings"])
    print("   matched           %s -> %s by %s" % (m["external_id"], ids, m["matched_by"]))
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

# expect CODE -> reads an error body on stdin, fails unless its code is CODE
expect() {
  python3 -c "import sys, json
e = json.load(sys.stdin).get('error') or {}
if e.get('code') != sys.argv[1]: sys.exit('expected %s, got %r' % (sys.argv[1], e))
print('   refused: %s: %s' % (e['code'], e['message']))" "$1"
}
status() {
  call GET "/v1/accounts/$1" | python3 -c "import sys, json; a = json.load(sys.stdin); l = a['limits']
fmt = lambda v: 'none' if v is None else '%.2f' % (v / 100)
print(f\"   $1: {a['status']}{' (' + a['status_reason'] + ')' if a.get('status_reason') else ''}, available {a['balance']['available']/100:.2f}, overdraft limit {fmt(l['overdraft'])}\")"
}

echo "10. Open two supplier wallets tagged with this run's cohort, and fund one with 200.00"
cohort="cohort-$RUN" acme="acme-$RUN" zenith="zenith-$RUN"
for id in "$acme" "$zenith"; do
  call POST /v1/accounts "{\"id\":\"$id\",\"currency\":\"USD\",\"normal_balance\":\"credit\",\"non_negative\":true,
    \"metadata\":{\"cohort\":\"$cohort\"}}" > /dev/null
done
call POST /v1/transfers "{\"currency\":\"USD\",\"reference\":\"fund-$RUN\",\"postings\":[
  {\"account\":\"$bank\",\"direction\":\"debit\",\"amount\":20000},
  {\"account\":\"$acme\",\"direction\":\"credit\",\"amount\":20000}]}" > /dev/null
status "$acme"

echo "11. Freeze $acme for review: payouts are refused until it is unfrozen"
call POST "/v1/accounts/$acme/freeze" '{"reason":"compliance review"}' > /dev/null
status "$acme"
call POST /v1/transfers "{\"currency\":\"USD\",\"reference\":\"payout-acme-$RUN\",\"postings\":[
  {\"account\":\"$acme\",\"direction\":\"debit\",\"amount\":5000},
  {\"account\":\"$bank\",\"direction\":\"credit\",\"amount\":5000}]}" | expect account_frozen

echo "12. List the cohort one account per page, following the signed cursor"
page="/v1/accounts?metadata%5Bcohort%5D=$cohort&limit=1"
while :; do
  out=$(call GET "$page")
  echo "$out" | python3 -c 'import sys, json
d = json.load(sys.stdin)
for a in d["accounts"]:
    print("   %-18s %-7s available %7.2f   (page as of entry %d, has_more %s)" % (
        a["id"], a["status"], a["balance"]["available"] / 100, d["as_of_seq"], d["has_more"]))'
  next=$(echo "$out" | field ".get('next_cursor', '')")
  [[ -n $next ]] || break
  page="/v1/accounts?metadata%5Bcohort%5D=$cohort&limit=1&cursor=$next"
done
echo "   transfers carrying reference fund-$RUN, found through the reference index:"
call GET "/v1/transfers?reference=fund-$RUN" | python3 -c 'import sys, json
for t in json.load(sys.stdin)["transfers"]:
    print("   %s %s %.2f" % (t["id"], t["state"], sum(p["amount"] for p in t["postings"] if p["direction"] == "debit") / 100))'

echo "13. Unfreeze and raise the overdraft limit to 50.00; a 230.00 payout now goes through"
call POST "/v1/accounts/$acme/unfreeze" '{"reason":"review passed"}' > /dev/null
call POST "/v1/accounts/$acme/limits" '{"kind":"overdraft","amount":5000,"reason":"approved credit line"}' \
  | field "['limits']['effective_seq']" | sed 's/^/   limit takes effect from entry /'
call POST /v1/transfers "{\"currency\":\"USD\",\"reference\":\"payout-acme-$RUN\",\"postings\":[
  {\"account\":\"$acme\",\"direction\":\"debit\",\"amount\":23000},
  {\"account\":\"$bank\",\"direction\":\"credit\",\"amount\":23000}]}" > /dev/null
status "$acme"
call GET "/v1/accounts/$acme/limits" | python3 -c 'import sys, json
for c in json.load(sys.stdin)["changes"]:
    print("   %-9s -> %-7s from entry %d  %s" % (c["kind"], "none" if c["amount"] is None else "%.2f" % (c["amount"] / 100),
        c["effective_seq"], c.get("reason", "")))'

echo "14. Close $acme: refused while it owes 30.00, accepted once it is settled to zero"
call POST "/v1/accounts/$acme/close" '{"reason":"offboarded"}' | expect failed_precondition
call POST /v1/transfers "{\"currency\":\"USD\",\"reference\":\"settle-$RUN\",\"postings\":[
  {\"account\":\"$bank\",\"direction\":\"debit\",\"amount\":3000},
  {\"account\":\"$acme\",\"direction\":\"credit\",\"amount\":3000}]}" > /dev/null
call POST "/v1/accounts/$acme/close" '{"reason":"offboarded"}' > /dev/null
status "$acme"
call POST /v1/transfers "{\"currency\":\"USD\",\"postings\":[
  {\"account\":\"$bank\",\"direction\":\"debit\",\"amount\":100},
  {\"account\":\"$acme\",\"direction\":\"credit\",\"amount\":100}]}" | expect account_closed

echo "15. Reconcile the bank's camt.053 statement for the operating account: a card payout batch net of fees"
ops="operating-$RUN"
call POST /v1/accounts "{\"id\":\"$ops\",\"currency\":\"USD\",\"normal_balance\":\"debit\"}" > /dev/null
for amt in 12000 8000 4550; do  # three card sales the processor pays out together as po-$RUN
  call POST /v1/transfers "{\"currency\":\"USD\",\"reference\":\"po-$RUN\",\"postings\":[
    {\"account\":\"$ops\",\"direction\":\"debit\",\"amount\":$amt},
    {\"account\":\"$shop\",\"direction\":\"credit\",\"amount\":$amt}]}" > /dev/null
done
call POST /v1/transfers "{\"currency\":\"USD\",\"reference\":\"fee-po-$RUN\",\"postings\":[
  {\"account\":\"$fees\",\"direction\":\"debit\",\"amount\":736},
  {\"account\":\"$ops\",\"direction\":\"credit\",\"amount\":736}]}" > /dev/null
call POST /v1/transfers "{\"currency\":\"USD\",\"reference\":\"wd-$RUN\",\"postings\":[
  {\"account\":\"$shop\",\"direction\":\"debit\",\"amount\":10000},
  {\"account\":\"$ops\",\"direction\":\"credit\",\"amount\":10000}]}" > /dev/null
echo "   ledger: sales 120.00 + 80.00 + 45.50 under po-$RUN, an estimated fee of 7.36, a 100.00 wire to the shop"
# entry ENTRY_ID AMOUNT CRDT|DBIT STATUS DETAILS -> one camt.053 Ntry
entry() {
  printf '      <Ntry><Amt Ccy="USD">%s</Amt><CdtDbtInd>%s</CdtDbtInd><Sts>%s</Sts><BookgDt><Dt>%s</Dt></BookgDt>
        <ValDt><Dt>%s</Dt></ValDt><AcctSvcrRef>%s</AcctSvcrRef><NtryDtls>%s</NtryDtls></Ntry>\n' \
    "$2" "$3" "$4" "$today" "$today" "$1" "$5"
}
camt=$(cat <<XML
<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.02">
  <BkToCstmrStmt>
    <GrpHdr><MsgId>msg-$RUN</MsgId><CreDtTm>${today}T23:00:00Z</CreDtTm></GrpHdr>
    <Stmt>
      <Id>stmt-$RUN</Id><CreDtTm>${today}T23:00:00Z</CreDtTm>
      <Acct><Id><IBAN>GB33BUKB20201555555555</IBAN></Id><Ccy>USD</Ccy></Acct>
$(entry "bk1-$RUN" 245.50 CRDT BOOK "<Btch><PmtInfId>po-$RUN</PmtInfId><NbOfTxs>3</NbOfTxs></Btch>")
$(entry "bk2-$RUN" 7.39 DBIT BOOK "<TxDtls><RmtInf><Ustrd>fee-po-$RUN</Ustrd></RmtInf></TxDtls>")
$(entry "bk3-$RUN" 100.00 DBIT BOOK "<TxDtls><Refs><EndToEndId>wd-$RUN</EndToEndId></Refs></TxDtls>")
$(entry "bk4-$RUN" 2.50 DBIT BOOK "<TxDtls><RmtInf><Ustrd>ACCOUNT MAINTENANCE</Ustrd></RmtInf></TxDtls>")
$(entry "bk5-$RUN" 60.00 CRDT PDNG "<TxDtls><Refs><EndToEndId>po-next-$RUN</EndToEndId></Refs></TxDtls>")
    </Stmt>
  </BkToCstmrStmt>
</Document>
XML
)
# reconcile_camt -> saves a run of the statement with a 0.05 tolerance and prints it; prints the run id last
reconcile_camt() {
  curl -sS -X POST "$URL/v1/accounts/$ops/reconcile?window_days=2&tolerance=0.05&save=true" \
    -H "Authorization: Bearer $KEY" -H 'Content-Type: application/xml' --data-binary "$camt" | python3 -c '
import sys, json
r = json.load(sys.stdin)
if "error" in r: sys.exit("reconcile: %r" % r["error"])
c = r["camt053"]
st = c["statements"][0]
print("   statement %s: %d entries, %d booked, %d pending skipped" % (st["id"], st["entries"], st["booked"], c["skipped_entries"]))
for m in r["matched"]:
    n = len(m.get("postings") or [1])
    d = m.get("difference", 0)  # the line minus the movement, for a near-match
    diff = " (ledger %.2f, difference %.2f)" % (m["amount"] / 100, d / 100) if d else ""
    print("   matched           %s %8.2f -> %d movement%s by %s%s" % (
        m["external_id"], (m["amount"] + d) / 100, n, "" if n == 1 else "s", m["matched_by"], diff))
for m in r["missing_in_ledger"]:
    print("   not in ledger:    %s %.2f %s" % (m["external_id"], m["amount"] / 100, m.get("reference", "")))
run = r["reconciliation"]
s = run["summary"]
print("   saved run #%d: matches %d (near %d), proposed %d, breaks %d" % (
    run["number"], s["matches"], s["near_matches"], s["proposed"], s["breaks"]))
print(run["id"])'
}
aging() {
  call GET "/v1/reconciliations/breaks/aging?account=$ops" | python3 -c 'import sys, json
r = json.load(sys.stdin)
print("   open breaks on %s: %d" % (r["account"], r["open_breaks"]))
for b in r["buckets"]:
    if b["open_breaks"]:
        print("   %-4s days: %d missing in ledger, %d missing in statement, %d mismatched, %s" % (b["bucket"],
            b["missing_in_ledger"], b["missing_in_statement"], b["amount_mismatches"],
            ", ".join("%s %s" % (a["currency"], a["amount"]) for a in b["amounts"])))'
}
out=$(reconcile_camt)
printf '%s\n' "$out" | sed '$d'
recon=$(printf '%s\n' "$out" | tail -n 1)
call POST "/v1/reconciliations/$recon/confirm" '{"all":true,"note":"checked against the payout report"}' | python3 -c '
import sys, json
r = json.load(sys.stdin); s = r["reconciliation"]["summary"]
print("   confirmed %d matches: %d proposed, %d confirmed" % (r["changed"], s["proposed"], s["confirmed"]))'
aging
curl -sS "$URL/metrics" -H "Authorization: Bearer $KEY" | grep '^ledgerd_recon_open_breaks{bucket="0-2"}' | sed 's/^/   /'
echo "   book the bank's 2.50 charge and reconcile the statement again: the break closes"
call POST /v1/transfers "{\"currency\":\"USD\",\"reference\":\"bank-charge-$RUN\",\"postings\":[
  {\"account\":\"$fees\",\"direction\":\"debit\",\"amount\":250},
  {\"account\":\"$ops\",\"direction\":\"credit\",\"amount\":250}]}" > /dev/null
reconcile_camt | sed '$d' | grep -E 'bk4-|saved run'
aging

echo "16. Invariants"
curl -sS "$URL/v1/admin/invariants" -H "Authorization: Bearer ${LEDGERD_ADMIN_TOKEN:-$KEY}" | field "['ok']" | sed 's/^/   ok: /'
