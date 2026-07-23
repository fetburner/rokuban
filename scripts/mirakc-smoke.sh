#!/usr/bin/env bash
set -euo pipefail

MIRAKC_URL="${1:?usage: $0 <mirakc-url> (e.g. http://localhost:40772)}"
MIRAKC_URL="${MIRAKC_URL%/}"

pass=0
fail=0

check() {
  local label="$1"; shift
  if "$@" >/dev/null 2>&1; then
    echo "  ✓ $label"
    pass=$((pass + 1))
  else
    echo "  ✗ $label"
    fail=$((fail + 1))
  fi
}

echo "=== mirakc smoke test ==="
echo "target: $MIRAKC_URL"
echo

# version
echo "[version]"
if version=$(curl -sf "$MIRAKC_URL/api/version"); then
  echo "  $(echo "$version" | jq -r '"current=\(.current) latest=\(.latest)"')"
  check "GET /api/version" test -n "$version"
else
  echo "  ✗ GET /api/version (connection failed)"
  fail=$((fail + 1))
fi

# services
echo "[services]"
if services=$(curl -sf "$MIRAKC_URL/api/services"); then
  count=$(echo "$services" | jq 'length')
  echo "  count=$count"
  check "GET /api/services (count > 0)" test "$count" -gt 0
else
  echo "  ✗ GET /api/services (connection failed)"
  fail=$((fail + 1))
fi

# programs
echo "[programs]"
if programs=$(curl -sf "$MIRAKC_URL/api/programs"); then
  count=$(echo "$programs" | jq 'length')
  echo "  count=$count"
  check "GET /api/programs (count > 0)" test "$count" -gt 0
else
  echo "  ✗ GET /api/programs (connection failed)"
  fail=$((fail + 1))
fi

# schedules
echo "[schedules]"
if schedules=$(curl -sf "$MIRAKC_URL/api/recording/schedules"); then
  count=$(echo "$schedules" | jq 'length')
  echo "  count=$count"
  check "GET /api/recording/schedules" test -n "$schedules"
else
  echo "  ✗ GET /api/recording/schedules (connection failed)"
  fail=$((fail + 1))
fi

# records
echo "[records]"
if records=$(curl -sf "$MIRAKC_URL/api/recording/records"); then
  count=$(echo "$records" | jq 'length')
  echo "  count=$count"
  check "GET /api/recording/records" test -n "$records"

  if [ "$count" -gt 0 ]; then
    rid=$(echo "$records" | jq -r '.[0].id')
    echo "  first record: $rid"
    check "GET /api/recording/records/$rid" curl -sf "$MIRAKC_URL/api/recording/records/$rid"
    check "HEAD /api/recording/records/$rid/stream" curl -sfI "$MIRAKC_URL/api/recording/records/$rid/stream"
  fi
else
  echo "  ✗ GET /api/recording/records (connection failed)"
  fail=$((fail + 1))
fi

# SSE (3 秒間接続して HTTP 200 を受信できることを確認)
echo "[SSE]"
sse_status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "$MIRAKC_URL/events" 2>/dev/null) || true
if [ "$sse_status" = "200" ]; then
  echo "  ✓ GET /events (connected)"
  pass=$((pass + 1))
else
  echo "  ✗ GET /events (status=$sse_status)"
  fail=$((fail + 1))
fi

echo
echo "=== result: $pass passed, $fail failed ==="
exit "$fail"
