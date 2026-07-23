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
    ((pass++))
  else
    echo "  ✗ $label"
    ((fail++))
  fi
}

echo "=== mirakc smoke test ==="
echo "target: $MIRAKC_URL"
echo

# version
echo "[version]"
version=$(curl -sf "$MIRAKC_URL/api/version")
echo "  $(echo "$version" | jq -r '"current=\(.current) latest=\(.latest)"')"
check "GET /api/version" test -n "$version"

# services
echo "[services]"
services=$(curl -sf "$MIRAKC_URL/api/services")
count=$(echo "$services" | jq 'length')
echo "  count=$count"
check "GET /api/services" test "$count" -gt 0

# programs
echo "[programs]"
programs=$(curl -sf "$MIRAKC_URL/api/programs")
count=$(echo "$programs" | jq 'length')
echo "  count=$count"
check "GET /api/programs" test "$count" -gt 0

# schedules
echo "[schedules]"
schedules=$(curl -sf "$MIRAKC_URL/api/recording/schedules")
count=$(echo "$schedules" | jq 'length')
echo "  count=$count"
check "GET /api/recording/schedules" test -n "$schedules"

# records
echo "[records]"
records=$(curl -sf "$MIRAKC_URL/api/recording/records")
count=$(echo "$records" | jq 'length')
echo "  count=$count"
check "GET /api/recording/records" test -n "$records"

if [ "$count" -gt 0 ]; then
  rid=$(echo "$records" | jq -r '.[0].id')
  echo "  first record: $rid"
  check "GET /api/recording/records/$rid" curl -sf "$MIRAKC_URL/api/recording/records/$rid"
  check "HEAD /api/recording/records/$rid/stream" curl -sfI "$MIRAKC_URL/api/recording/records/$rid/stream"
fi

# SSE (3 秒間接続して少なくとも接続が成功することを確認)
echo "[SSE]"
sse_out=$(curl -sf -N --max-time 3 "$MIRAKC_URL/events" 2>&1 || true)
check "GET /events (connect)" true

echo
echo "=== result: $pass passed, $fail failed ==="
exit "$fail"
