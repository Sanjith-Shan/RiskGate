## What / Why

<!-- What changes, and the problem it solves. Link the spec section or issue. -->

## How tested

<!-- Commands you ran and what they printed. Paste real output, trimmed. -->

```
go vet ./...
staticcheck ./...
go test -race ./...
scripts/fuzz_all.sh 20s
```

## Numbers changed

<!--
Every number this PR adds or changes (latency, recall, AUC, rows, rps...).
For each: the value, the command that produced it, the commit, the machine
label (loadgen prints one), and whether the data is real (IEEE-CIS),
simulated, or synthetic. Numbers go into profile/NUMBERS_LEDGER.md with this
provenance before a resume sees them. Write "None" if none.
-->

| Number | Value | Command | Machine | Data |
|---|---|---|---|---|

## Risks / rollback

<!-- What could break, who notices, and how to undo it (revert, flag, rule-set version). -->

## Checklist

- [ ] `go vet ./...` clean
- [ ] `staticcheck ./...` clean
- [ ] `go test -race ./...` passes
- [ ] Fuzz targets run (`scripts/fuzz_all.sh`), new fuzz targets added where input is untrusted
- [ ] Docs updated (`DESIGN.md`, `README.md`, `docs/`) and assumptions labelled where they appear
- [ ] No data committed; the test month untouched unless this is the one final evaluation
