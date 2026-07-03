# csi-rclone-reloaded v1.6.0 — upgrade notes, OneDrive 409 analysis, deploy runbook & monitoring

_Branch: `update-rclone`. Written 2026-07-03. Scope: this driver repo + the `media_server_personal-2`
`terraform-os` rclone modules that consume it._

---

## 1. TL;DR / honest assessment

- **You were never "on an old rclone" — you were on an _unknown_ one.** The Dockerfiles installed rclone
  with `curl https://rclone.org/install.sh | bash` (no version pin), so each rebuild silently baked in
  whatever was latest-stable that day. The live cluster is running **rclone v1.69.1** inside
  `modes/csi-rclone-reloaded:v1.5.1`. This release **pins rclone to v1.74.3** with a per-arch,
  SHA256-verified download so the build is finally reproducible.
- **The version bump does NOT fix the 409 storm.** The 409s are a **concurrent-write race** on OneDrive
  (`nameAlreadyExists` / `resourceModified`), not a bug rclone patched and not throttling. `409` isn't even
  in rclone's OneDrive retry list, so `--low-level-retries` is irrelevant to it. The fix is **fewer
  concurrent writers** (`--transfers`, `--tpslimit`) + `--vfs-cache-mode writes/full` so the VFS write-back
  queue re-uploads until it sticks. Your instinct (12 → 4 → 1 transfers, plus a serialized drain volume)
  was correct — it's a config problem, not a version problem.
- **`--onedrive-no-versions` is not available to you.** All 11 remotes are `drive_type = personal`; that
  flag is Business/SharePoint-only. `--tpslimit` is the available backend-agnostic lever.
- **The `cluster-health-alert` CronJob does not detect 409s.** It's a once-daily pod-Ready → Discord check.
  A 409 storm keeps pods Ready (the mount stays up, uploads just fail/retry in the VFS queue), so it will
  never fire on this. See §6 for what actually catches it.
- **The registrar CrashLoop you're seeing is a separate issue** (the `node-driver-registrar` sidecar exits
  0 and loops, 13–18 restarts) — the `rclone` container itself has **0 restarts**. That's what your
  uncommitted `--health-port`/livenessProbe change targets; unrelated to rclone version.

There _is_ a real reason to take v1.74.3 — just not the 409s: the OneDrive **Personal** fixes in 1.73.0
(dir modtime, sign-in, permissions), the **bare-`remote:` root silent-write fix** in 1.74.2 (both your PVs
mount at `remotePath=""`), `--max-connections`/`--max-buffer-memory` (1.70), ListP paged listing for
OneDrive (1.72), the VFS stale/slow dir-cache fixes (1.69.2), and the two **rc auth-bypass/RCE CVEs**
fixed in 1.73.5 (harmless today because you don't run `--rc`, but a prerequisite the moment you add
monitoring).

---

## 2. What changed in this driver repo (`update-rclone` branch)

| File | Change |
|---|---|
| `Dockerfile`, `cmd/csi-rclone-plugin/Dockerfile` | rclone pinned to **v1.74.3** via per-arch (`TARGETARCH`) SHA256-verified download (install.sh can't pin a version — verified). |
| `Dockerfile`, `Dockerfile.dm` | golang builder pinned `golang:alpine` → **`golang:1.24-alpine`** (was floating). |
| `cmd/csi-rclone-plugin/Dockerfile(.dm)` | Reconciled with root: **fuse → fuse3**, **alpine:3.16 → 3.19**. `make container` now produces an image equivalent to CI's. |
| `pkg/rclone/nodeserver.go` | Removed dead defaults `--cache-info-age=72h`, `--cache-chunk-clean-interval=15m` (deprecated `cache` backend, no-ops on VFS) and the harmful `--dir-cache-time=5s` (→ rclone's native 5m). Kept `vfs-cache-mode=writes`, `allow-non-empty`, `allow-other`. |
| `pkg/rclone/nodeserver.go` | **Now honors CSI `mountOptions` + `readOnly`** (previously computed then discarded). ⚠ behavior change — see §4. |
| `pkg/rclone/nodeserver.go` | Per-volume rclone config now written to a **deterministic 0600 path** and removed in `NodeUnpublishVolume` — fixes the indefinite accumulation of plaintext-secret temp files. Fork-race preserved (file lives for the mount's lifetime). |
| `pkg/rclone/nodeserver.go` | Fixed a pre-existing `go vet` warning in `getSecret` (2 args / 1 verb). `go vet ./...` is now clean. |
| `VERSION`, `CHANGELOG.txt` | v1.5.1 → **v1.6.0**. |

Verification: `go build ./...` exit 0, `go vet ./...` exit 0, `make plugin` produces the binary with
`DriverVersion=v1.6.0`, an in-container `docker build` reports `rclone v1.74.3` / alpine 3.19 / fuse3, and
both baked SHA256s match rclone's official `SHA256SUMS`.

---

## 3. What changed in the consumer repo (NOT applied — edits only)

`media_server_personal-2/.../terraform-os/modules/rclone-pvs/`:

- **`onedrive.tf` (`od`, raw 1.1 TB — mounted by minecraft + prowlarr + Plex-secondary):** it had *no*
  tuning and inherited the driver's harmful defaults (`dir-cache-time=5s`, `vfs-cache-mode=writes`). Added a
  **balanced** profile: `vfs-cache-mode=full`, `dir-cache-time=1h` + `poll-interval=1m`, modest
  `buffer-size=32M`/`vfs-read-ahead=128M` (it's mixed small-IO, not pure streaming), `vfs-cache-max-size=20G`,
  409-safety (`transfers=3`, `tpslimit=10/20`), and its own `log-file=/var/log/rclone-od.log`.
- **`onedrive_shared.tf` (`od-union`, 11 TB — Plex main library):** kept your `transfers=4`/`checkers=8`;
  added `tpslimit=10`/`tpslimit-burst=20` (a hard TPS ceiling you didn't have); raised
  `vfs-read-ahead 32M → 128M` for 4K seeking.
- **Both:** `--max-connections=8` and `--max-buffer-memory=1G` added but **commented out**, gated:
  `# UNCOMMENT ONLY AFTER deploying csi-rclone v1.6.0 (rclone 1.74.3)` — the live 1.69.1 would reject them
  and fail the mount.
- `configData` heredocs (OAuth tokens, crypt passwords, drive_ids) **untouched** — verified in the diff.
- **`rclone-od-drain-t1` (serialized `transfers=1` drain rig) left completely alone** — it's a manual
  backlog-clearing tool and it works.

`csi.volumeAttributes` is immutable, so applying these means a **PV replace** (delete + recreate). Safe
because all three PVs are `reclaimPolicy: Retain` — the OneDrive data is never touched.

---

## 4. Behavior changes to be aware of before rollout

1. **`mountOptions` / `readOnly` are now honored.** Audited the live cluster: **no** PV or StorageClass
   sets `mountOptions`, and **nothing** mounts `readOnly` — so this has **zero impact on your cluster**.
   (General warning for the future: a PV with mount(8)-style `mountOptions` like `ro`/`noatime` would now
   become `--ro`/`--noatime` and crash the rclone mount; and a genuinely-`readOnly` PVC would start
   returning EROFS to writers.)
2. **`dir-cache-time` default is now 5m (was 5s).** New files/dirs can take up to 5m to appear on a mount
   that doesn't set `dir-cache-time` explicitly. Both your tuned PVs set it (1h / 24h) + `poll-interval`,
   so no surprise there.
3. **Bumping rclone later** requires updating *both* per-arch SHA256s in *both* online Dockerfiles or the
   build fails at the checksum step (intentional supply-chain guard).

---

## 5. Deploy runbook (you run this — nothing here has been applied)

> Order matters: **driver image first, then PV replaces.** The PV tuning's 1.70+ flags depend on the new
> image. Rolling the node DaemonSet restarts the `rclone` container, which kills and re-establishes every
> mount on that node — so treat driver rollout as a short maintenance window for the media apps.

**CI/CD reality (two repos, two systems):**
- The **driver image** is built by **GitHub Actions** in `github.com/mod35/csi-rclone-reloaded`
  (`.github/workflows/build_images.yml`, on push to `master`) → Docker Hub `modes/csi-rclone-reloaded`.
  **GitLab CI is not involved in building the image.**
- The **media-server** repo (`gitlab.com/ersutton/media_server_personal`) has a `.gitlab-ci.yml`, but
  **every job in it is commented out** — the pipeline is dormant. So the Terraform upgrade there is
  currently applied **manually / locally**, not by CI. Terraform state is in **S3**
  (`backend.tf` → `bucket = terraform.state.mod35.com`); the csi-rclone + rclone-pvs modules live in the
  **`terraform-os`** project (the sibling `terraform/` project is the app layer: Plex, etc.).
- The csi-rclone image tag is a **hardcoded string** (no version variable) in two files.

**Step 1 — build & push v1.6.0 (GitHub Actions)**
- Merge `update-rclone` → `master` on `github.com/mod35/csi-rclone-reloaded`. The `build images` Action
  builds the root Dockerfile multi-arch (amd64+arm64) and pushes `modes/csi-rclone-reloaded:v1.6.0` +
  `:latest`.
- Confirm: `docker run --rm modes/csi-rclone-reloaded:v1.6.0 rclone version` → `rclone v1.74.3`.

**Step 2 — deploy the new driver image (manual Terraform, `terraform-os`)**
- Bump the tag `v1.5.1 → v1.6.0` at exactly these two lines (hardcoded strings, no variable):
  `terraform-os/modules/csi-rclone/csi-controller-rclone.tf:67` and
  `terraform-os/modules/csi-rclone/csi-node-plugin-rclone.tf:109`.
  _(I did not pre-edit these — they'd break if applied before Step 1 pushes the image.)_
- Scale down the media consumers on each node (or accept a mount blip), then apply locally:
  `cd infrastructure/new-server/terraform-os && terraform init --backend-config "bucket=terraform.state.mod35.com" && terraform apply`.
  The controller StatefulSet + node DaemonSet roll. (If you ever re-enable the GitLab pipeline, this maps
  to the dormant `plan-os-infrastructure` / `deploy-os-infrastructure` jobs.)
- Verify: `kubectl -n kube-system exec <new node-plugin pod> -c rclone -- rclone version` → 1.74.3, and the
  mounts come back (`ps`/`mount | grep fuse.rclone`).

**Step 3 — apply the PV tuning (PV replace, per volume)**
- Since the driver is now on 1.74.3, you may **uncomment the `max-connections`/`max-buffer-memory`** lines
  in `onedrive.tf` / `onedrive_shared.tf` before this step.
- For each of `rclone-od-pvc-no-secrets` and `rclone-od-pvc-no-secrets-shared`: scale down its consumers →
  `terraform apply` the `rclone_pvs` module (Terraform replaces the immutable PV+PVC; `Retain` keeps the
  data) → scale consumers back up → verify the live argv with
  `kubectl -n kube-system exec <node-plugin> -c rclone -- sh -c 'cat /proc/*/cmdline | tr "\0" " "'`.

**Step 4 — verify & retire**
- Watch for 409s during the next real upload batch. Once the backlog is drained, you can retire
  `rclone-od-drain-t1` + `rclone-drain-keeper`.

Rollback: revert the image tag to `v1.5.1` and re-apply; PVs can be replaced back to their prior
`volumeAttributes` (data is safe under `Retain`).

---

## 6. Monitoring recommendations (what actually catches a 409 storm)

The `cluster-health-alert` CronJob checks pod-Ready and won't catch this. What will, cheapest → proper:

1. **rc `vfs/queue` (best signal, no driver change needed).** The driver passes any `volumeAttribute`
   straight through as an rclone flag, so you can enable rclone's remote-control API **per mount purely via
   PV config** — e.g. add `rc = "true"`, a **unique** `rc-addr` per PV (or better a unix socket path so
   ports don't collide on a node), and `rc-enable-metrics = "true"` + a `metrics-addr`. Then
   `rclone rc vfs/queue` shows every file still queued for upload **with its retry count and next-attempt
   delay** — the one thing log-grep can't cheaply tell you: a 409 that self-healed in 40s vs. one that's
   genuinely stuck. `vfs/queue-set-expiry` can force an immediate retry. Post-1.74 rc defaults to
   auth-required, so set `rc-user`/`rc-pass` or bind localhost/socket only.
2. **Prometheus `/metrics`.** With `rc-enable-metrics`/`metrics-addr` set, each mount exposes
   `rclone_errors_total`, `rclone_fatal_error`, transfer/queue gauges. If you have Prometheus/Grafana,
   alert on `rate(rclone_errors_total[5m])` or a non-empty upload queue older than N minutes → your
   existing Discord webhook. This is the "proper" path and needs no bespoke log parser.
3. **Cheapest stopgap.** You already write `/var/log/rclone.log` (JSON) on the tuned mounts. A small
   sidecar/CronJob that `kubectl exec`s the node-plugin and greps for `"status":409` / `resourceModified` /
   `failed to upload` → the same Discord webhook. Works today, but it's log-scraping and can't distinguish
   self-healed from stuck the way `vfs/queue` can.

The honest sequence: you've already cut the **rate** of 409s at the source (low transfers + tpslimit) and
`vfs-cache-mode=full` makes the survivors self-heal via backoff. So monitoring is now about **visibility**
("did the queue actually drain?"), for which **`vfs/queue` + `/metrics` fed into Discord** is the right
end state — and, usefully, you can turn it on through PV `volumeAttributes` without touching driver code.

---

## 7. Other things found (not changed here — flagged honestly)

- **Plaintext secrets in git.** OAuth tokens + crypt passwords for 11 accounts live in the consumer repo's
  `.tf`/`rclone.conf`. Independent of this work, but worth moving to a Secret / SOPS / sealed-secrets.
- **`rclone.conf` reference file has drifted** from the live embedded `configData` (missing the
  `od-crypt-15` upstream). Regenerate it before next using the README token-reconnect flow.
- **`od` and `od-0` share the same underlying drive_id** via two independently-refreshed token sets — a
  latent "refreshed one, not the other" outage source (already noted in the repo README).
- **`make buildx` / `.dm` cross-arch caveat** (pre-existing): the single-stage cmd Dockerfile COPYs one
  host-arch Go binary. CI's root multi-stage Dockerfile is the supported multi-arch path; prefer it.
