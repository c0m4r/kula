# Backups

Kula can take scheduled **snapshots** of its storage tier files. This protects your metric
history against accidental deletion, lets you archive longer-term data off the ring-buffer, or
move history between machines.

## What gets backed up

The storage engine keeps each tier in a fixed-size binary file (`tier_0.dat`, `tier_1.dat`,
`tier_2.dat`). A backup run copies these files into a timestamped sub-directory:

```
<storage.directory>/backup/20060102-150405/
  tier_0.dat[.gz]
  tier_1.dat[.gz]
  tier_2.dat[.gz]
```

## Configuration

```yaml
backup:
  enabled: false       # master switch
  cron: "0 0 * * *"    # standard 5-field crontab expression (default: midnight)
  maxtier: 3           # how many tiers to back up, from the raw tier
  retention: 1d        # how long to keep backups before pruning
  compress: true       # gzip each backed-up tier file
```

- **`enabled`** — turn scheduled backups on. An invalid `cron` expression fails fast at
  startup (before the server starts), so a typo won't silently disable backups.
- **`cron`** — minute, hour, day-of-month, month, day-of-week. Default `0 0 * * *` runs at
  midnight.
- **`maxtier`** — counting from the raw tier:
  - `1` = `tier_0.dat` only (raw 1-second data)
  - `2` = `tier_0` + `tier_1`
  - `3` = all three tiers
- **`retention`** — how long to keep backups before pruning. Supports `s`, `m`, `h`, `d`
  suffixes (e.g. `7d`). An **empty** value disables pruning (keep forever). Default `1d`.
- **`compress`** — gzip each tier file to `tier_N.dat.gz`.

When backups are enabled, Kula logs the effective schedule on startup, for example:

```
Backup enabled (schedule "0 0 * * *", maxtier 3, retention 1d, compress true)
```

## Restoring

To restore, stop Kula, copy the backed-up `tier_*.dat` (decompressing `.gz` first if
compressed) back into `storage.directory`, and start Kula again. On startup the storage engine
restores its latest-sample cache and reconstructs pending aggregation buffers from the files,
so it resumes serving recent data and continuing tier rollups. Backups leave out the `tier_N.idx`
seek indexes; a restored tier no longer matches its old index, so Kula rebuilds it at the first
start (one read of each tier file).

> Keep `storage.directory` and the tier resolutions consistent between backup and restore. The
> binary format is forward-compatible (old records are skipped cleanly when new metric fields
> are added), but mismatched tier resolutions are not supported.

## Notes

- A backup does not pause collection or the dashboard. Each tier is copied while Kula keeps
  writing, and the records written meanwhile are then copied too, so a backup matches its tier
  as it was when that tier's copy finished.
- With `compress`, only the `.gz` files are synced to disk. The uncompressed copy each is made
  from is deleted, so on a slow disk it may never be written out at all.
- Backups live *inside* `storage.directory/backup` by default. For off-host durability, sync
  that directory elsewhere (rsync, object storage) with your own tooling.
- The sandbox grants read-write access to the storage directory, so backups work under
  Landlock confinement.

Next: [Reverse Proxy & TLS](13-reverse-proxy.md).
