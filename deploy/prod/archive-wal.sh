#!/usr/bin/env bash
#
# PostgreSQL archive_command target. Postgres invokes it as:
#
#     archive-wal.sh %p %f
#
# %p is the absolute path of the segment that just filled in pg_wal, %f its bare
# filename (for example 00000001000000000000002A).
#
# Postgres runs this as the postgres user with a nearly empty environment, and
# it re-runs it on every failed segment until it succeeds. That contract shapes
# the whole script:
#
#   1. Exit non-zero on ANY failure. Postgres reads a non-zero exit as "not
#      archived yet" and retries. Exit zero while the copy is actually missing
#      and the segment is recycled, which opens a permanent hole in the WAL
#      history -- every PITR restore past that point fails, and you find out
#      months later when you need it. There is no alarm for this. It has to be
#      right the first time.
#
#   2. Be idempotent. Retries re-send segments that already landed. Overwriting
#      an object with identical bytes is fine; doing anything clever about
#      "already exists" is how you accidentally skip a real copy.
#
#   3. Be self-contained. Configuration comes from a file, not the operator's
#      shell profile, because the postgres user will not have one.
#
# Configuration, read from $WAL_ARCHIVE_CONFIG if set, else
# /etc/panda/wal-archive.env, else the environment:
#
#   WAL_ARCHIVE_DIR          a directory to copy segments into. On the database
#                            host this is only a staging area -- point it at a
#                            mounted bucket or an NFS export to make it real.
#   WAL_ARCHIVE_UPLOAD_CMD   optional shell snippet that receives the staged
#                            file as "$1" and moves it to object storage.
#                            Prefer this over WAL_ARCHIVE_DIR for object storage:
#                            a CLI upload is atomic per object, while a plain
#                            directory copy is not.
#
# At least one of the two must move the data off this host; see README.md.
set -euo pipefail

SEGMENT_PATH="${1:?usage: archive-wal.sh <segment-path> <segment-name>}"
SEGMENT_NAME="${2:?usage: archive-wal.sh <segment-path> <segment-name>}"

CONFIG="${WAL_ARCHIVE_CONFIG:-/etc/panda/wal-archive.env}"
if [[ -f "$CONFIG" ]]; then
	# shellcheck disable=SC1090 # the whole point is that the path is external
	source "$CONFIG"
fi

log() { printf '%s archive-wal: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; }

[[ -f "$SEGMENT_PATH" ]] || {
	log "$SEGMENT_PATH does not exist"
	exit 1
}

if [[ -z "${WAL_ARCHIVE_DIR:-}" && -z "${WAL_ARCHIVE_UPLOAD_CMD:-}" ]]; then
	log "neither WAL_ARCHIVE_DIR nor WAL_ARCHIVE_UPLOAD_CMD is configured; refusing to report a segment archived when nothing stored it"
	exit 1
fi

if [[ -n "${WAL_ARCHIVE_DIR:-}" ]]; then
	mkdir -p "$WAL_ARCHIVE_DIR" || {
		log "cannot create $WAL_ARCHIVE_DIR"
		exit 1
	}
	# Copy under a unique temporary name and rename into place, so a reader (or
	# a restore) can never observe a half-written segment. $$ keeps concurrent
	# invocations -- Postgres normally serializes them, but a manual replay
	# would not -- from colliding on the temporary.
	tmp="$WAL_ARCHIVE_DIR/.$SEGMENT_NAME.tmp.$$"
	cp "$SEGMENT_PATH" "$tmp" || {
		log "copy to $tmp failed"
		rm -f "$tmp"
		exit 1
	}
	# Compare sizes before publishing: a full disk truncates the copy while
	# leaving cp's exit status at 0 on some filesystems, and a short segment is
	# indistinguishable from a good one once it is in the archive.
	if [[ "$(wc -c <"$tmp")" != "$(wc -c <"$SEGMENT_PATH")" ]]; then
		log "size mismatch copying $SEGMENT_NAME; not publishing it"
		rm -f "$tmp"
		exit 1
	fi
	mv -f "$tmp" "$WAL_ARCHIVE_DIR/$SEGMENT_NAME" || {
		log "publishing $SEGMENT_NAME failed"
		rm -f "$tmp"
		exit 1
	}
	log "stored $SEGMENT_NAME"
fi

if [[ -n "${WAL_ARCHIVE_UPLOAD_CMD:-}" ]]; then
	source_file="${WAL_ARCHIVE_DIR:-}/$SEGMENT_NAME"
	[[ -n "${WAL_ARCHIVE_DIR:-}" ]] || source_file="$SEGMENT_PATH"
	bash -c "$WAL_ARCHIVE_UPLOAD_CMD" archive-upload "$source_file" "$SEGMENT_NAME" || {
		log "upload of $SEGMENT_NAME failed"
		exit 1
	}
	log "uploaded $SEGMENT_NAME"
fi
