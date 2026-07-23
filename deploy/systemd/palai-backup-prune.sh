#!/bin/sh
# E14 T5 — retention/prune example for palai-backup archives. Deletes archives older than
# <days> days, but ALWAYS keeps the single newest one — so a stale install that stopped
# backing up still retains its last good backup instead of pruning itself to zero. Called by
# palai-backup.service ExecStartPost; safe to run by hand. Proven by deploy/systemd/prune_test.go.
#
# Usage: palai-backup-prune.sh <backup-dir> <retention-days>
set -eu

dir="${1:?usage: palai-backup-prune.sh <backup-dir> <retention-days>}"
days="${2:?usage: palai-backup-prune.sh <backup-dir> <retention-days>}"

case "$days" in
	'' | *[!0-9]*) echo "palai-backup-prune: retention-days must be a non-negative integer, got '$days'" >&2; exit 2 ;;
esac
[ -d "$dir" ] || { echo "palai-backup-prune: no such directory: $dir" >&2; exit 2; }

# The newest archive by mtime — never pruned. ls -t is portable across GNU and BSD `find`
# (unlike GNU-only `find -printf`), and the fixed glob carries no parse ambiguity here.
# shellcheck disable=SC2012
newest="$(ls -1t "$dir"/palai-backup-*.tar.gz 2>/dev/null | head -n1 || true)"

# -mtime +<days> is "older than <days>*24h", portable on GNU and BSD find. The newest match is
# skipped explicitly so it survives even when it is itself older than the retention window.
find "$dir" -maxdepth 1 -type f -name 'palai-backup-*.tar.gz' -mtime +"$days" 2>/dev/null |
	while IFS= read -r archive; do
		[ "$archive" = "$newest" ] && continue
		rm -f -- "$archive" && echo "palai-backup-prune: removed $archive"
	done
