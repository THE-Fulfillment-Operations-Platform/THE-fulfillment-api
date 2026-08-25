#!/usr/bin/env bash
# Build every docs/sales-kit/html/*.html into docs/sales-kit/*.pdf using headless Chrome.
# Usage: bash docs/sales-kit/build.sh [name-filter]
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
CHROME=""
for c in "/c/Program Files/Google/Chrome/Application/chrome.exe" \
         "/c/Program Files (x86)/Microsoft/Edge/Application/msedge.exe" \
         "/c/Program Files/Microsoft/Edge/Application/msedge.exe"; do
  [ -f "$c" ] && CHROME="$c" && break
done
[ -n "$CHROME" ] || { echo "No Chrome/Edge found" >&2; exit 1; }
FILTER="${1:-}"
WIN_HERE="$(cygpath -m "$HERE")"
for f in "$HERE"/html/*.html; do
  base="$(basename "$f" .html)"
  [ -n "$FILTER" ] && [[ "$base" != *"$FILTER"* ]] && continue
  out="$HERE/$base.pdf"
  "$CHROME" --headless=new --disable-gpu --no-pdf-header-footer \
    --print-to-pdf="$(cygpath -m "$out")" "file:///$WIN_HERE/html/$base.html" >/dev/null 2>&1 \
    && echo "ok  $base.pdf" || echo "FAIL $base"
done
