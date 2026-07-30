# Scan Observability

## Goal

Restore enough scan-loop logging to explain what GitHub returned, what the reconciler decided for each PR, which queue actions ran, and whether the scan completed cleanly.

## Approved Approach

Emit one concise decision line for every candidate PR on every scan. This favors operational visibility over change-only silence and requires no logging mode or configuration.

Rejected alternatives:

- Change-only logs hide healthy scans and make missing discovery hard to diagnose.
- Configurable debug verbosity adds policy and configuration for one operational need.

## Log Contract

Each scan emits:

1. Start line.
2. Discovery line with assigned, tracked-open, candidate, and snapshot-completeness counts.
3. One line per successfully reconciled PR with:
   - repository and PR number
   - shortened head SHA
   - assigned state
   - placement
   - effective GitHub review state or `none`
   - completed local-review state
   - queue result: `none`, `kept`, or `created`
   - canceled and superseded request counts
4. Existing phase-specific error lines with PR identity.
5. Final line with candidates, reconciled, failed, created, canceled, superseded, completeness, and duration.

PR lines appear only after the database transaction commits. A cancellation count therefore describes committed queue state, not intent.

## Data Shape

Scanner reconciliation returns an internal observation result containing the decision facts needed for logging alongside the database result. The pure decision function and persisted models remain unchanged.

Head SHA logging uses at most twelve characters. Review bodies, comments, tokens, URLs with query strings, and other user-controlled content are never logged.

## Error Handling

- Discovery errors retain the existing incomplete-snapshot message and still produce discovery/final summaries.
- Per-PR hydration, review lookup, and database failures retain PR identity and increment the failed count.
- A failed PR emits no successful decision line.
- Final summary is emitted before returning the joined scan error.

## Test Strategy

A scanner test captures the standard logger and asserts:

- discovery counts and completeness are visible;
- every successfully reconciled candidate produces one decision line;
- created and superseded queue actions are visible;
- final counts match observed behavior;
- full commit SHAs and PR titles are absent.

Existing scanner behavior and race tests remain unchanged.

## Scope

In:

- `internal/scanner/scanner.go`
- `internal/scanner/scanner_test.go`

Out:

- log levels or structured logging migration
- configuration changes
- reactor, runner, GitHub client, database, and Web UI logging
- metrics or tracing

## Success Criteria

1. A healthy scan can be followed from discovery through each PR decision to the final summary.
2. Queue creation and cancellation are visible with PR identity.
3. Partial and failed scans remain visibly incomplete.
4. Logs expose no full SHA or review content.
5. `go test ./internal/scanner` and `go test ./...` pass.

## Simplicity Check

PASS. Fixed log lines use the existing standard logger and scanner data flow. No logging abstraction, configuration surface, or dependency is added.
