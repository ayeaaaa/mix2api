# Real-account mutable smoke — 2026-11-11

Scope: one dedicated WorkBuddy CN account, one CodeArts account, one Raccoon account, all copied to a temporary v2 accounts directory. Original account files were not modified. Mutating actions were limited to idempotent check-in/reward/growth calls authorized for this test.

## WorkBuddy CN

- `POST /accounts/checkin` with `{}`: `ok=true`; result `state=checked`, `skipped=true`, message `今日已签到，跳过本轮`; no double claim.
- `POST /tasks/run` with `uid`, `claim=true`: `ok=true`; `credit_added=209`.
  - mini-program expert-conversation task claimed +200
  - cat travel reward +9
  - Buddy blind box opened
  - logs ended with `全部完成！本次累计新增到账: +209 积分`
- `POST /tasks/streak-bonus`: `ok=true`; message `连登状态正常（无需兑换/无抽奖次数）`.
- Account credits increased; check-in/activity timestamps remained coherent; no duplicate claims.

## CodeArts

- `POST /codearts/accounts/checkin` with `{}`: `ok=true`; result reported `每日签到领100积分: 已领取` and `新用户注册送 4000 积分: 已领取`; credits `0` this run (already claimed, no double pay).

## Raccoon

- `POST /raccoon/credits/refresh`: `ok=true`; account credits `remain=9282`, `used=18`.
- `POST /raccoon/accounts/grant-desktop`: `ok=true`; `granted=false`, `points=0`, message `桌面端登录奖励已领取过` (idempotent, no double pay).

## Observations

- All mutable actions returned readable v1-compatible results and did not duplicate claims.
- WorkBuddy growth task chain performed cat/Buddy steps internally as designed; standalone cat/Buddy calls were not repeated.
- No real media generation or real update/service operation was performed in this run.

## Gate F status

This closes real-account mutable check-in/growth/streak/reward smoke for WorkBuddy CN, CodeArts, and Raccoon. Still pending:

- real media generation/edits/video
- real systemd/Windows update install/rollback
- CI workflow dry-run
- multi-arch GHCR/arm64 Docker
- v1.6.5 performance baseline
