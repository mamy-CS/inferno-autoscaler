# Deprecation checklist: `deploy/install-llmd-infra.sh` (llm-d infra shim)

This document is a **working migration plan** for retiring the in-repo llm-d bootstrap (`deploy/install-llmd-infra.sh` and helpers under `deploy/lib/infra_llmd.sh`, `deploy/lib/discovery.sh`, etc.) in favor of **llm-d–owned install tooling** (helmfile, CLI, or documented guides). Use it as a tracker or issue template.

## Goal

- **Stop owning** llm-d gateway / EPP / ModelService lifecycle inside this repository.
- **Keep** WVA-focused install (`deploy/install.sh`) and e2e/benchmark reliability.
- **Preserve** a clear contract (env vars, ordering: WVA base then inference stack) for CI and contributors.

## Current responsibilities (to replace or delegate)

| Area | Today (shim) | Future owner |
|------|----------------|--------------|
| llm-d repo clone / ref | `deploy/lib/infra_llmd.sh` | llm-d install docs or CI image with pinned tree |
| Gateway control plane | `helmfile` under llm-d `guides/prereq/gateway-provider` | Same, invoked by upstream entrypoint or documented one-liner |
| EPP / ModelService / pools | `helmfile apply` + selectors | llm-d |
| CRDs (e.g. GAIE) | `kubectl apply -k` in shim | llm-d or versioned bundle from upstream |
| Emulated cleanup (prefill/decode) | `apply_llm_d_infrastructure_fixes` | e2e fixture or optional llm-d profile; flag `LLMD_REMOVE_EMULATED_DECODE_DEPLOYMENTS` until removed |
| WVA `poolGroup` alignment after pools exist | `helm upgrade` on WVA chart in shim | Thin post-hook after upstream install, or WVA chart default once API stable |
| GPU / simulator detection | `detect_gpu_type` in `deploy/lib/discovery.sh` | llm-d profile or explicit e2e env (no magic in WVA repo) |

## Phases and acceptance criteria

### Phase 1 — Stabilize contract (no behavior change)

- [ ] Document the **minimum env surface** for e2e: `INSTALL_GATEWAY_CTRLPLANE`, `LLMD_*`, `LLM_D_RELEASE`, namespaces, `IMG` / WVA image vars.
- [ ] Ensure **no interactive** paths remain in deploy scripts (done: explicit flags only).
- [ ] List every **consumer**: `make deploy-e2e-infra`, `deploy-e2e-llmd-infra`, `deploy/install-multi-model.sh`, nightly script, OpenShift/benchmark workflows.

**Done when:** A single table in `deploy/README.md` (or this doc) lists callers and the env they set.

### Phase 2 — Introduce upstream install path behind a switch

- [ ] Add **`LLMD_INSTALL_MODE=shim|upstream`** (or `USE_LLM_D_UPSTREAM_INSTALL=true`) with default `shim`.
- [ ] Implement `upstream` branch: shell out to the **documented** llm-d command(s) (helmfile path, release, namespace) with the same effective outcome as today for `kind-emulator` at minimum.
- [ ] Keep **shim** as fallback for one release cycle.

**Done when:** `make deploy-e2e-infra LLMD_INSTALL_MODE=upstream` passes `test-e2e-smoke-with-setup` on Kind (CI optional job first).

### Phase 3 — Migrate CI and docs

- [ ] `.github/workflows/*`: switch primary job to `upstream`; keep shim job until green.
- [ ] `test/e2e/README.md`, `docs/developer-guide/testing.md`, `deploy/README.md`: primary instructions use upstream; shim marked deprecated.
- [ ] `Makefile`: `deploy-e2e-infra` calls upstream by default; target name can stay for compatibility.

**Done when:** Default CI path does not source `deploy/lib/infra_llmd.sh` for the happy path.

### Phase 4 — Delete shim code

- [ ] Remove `deploy/install-llmd-infra.sh` or reduce to a 10-line wrapper that execs upstream + WVA post-step only.
- [ ] Remove dead helpers or move **one** small `deploy/lib/llmd_post_wva.sh` (poolGroup upgrade only) if still needed.
- [ ] Remove `LLMD_INSTALL_MODE` when only one path remains.

**Done when:** Grep for `install-llmd-infra` and `infra_llmd.sh` shows only historical CHANGELOG or a single migration note.

## Mapping: Make targets and workflows (audit list)

Check off as each is migrated to upstream install.

| Entry point | File / location | Notes |
|-------------|-----------------|--------|
| `deploy-e2e-infra` | `Makefile` | Primary e2e infra |
| `deploy-e2e-llmd-infra` | `Makefile` | Deprecated; remove or alias to upstream |
| `deploy-wva-emulated-on-kind` | `Makefile` | Kind + llm-d |
| `deploy-e2e-infra-multi-model` | `Makefile` → `deploy/install-multi-model.sh` | Multi-model path |
| PR Kind e2e | `.github/workflows/ci-pr-checks.yaml` | |
| OpenShift e2e | `.github/workflows/ci-e2e-openshift.yaml` | |
| Benchmark deploy | `.github/workflows/ci-benchmark.yaml` | |
| Nightly / CKS | `deploy/lib/llm_d_nightly_install.sh` | |

## WVA-only steps that may stay in this repo

After migration, this repo may still run **only**:

- `deploy/install.sh` (WVA + monitoring + scaler backend).
- Optional **post-install** reconcile: e.g. WVA helm `poolGroup` if InferencePool API group must be detected at runtime (`detect_inference_pool_api_group`).
- Test fixtures applying CRs / workloads (already in `test/e2e/`).

Prefer moving even poolGroup detection upstream if llm-d install can guarantee a single API group for a given release.

## Rollback

- Keep a **git tag** or branch with the last known-good shim for emergency backport.
- Document: `LLMD_INSTALL_MODE=shim` until removed.

## Exit criteria (definition of done)

1. Default local and CI flows use **llm-d-owned** install for gateway/EPP/ModelService.
2. No duplicate helmfile/logic fork in this repo (or only a pinned submodule / versioned script reference).
3. E2E smoke and full (or agreed subset) green on Kind and OpenShift smoke path.
4. Contributors can follow docs without cloning llm-d manually unless upstream requires it explicitly.

---

*Location: requested “tuner test” folder — `internal/engines/analyzers/queueingmodel/tuner/test/`. Move to `docs/developer-guide/` if this content should live with other migration notes.*
