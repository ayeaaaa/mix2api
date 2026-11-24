# Hourly check-in catch-up

The daily check-in slot is still the first attempt, but a successful batch no
longer hides accounts that were not eligible at that moment. After the slot,
each provider's scheduler sweeps once per clock hour and only processes
accounts that do not yet have a successful check-in stamped for the local day.

## Behavior

- Already checked today: skipped locally; no upstream claim is sent.
- Manually disabled, removed, banned, or task-disabled accounts (where the
  provider models that flag) are skipped.
- Auto-disabled/auth-expired and cooling accounts: still attempted. CodeArts
  and WorkBuddy refresh the token first when the account is auto-disabled,
  marked expired, or close to expiry; a successful refresh can clear the
  auto-disable and complete the check-in.
- A failed account keeps the existing `checkin_retry_minutes` backoff (default
  30, max 120). The hourly sweep does not bypass an active retry window.
- `checkin_retry_minutes = 0` disables automatic retries, including an already
  scheduled retry and the hourly sweep.
- The manual all-accounts action remains a manual operation and does not wait
  for the hourly gate.

## Providers

| Provider | Hourly source of truth |
|---|---|
| CodeArts | `state.checkin.ok=true` with `checked_at` for the local day; auto-disabled accounts are attempted |
| WorkBuddy CN | `auth.lastCheckin` for the local day; auth-auto-disabled accounts are attempted after token refresh |
| Doubao | `account.last_checkin` for the local day |
| Raccoon | Already runs in four-hour windows; the next window is its catch-up |

The sweep is local-first: an account already stamped today does not call the
upstream status or claim endpoint. Accounts with no local stamp use the
provider's existing status check before claiming.
