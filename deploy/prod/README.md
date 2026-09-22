# Production deployment template

Use 3 or 5 etcd members with TLS, authentication, least privilege, snapshots, and a tested restore runbook. Inject secrets externally; no real values belong here.

## PostgreSQL backups

Replication and backups solve different problems, and only one of them is here.
A standby protects you from a machine dying. It does not protect you from a
`DROP TABLE`, a migration that misbehaves, or a `DELETE` with the wrong `WHERE`
-- every one of those is faithfully replicated to the standby within
milliseconds. Before adding replication, have this working and rehearsed:
the failure you cannot replicate your way out of is the one that actually
happens.

Two layers, because they answer different questions:

| Layer | Question it answers | Cadence | Artifact |
| --- | --- | --- | --- |
| `backup-postgres.sh logical` | "the database as it was last night" | nightly | one `pg_dump` per database, plus cluster globals |
| `backup-postgres.sh base` | "the database as it was at 14:32, four minutes before the bad migration" | weekly | `pg_basebackup` of the cluster |
| `archive-wal.sh` | the change history that turns a base backup into an arbitrary point in time | continuous | WAL segments |

The WAL archive is useless on its own -- it is a log of changes with no starting
point to replay them onto. A base backup is what gives it one. Take both or
neither.

### Installing

Copy the scripts somewhere the `postgres` user can execute, e.g.
`/opt/panda/`, and point the server at the archive hook by adding
`postgres-archive.conf` to the cluster's configuration (see the header of that
file for how to include it). `archive_mode` is read once at startup: a reload
leaves the WAL archive silently off while `pg_settings` reports the value you
just set, so plan on a restart.

Configuration for the archive hook goes in `/etc/panda/wal-archive.env`, because
Postgres runs `archive_command` with an almost empty environment -- there is no
shell profile to inherit from:

```sh
# /etc/panda/wal-archive.env  (mode 0600, owned by postgres)
WAL_ARCHIVE_DIR=/var/lib/postgresql/wal-archive
WAL_ARCHIVE_UPLOAD_CMD='ossutil cp "$1" oss://your-bucket/panda-wal/"$2"'
```

Keep `WAL_ARCHIVE_DIR` on the database host only as a staging area. It is the
upload command that satisfies "offsite"; a directory on the same disk as the
database dies with the database.

Nightly backups run with the same credentials a normal client would use, so put
them in a `PGPASSFILE` rather than `PGPASSWORD` -- environment variables are
visible in `/proc` and in the process table:

```sh
# /etc/panda/backup.env
BACKUP_DIR=/var/backups/panda
# BACKUP_DATABASES is deliberately not set here. The script's default is every
# database the platform owns, and leaving it unset keeps that list in one place
# instead of two -- this sample used to pin "panda_identity panda_merchant",
# which is what the script defaulted to, so anything copied from here would
# have kept backing up only those two even after the default was fixed.
#
# Setting it narrows the backup. A narrowed run still reports success, so the
# script warns about every database on the server the list does not cover.
# BACKUP_DATABASES="panda_identity panda_merchant"
BACKUP_UPLOAD_CMD='ossutil cp -r "$1" oss://your-bucket/panda-backups/'
```

```cron
# Nightly logical dumps, weekly physical base backup. The scripts take a lock
# under BACKUP_DIR, so an overlap is refused rather than run twice.
17 3 * * *   . /etc/panda/backup.env; PGPASSFILE=/etc/panda/pgpass /opt/panda/backup-postgres.sh logical
40 4 * * 0   . /etc/panda/backup.env; PGPASSFILE=/etc/panda/pgpass /opt/panda/backup-postgres.sh base
```

An unset `BACKUP_UPLOAD_CMD` is a hard error rather than a warning. The only way
to run without it is `BACKUP_ALLOW_LOCAL_ONLY=true`, which is named that way on
purpose: it exists for drills, not for production.

### Prerequisites for `base` mode

`pg_basebackup` opens a *replication* connection, which is governed by its own
`pg_hba.conf` entry that a default installation does not have. The connection
fails with `no pg_hba.conf entry for replication connection` until you add one:

```
# pg_hba.conf
host  replication  backup  <backup-host>/32  scram-sha-256
```

The role also needs `REPLICATION` (`ALTER ROLE backup WITH REPLICATION;`) or
superuser. Narrow the address range to the backup host -- a replication
connection can read every byte in the cluster, so this is not a line to widen
"temporarily" and forget.

### Monitoring

A backup that stopped working is silent, and you find out during the incident it
was supposed to cover. The archiver exposes exactly one counter that matters:

```sql
select archived_count, failed_count, last_archived_wal, last_failed_wal,
       now() - last_archived_time as since_last
from pg_stat_archiver;
```

Alert when `failed_count` increases, when `since_last` exceeds a few times
`archive_timeout`, or when `pg_wal` grows. **A failing `archive_command` does not
lose data -- it fills the disk until the instance stops accepting writes.** That
is the correct failure direction, and it means an unexplained full disk is the
symptom of an archive that has been broken for days, not a disk that is too
small. Check `/var/backups/panda/` for directories with no matching `.complete`
file for the same reason: an interrupted run is kept rather than rotated away,
so a backup that has been broken for a month is still sitting there to be found.

## Restore drill

Run this against a scratch instance, on a schedule, before you need it. A backup
that has never been restored is a hypothesis.

### From a logical dump

```sh
# 1. Verify the artifacts before importing anything. A truncated dump from a
#    full disk fails here rather than halfway through a restore.
cd /var/backups/panda/logical/2026-09-11T031700Z && sha256sum -c SHA256SUMS

# 2. Roles must exist before the dumps, or every ownership clause fails and
#    buries the real errors.
psql -d postgres -f globals.sql

# 3. Restore into a NEW database first. Never restore over a live one: if the
#    dump turns out to be the wrong one, you have destroyed the evidence and
#    the original together.
createdb -T template0 panda_identity_restore
pg_restore -j 4 -d panda_identity_restore panda_identity.dump

# 4. Prove it, do not eyeball it. Compare exact per-table counts against the
#    baseline the backup recorded at dump time:
psql -d panda_identity_restore <<'SQL' > /tmp/restored.txt
select format('%s.%s %s', schemaname, relname,
              (xpath('/row/c/text()',
                     query_to_xml(format('select count(*) as c from %I.%I', schemaname, relname),
                                  false, true, '')))[1]::text)
from pg_stat_user_tables order by schemaname, relname;
SQL
diff 2026-09-11T031700Z/panda_identity.rowcounts.txt /tmp/restored.txt && echo "identical"
```

`BACKUP_ROWCOUNT=false` skips the baseline for databases where the per-table
scan is too expensive. Set it deliberately, not to make a slow job finish.

Swap the names and promote only once the comparison is clean.

### Point-in-time recovery

Recovering to a time rather than to "last night" means replaying WAL onto a base
backup. The marker row below is written *after* the base backup, so it exists
only in the WAL archive -- which is what makes this a test of the archive rather
than of the base backup.

```sh
# 1. Find the target. A named restore point is easier to reason about than a
#    timestamp, and can be created right before a risky change:
#      select pg_create_restore_point('before_migration_2026_09_11');

# 2. Restore the base backup and configure recovery.
cp -a /var/backups/panda/base/2026-09-08T040000Z /var/lib/postgresql/pitr-data
rm -f /var/lib/postgresql/pitr-data/backup_manifest
touch /var/lib/postgresql/pitr-data/recovery.signal
cat >> /var/lib/postgresql/pitr-data/postgresql.auto.conf <<'EOF'
restore_command = 'cp /var/lib/postgresql/wal-archive/%f %p'
recovery_target_name = 'before_migration_2026_09_11'
recovery_target_action = 'promote'
EOF

# 3. Start it on a spare port and WATCH THE LOG. "restored log file ... from
#    archive" is the line that proves the archive is doing the work; if every
#    segment came from the base backup's bundled WAL, you are testing the backup
#    and not the archive.
pg_ctl -D /var/lib/postgresql/pitr-data -o "-p 5433" -l /tmp/pitr.log start
grep -E "restored log file|recovery stopping|archive recovery complete" /tmp/pitr.log
```

Then verify the last known-good row is present and the first bad one is not, and
only then promote. If the recovery lands short of the target, the usual cause is
a gap in the archive -- which is why `failed_count` above is worth an alert.

Two things that will bite during a real recovery, not during a drill:

- **Timeline history files.** After a promotion, later recoveries to a point
  before that promotion need the `*.history` file, which Postgres writes into
  `pg_wal` on promotion but does not push through `archive_command`. Copy it into
  the archive after any promotion you intend to keep recovering across.
- **The archive is only as good as its oldest base backup.** Segments before the
  oldest base backup can never be replayed. Retain base backups at least as long
  as the furthest-back point in time you intend to recover to.
