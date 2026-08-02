# Codex Landlock seccomp profile

Restores real per-run sandboxing for Codex in Kubernetes pods.

## Why

On the current nodes, both Codex sandbox backends are denied:

- bubblewrap needs an unprivileged user namespace, blocked by the node sysctl
  (`kernel.unprivileged_userns_clone=0`).
- Landlock (`CODEX_USE_LEGACY_LANDLOCK=true`) needs the `landlock_add_rule`,
  `landlock_create_ruleset`, and `landlock_restrict_self` syscalls, which the
  container runtime's `RuntimeDefault` seccomp profile predates, so
  `landlock_restrict_self` fails with `Sandbox(LandlockRestrict)`.

`codex-landlock-seccomp.json` is the upstream moby default profile
(moby/moby v27.5.1, `SCMP_ACT_ERRNO` default, 428-syscall allowlist) which
already includes the `landlock_*` syscalls. Using it as a `Localhost` profile
keeps RuntimeDefault-equivalent hardening while letting Landlock work.

## What goes where

This directory is a hand-off bundle for the Argo-managed infrastructure repo;
nothing here is referenced by the kustomizations in `k8s/`.

1. **Infra repo (Argo):** `daemonset.yaml` + a ConfigMap holding
   `codex-landlock-seccomp.json`. The DaemonSet copies the profile to
   `/var/lib/kubelet/seccomp/agent-runtime/codex-landlock-seccomp.json` on
   every node. Create the ConfigMap alongside it, either:

   ```yaml
   # kustomize
   configMapGenerator:
     - name: codex-seccomp-profile
       files:
         - codex-landlock-seccomp.json
   ```

   or

   ```sh
   kubectl -n <ns> create configmap codex-seccomp-profile \
     --from-file=codex-landlock-seccomp.json
   ```

   The DaemonSet is namespace-agnostic; run it wherever node-level installers
   live (it tolerates all taints so the profile lands on every node).

2. **This repo:** the `k8s/{stage,prod}` api and worker deployments reference
   the profile via:

   ```yaml
   seccompProfile:
     type: Localhost
     localhostProfile: agent-runtime/codex-landlock-seccomp.json
   ```

## Rollout order

1. Sync the DaemonSet and wait for it to be Ready on all nodes (the profile
   file must exist before any pod references it).
2. Roll the agent-runtime api/worker deployments (they pick up the Localhost
   profile from this repo's manifests).
3. Verify inside the worker pod:
   `codex sandbox -c features.use_legacy_landlock=true -- echo probe-ok`
4. Once the probe passes, remove `CODEX_SANDBOX_UNAVAILABLE` from Doppler
   (keep `CODEX_USE_LEGACY_LANDLOCK=true`) so runs get kernel-enforced
   read-only/workspace-write sandboxing again.

## Updating the profile

Re-fetch from a current moby tag and confirm the landlock syscalls survive:

```sh
curl -fsSL https://raw.githubusercontent.com/moby/moby/<tag>/profiles/seccomp/default.json \
  -o codex-landlock-seccomp.json
grep -c landlock codex-landlock-seccomp.json  # expect 3 names present
```
