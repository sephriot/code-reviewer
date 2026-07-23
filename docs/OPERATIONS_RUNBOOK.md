# Operations runbook

## Backup

Stop legacy writer first. Never use `data/reviews.db` as v2 database.

```bash
go run ./cmd/reviewctl db backup --source data/reviews.db --destination data/backups/reviews.db
go run ./cmd/reviewctl db verify-backup --backup data/backups/reviews.db
```

Back up v2 with normal filesystem-consistent SQLite backup tooling while
`reviewd` is stopped. Keep backup outside repository and verify file checksum.

## Upgrade

1. Keep `REVIEWD_PUBLICATION_MODE_ENABLED=false`.
2. Stop `reviewd`.
3. Back up `data/control-plane.db`.
4. Apply only known migrations:

```bash
go run ./cmd/reviewctl db migrate --database data/control-plane.db --apply
go run ./cmd/reviewctl db status --database data/control-plane.db
```

5. Start `./run.sh`; confirm schema current, read model online, and Runtime
   Activity has no failed migration/job entries.

## Restore

1. Stop `reviewd` and retain failed database copy for investigation.
2. Restore verified v2 backup to `data/control-plane.db`.
3. Run `reviewctl db status`; never migrate legacy database.
4. Start with publication disabled and reconcile GET-only before enabling any
   review execution or publication.

## Cutover rehearsal

```bash
go run ./cmd/reviewctl db ownership-probe --state-dir data/writer-ownership
go run ./cmd/reviewctl db checkpoint --database data/control-plane.db --name rehearsal-pre-cutover
```

Do not transfer legacy writer ownership until export/rollback suppression is
implemented and a fixture rehearsal verifies counts and checksums.
