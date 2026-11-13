# v1.6.5 migration and rollback

Migration is **copy-only**. It never writes to the source tree, never rewrites
JSON, and never starts either gateway. The v1.6.5 repository and its data
directory remain untouched.

## Preconditions

- Stop the v1.6.5 gateway before taking the copy. A running writer is detected
  by the source snapshot check and aborts the migration, but stopping first is
  still required for a consistent copy.
- Use a dedicated destination that does not already contain data.
- The destination's parent directory must exist, be writable, and must not be
  reached through a symlink; migration rejects symlinked destination or
  receipt parents instead of writing through them.
- Run with enough free space for the whole data tree.

## Migrate

```sh
# Data root mode: contains accounts/, usage/, media/, updates/, catalog/
mix2api migrate --from /srv/mix2api/data --to /srv/mix2api-v2/data --dry-run
mix2api migrate --from /srv/mix2api/data --to /srv/mix2api-v2/data

# Accounts-only mode: --from is the v1 accounts directory
mix2api migrate --from /srv/mix2api/app/accounts --to /srv/mix2api-v2/accounts-v2
```

The command validates every file it copies:

- `.json`: complete JSON document, unknown keys preserved byte-for-byte;
- `.jsonl`: every complete line is a JSON value; a v1 trailing fragment
  without a newline is tolerated;
- `.sqlite3`: `PRAGMA integrity_check` must return `ok`;
- account/state/settings/usage/media/agent/update/pricing files are copied with
  their permissions and checked against a source SHA-256 snapshot.

A destination lock (`<to>.migration.lock`) prevents concurrent copies. A lock
that is recent and valid blocks the copy; a lock older than 24 hours is
reclaimed. Empty or truncated locks are treated as live for one minute (the
window between create and the fsynced PID write) before their mtime makes them
reclaimable. The copy is assembled in `<to>.partial-*` and published with one
rename only after the destination passes the same validation. Failures leave no
destination and no staging directory.

The receipt (`<to>.migration.json`, mode `0600`) lists every file, its sha256,
mode, and size. It is staged before the destination is published. A dry run
validates and prints the same summary but writes neither the destination, lock,
nor receipt. Store the receipt with the migration record; it is outside the
data directory, so v1 and v2 never see it.

## Start v2

```sh
ACCOUNTS_DIR=/srv/mix2api-v2/data/accounts \
USAGE_DIR=/srv/mix2api-v2/data/usage \
WB_MEDIA_DIR=/srv/mix2api-v2/data/media \
/srv/mix2api-v2/mix2api
```

Use the panel and a small `/v1/models` + chat check before switching traffic.

## Rollback

Rollback produces a fresh directory that v1.6.5 can read. It copies current v2
data (including usage rows appended by v2 and v2-created files) and validates it
with the same structural v1-format validators (JSON/JSONL/SQLite; no value rewriting):

```sh
mix2api rollback --from /srv/mix2api-v2/data --to /srv/mix2api-v1-restored --dry-run
mix2api rollback --from /srv/mix2api-v2/data --to /srv/mix2api-v1-restored
```

The original v1 directory is still intact; keep it as the first fallback and
verify the restored copy before pointing v1 at it. Never point v1 and v2 at the
same live directory.

## What is not migrated

- running process state, in-memory sessions, or OAuth callbacks in flight;
- logs still buffered in a running process;
- the v1 Python executable or dashboard file (v2 embeds the dashboard);
- absolute paths stored in user-created agent configuration files. Review
  agent entries pointing at the old installation root.
