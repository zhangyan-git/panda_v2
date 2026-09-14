#!/usr/bin/env bash
#
# Backup for the panda databases.
#
#   backup-postgres.sh [logical|base]
#
# logical (default)
#   One custom-format pg_dump per database, plus the cluster globals. Restores
#   to the instant the dump started. This is the net under the failures that
#   replication does not catch: a dropped table, a bad migration, a
#   DELETE without a WHERE.
#
# base
#   pg_basebackup of the whole cluster, verified against its own manifest. This
#   is what makes the WAL archive worth anything -- archive_mode on its own is a
#   log of changes with no starting point to replay them onto, so PITR is
#   impossible until a base backup exists. Run it on a slower schedule; it is
#   much larger than the logical dumps.
#
# Both modes write one timestamped directory, verify what they wrote, push it
# off this host, and only then prune old copies.
#
# Configuration is environment-only, and uses the libpq names, so a PGPASSFILE
# or the service's own exported settings already work:
#
#   PGHOST PGPORT PGUSER          server to back up
#   PGPASSWORD | PGPASSFILE       credentials. Never pass these as arguments --
#                                 they land in the process table and the shell
#                                 history.
#   BACKUP_UPLOAD_CMD             shell snippet that copies "$1" offsite
#   BACKUP_REMOTE                 what that snippet uploads to (your choice of
#                                 name; the snippet is what reads it)
#
# Optional: BACKUP_DIR (default /var/backups/panda), BACKUP_DATABASES,
# BACKUP_RETENTION_DAYS, BACKUP_BASE_RETENTION_DAYS, BACKUP_ROWCOUNT,
# BACKUP_ALLOW_LOCAL_ONLY, and the PG_DUMP / PG_RESTORE / PG_BASEBACKUP /
# PG_VERIFYBACKUP / PG_DUMPALL / PSQL overrides.
#
# See README.md for the restore drill. A backup that has never been restored is
# a hypothesis, not a backup.
set -euo pipefail

MODE="${1:-logical}"
case "$MODE" in
	logical | base) ;;
	*)
		echo "usage: $(basename "$0") [logical|base]" >&2
		exit 2
		;;
esac

BACKUP_DIR="${BACKUP_DIR:-/var/backups/panda}"
DATABASES="${BACKUP_DATABASES:-panda_identity panda_merchant}"
RETENTION_DAYS="${BACKUP_RETENTION_DAYS:-14}"
BASE_RETENTION_DAYS="${BACKUP_BASE_RETENTION_DAYS:-7}"
ROWCOUNT="${BACKUP_ROWCOUNT:-true}"
PG_DUMP="${PG_DUMP:-pg_dump}"
PG_DUMPALL="${PG_DUMPALL:-pg_dumpall}"
PG_RESTORE="${PG_RESTORE:-pg_restore}"
PG_BASEBACKUP="${PG_BASEBACKUP:-pg_basebackup}"
PG_VERIFYBACKUP="${PG_VERIFYBACKUP:-pg_verifybackup}"
PSQL="${PSQL:-psql}"

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
die() {
	log "ERROR: $*" >&2
	exit 1
}

# Pick the checksum tool once, rather than trying one and falling back on
# failure: the fallback would fire after the redirect had already truncated the
# checksum file, and a missing tool would leave it empty instead of saying so.
# GNU coreutils ships sha256sum, macOS the Perl shasum.
SHA256_TOOL=()
if command -v sha256sum >/dev/null 2>&1; then
	SHA256_TOOL=(sha256sum)
elif command -v shasum >/dev/null 2>&1; then
	SHA256_TOOL=(shasum -a 256)
else
	die "neither sha256sum nor shasum is installed"
fi

# A copy sitting on the same host as the database is not a backup: it dies with
# the disk, and it is exactly the copy a compromised host lets the attacker
# delete. Requiring an offsite destination is the difference between a backup
# and a comforting directory listing, so the local-only escape hatch is named
# after what it actually does.
if [[ -z "${BACKUP_UPLOAD_CMD:-}" && "${BACKUP_ALLOW_LOCAL_ONLY:-false}" != "true" ]]; then
	die "BACKUP_UPLOAD_CMD is not set and BACKUP_ALLOW_LOCAL_ONLY is not true. Set the upload command, or set BACKUP_ALLOW_LOCAL_ONLY=true if you are doing a drill and know the copy is not leaving this host."
fi

if [[ -z "${PGPASSWORD:-}" && -z "${PGPASSFILE:-}" ]]; then
	log "WARNING: neither PGPASSWORD nor PGPASSFILE is set; relying on the server's trust/peer configuration"
fi

STAMP="$(date -u +%Y-%m-%dT%H%M%SZ)"
DEST="$BACKUP_DIR/$MODE/$STAMP"
MARKER="$DEST.complete"

mkdir -p "$BACKUP_DIR/$MODE"

# Two cron entries that overlap -- or one slow run still going when the next
# fires -- would have both write the same kind of artifact and both prune.
if command -v flock >/dev/null 2>&1; then
	exec 9>"$BACKUP_DIR/.lock"
	flock -n 9 || die "another backup run holds $BACKUP_DIR/.lock"
fi

# On unexpected failure, say which directory was left behind. It has no marker,
# so the pruner will keep it -- a half-written backup is not evidence that the
# data is safe, but it is also not something to silently delete.
trap 'log "FAILED: $MODE run left $DEST (unmarked, will not be pruned)"' ERR

# exactRowCounts prints one line per table with its precise row count. The
# counts come from a real count(*) per table rather than reltuples, because
# reltuples is an estimate and an estimate cannot prove a restore was complete.
# Set BACKUP_ROWCOUNT=false where that scan is too expensive.
exactRowCounts() {
	local db="$1" out="$2"
	"$PSQL" --no-password --no-align --tuples-only --dbname="$db" >"$out" <<-'SQL'
		select format('%s.%s %s', schemaname, relname,
		              (xpath('/row/c/text()',
		                     query_to_xml(format('select count(*) as c from %I.%I', schemaname, relname),
		                                  false, true, '')))[1]::text)
		from pg_stat_user_tables
		order by schemaname, relname;
	SQL
}

backupLogical() {
	mkdir -p "$DEST"
	local db out
	for db in $DATABASES; do
		out="$DEST/$db.dump"
		log "dumping $db"
		"$PG_DUMP" --no-password --format=custom --file="$out" --dbname="$db" || die "pg_dump $db failed"
		# A dump that cannot be listed cannot be restored, and the failure is
		# far cheaper to find now than during an incident. Truncation from a
		# full disk is the usual cause.
		"$PG_RESTORE" --list "$out" >/dev/null || die "$out is not a readable archive"
		if [[ "$ROWCOUNT" == "true" ]] && command -v "$PSQL" >/dev/null 2>&1; then
			exactRowCounts "$db" "$DEST/$db.rowcounts.txt" || die "row counts for $db failed"
		fi
	done

	# Roles must exist before the dumps can be restored, otherwise every
	# ownership clause fails and the restore reports a wall of errors that
	# hides the real ones. Passwords in this file are hashed, but the file is
	# still a credential: it goes offsite with the dumps and nowhere else.
	log "dumping cluster globals"
	"$PG_DUMPALL" --no-password --globals-only --file="$DEST/globals.sql" || die "pg_dumpall failed"
}

backupBase() {
	log "taking a base backup into $DEST"
	# -X fetch bundles the WAL the backup itself needs, so this artifact can be
	# restored on its own. Everything after it comes from the archive.
	"$PG_BASEBACKUP" --no-password --pgdata="$DEST" --format=plain \
		--wal-method=fetch --checkpoint=fast --progress || die "pg_basebackup failed"
	log "verifying against the backup manifest"
	"$PG_VERIFYBACKUP" "$DEST" || die "$DEST did not verify against its manifest"
}

case "$MODE" in
logical) backupLogical ;;
base) backupBase ;;
esac

# sha256 for every artifact, so a later restore can tell corruption from a bad
# day before it spends an hour importing garbage.
log "checksumming"
# Relative paths and the redirect placed on find itself, so the file lands in
# DEST and `sha256sum -c SHA256SUMS` verifies from there after a restore.
(cd "$DEST" && find . -type f ! -name SHA256SUMS -exec "${SHA256_TOOL[@]}" {} + >SHA256SUMS)

if [[ -n "${BACKUP_UPLOAD_CMD:-}" ]]; then
	log "uploading to ${BACKUP_REMOTE:-<unset>}"
	bash -c "$BACKUP_UPLOAD_CMD" backup-upload "$DEST" ||
		die "upload failed; $DEST stays local and unmarked so it will not be pruned"
else
	log "WARNING: local-only run, $DEST is still on this host -- not a backup"
fi

# The marker is a sibling rather than a file inside DEST: for base mode DEST is
# a PostgreSQL data directory, and dropping stray files into it is how a
# restored cluster ends up subtly wrong.
touch "$MARKER"
log "backup complete: $DEST"

# Pruning is gated on the marker, not on age. An interrupted or failed run is
# never deleted, so a backup that has been broken for a month is still sitting
# there to be found, instead of being rotated away by the schedule that was
# supposed to be protecting it.
prune() {
	local root="$1" days="$2" old
	[[ "$days" =~ ^[0-9]+$ ]] || die "retention must be a whole number of days"
	[[ "$days" -gt 0 ]] || return 0
	# Neither mode has necessarily ever run, so the other one's directory may
	# not exist yet. Absent means nothing to prune, not a failed backup.
	[[ -d "$root" ]] || return 0
	find "$root" -mindepth 1 -maxdepth 1 -type d -name '20*' -mtime "+$days" | while read -r old; do
		if [[ -f "$old.complete" ]]; then
			log "pruning $old"
			rm -rf "$old" "$old.complete"
		else
			log "keeping $old: no completion marker, so the copy was never intact"
		fi
	done
}

prune "$BACKUP_DIR/logical" "$RETENTION_DAYS"
prune "$BACKUP_DIR/base" "$BASE_RETENTION_DAYS"
