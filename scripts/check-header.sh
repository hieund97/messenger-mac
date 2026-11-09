#!/bin/bash
# So chữ ký hàm trong header do Go sinh ra với core/capi/core.h (bỏ qua const), lệch thì fail.
set -euo pipefail
gen="$1"; hand="$2"
norm() { sed -E 's/const //g; s/[[:space:]]+/ /g; s/ \*/* /g; s/;.*//; s/([A-Za-z0-9_*]+) [A-Za-z_][A-Za-z0-9_]*([,)])/\1\2/g; s/^ //; s/ $//' ; }
want=$(grep -E '^extern [a-z0-9_]+\*? ?core_' "$gen" | sed 's/^extern //' | sed -E 's/GoInt32/int32_t/; s/GoInt64/int64_t/' | norm | sort)
have=$(grep -E '^[a-z0-9_]+ +\*?core_' "$hand" | norm | sort)
if [ "$want" != "$have" ]; then
  echo "core.h lệch với header Go sinh ra:"; diff <(echo "$want") <(echo "$have") || true; exit 1
fi
