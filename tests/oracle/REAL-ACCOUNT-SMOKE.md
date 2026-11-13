# Real-account smoke gate (Gate F)

This gate is **manual and pending**. The oracle harness in this directory only
records fake-upstream traffic; it is necessary but not sufficient for a v2.0.0
release.

## Rules

- Use dedicated accounts created for this gate. Never use an account that is
  serving production traffic and never share the v1 and v2 data directory.
- Do not paste access tokens, refresh tokens, cookies, panel passwords, API
  keys, or full account UIDs into reports. Mask UIDs to their last 6 characters.
- Run against a release candidate binary built from the commit being signed.
- Record the build identity from `mix2api version --json`.
- Keep a copy of the account/data fixture used for the run in an approved
  secret store, never in this repository.
- Stop v2 before pointing v1.6.5 at the migrated directory.
- Do not tag, publish, or push a release until this checklist is signed.

## Checklist

### Environment

- [ ] Dedicated v1.6.5 oracle tree and fake upstream recorded separately from
      this real-account run.
- [ ] Candidate binary `version --json` recorded (version, commit, date).
- [ ] Dedicated account set: WorkBuddy intl, WorkBuddy cn, CodeArts, Raccoon.
      At least two accounts per provider where the provider supports pooling.
- [ ] `HOST=127.0.0.1` or a private reverse proxy; panel/API credentials set.
- [ ] Data migrated with `mix2api migrate --dry-run` and then migrate.

### Core gateway

- [ ] `GET /health` reports the expected version and provider summary.
- [ ] `GET /v1/models` (all realms) lists the expected model IDs; no secret
      values or absolute paths appear.
- [ ] Non-stream chat completes for each provider; usage row is appended.
- [ ] Streaming chat emits role/content/reasoning/tool-call frames in the v1
      order and terminates with `[DONE]` (or the documented provider terminator).
- [ ] Responses and Messages endpoints work for a supported model.
- [ ] Invalid model/account errors preserve v1 status and error envelope.
- [ ] One forced upstream 429/401 causes the expected failover or readable
      error, and the account cooldown is visible in the panel.

### Panel and admin

- [ ] Panel login, settings save, realm switch, API-key create/disable.
- [ ] Account import/export, enable/disable, note, test, check-in, refresh.
- [ ] Usage, analytics, logs, pricing, media task pages render.
- [ ] Update page shows current version, executor capability, and backup list;
      install/rollback queue requests but the gateway never executes them.
- [ ] Agent configurator lists local clients and applies a harmless config;
      restore returns the original file.

### Data compatibility

- [ ] v1 account/state/settings/usage files are readable after migration;
      unknown keys remain.
- [ ] A v2-written usage row and settings save are readable by v1.6.5 after
      `mix2api rollback` to a fresh directory.
- [ ] Media task rows and agent state survive a restart.
- [ ] Logs database integrity check passes after a fresh start.

### Update executor

- [ ] Install a real RC asset with a complete backup; restart succeeds.
- [ ] Force a failure after backup (bad start hook) and verify the previous
      binary and files are restored.
- [ ] Roll back to the previous backup and verify the service starts.
- [ ] Windows: repeat install/rollback with the stop/start hooks; the running
      `.exe` is replaced without a locked-file error.

### Evidence

- [ ] Oracle `compare` passes for the selected cases.
- [ ] Load harness report meets the documented p95/memory thresholds.
- [ ] No secrets in logs, status files, backups inventory, or reports.
- [ ] Reviewer confirms `git status` contains no generated data or oracle copy.

## Signoff

| Field | Value |
| --- | --- |
| Candidate version | PENDING |
| Commit | PENDING |
| Build date | PENDING |
| Tester | PENDING |
| Date | PENDING |
| Accounts (masked) | PENDING |
| Oracle run | PENDING |
| Load run | PENDING |
| Exceptions approved by | PENDING |

**Current status: PENDING.** A v2.0.0 final tag must not be created from this
tree until this table is complete and reviewed.

## Known exclusions

- The harness cannot prove behavior of real upstream providers; only this
  dedicated-account run can.
- Long-running quota windows, daily check-in, and growth-task cycles are
  sampled, not exhausted.
- v1 and v2 do not share a live directory; concurrent dual-run behavior is
  explicitly out of scope.
- The panel's visual pixel parity is covered by dashboard tests, not by this
  smoke gate.
