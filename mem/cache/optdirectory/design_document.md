# Design Document: R-Coalescability Metrics

**Status: LOCKED — definitions must not be changed after initial measurement begins.**

---

## Definition 1 (R-coalescable region)

An address-aligned R-byte region is **R-coalescable** if and only if all tracked
cache lines within that region (≥ 1 tracked entry) have **identical sharer sets**.

## Definition 2 (R-coalescable ratio @ strict)

```
strict_ratio = #{R-coalescable regions} / #{R-regions with ≥1 tracked entry}
```

## Definition 3 (R-coalescable ratio @ relaxed)

For each R-region with ≥1 tracked entry:

```
relaxed_ratio_per_region = max_count(identical_sharer_set) / total_tracked_entries_in_region
```

Aggregate:

```
avg_relaxed_ratio = mean(relaxed_ratio_per_region) over all non-empty R-regions
```

---

## Views

### `read_only`
Cache lines that were **read but not written** during the current kernel window.
Implementation: blocks in `accessMaskKernel` that are NOT in `writeMaskKernel`.

### `union`
All cache lines **accessed (read or written)** during the current kernel window.
Implementation: all blocks in `accessMaskKernel`.

### `snapshot`
All **currently valid** directory entries at the kernel boundary, regardless of
whether they were accessed in the current kernel.
Implementation: iterate `directory.GetSets()`, collect `blk.IsValid` entries.

---

## Region Granularities

All five granularities are measured simultaneously and reported together:

| R (bytes) | R / 64B cache lines |
|-----------|---------------------|
| 64        | 1                   |
| 256       | 4                   |
| 1 024     | 16                  |
| 4 096     | 64                  |
| 16 384    | 256                 |

---

## GPU Bitmask Convention

- `sharerSet[blockID]` is a `uint64`; bit `(gpuID - 2)` represents GPU with ID `gpuID`.
- GPU IDs start at 2 in this simulation (GPU[2], GPU[3], …).
- `cohState[blockID]` = 1 means Valid, 0 means Invalid.
- `blockID` = `physicalAddress >> log2BlockSize` (= `Block.Tag >> log2BlockSize`).

---

## Exit Criterion (PHASE 0)

**PASS**: at least one `(R × view)` combination achieves
`avg_strict_coalescable_ratio ≥ 0.30` (30 %) across all measured kernel snapshots.

**R6 FLAG** (FAIL): no `(R × view)` combination meets the threshold →
flag R6 and trigger Plan B / Plan C pivot.

The exit criterion is evaluated **automatically** in `EmitCumulativeReport()`.

---

## `full_redundant` metric (Figure d)

A region is **fully redundant** if it is strict-coalescable AND all entries in the
region also have **identical coherence states** (`cohState`).

```
full_redundant_ratio = #{fully_redundant regions} / #{non-empty regions}
```

---

## REC / HMG Normalization Rules

These rules enable apples-to-apples comparison with the ideal-directory baseline.
**Do not change these rules after measurement begins.**

| Scheme | Normalization |
|--------|---------------|
| Ideal directory (this impl) | Tracks individual 64 B cache lines — no expansion needed. |
| REC (16-CL coalesced entry) | Expand each coalesced entry into 16 individual 64 B CLs, all with the same sharer set as the coalesced entry. |
| HMG (4-CL group) | Treat each 4-CL group as 4 individual 64 B CLs with the same sharer set (guaranteed by HMG's coalescing invariant). |

The normalization is applied before computing `sharerSet` comparison, so all
schemes share the same 64 B granularity baseline.

---

## CSV Schema

### `motivation_coalescability_GPU{N}.csv` — per-kernel snapshots

```
sim_time, gpu_id, kernel_id, view, region_size_bytes,
num_regions_with_entries,
strict_coalescable_ratio,
avg_relaxed_ratio,
full_redundant_ratio,
sharer_set_cardinality_dist_json
```

Written at every kernel boundary. One row per `(kernel × view × R)` triple.

### `motivation_cumulative_GPU{N}.csv` — end-of-simulation summary

```
gpu_id, view, region_size_bytes, num_kernels,
avg_strict_ratio, avg_relaxed_ratio, avg_full_redundant_ratio,
min_strict_ratio, max_strict_ratio,
phase0_pass
```

Written once in `EmitCumulativeReport()` at simulation end.

### Per-access trace — `motivation_trace_GPU{N}.csv` (debug, `dumpTextEnabled` only)

```
sim_time, gpu_id, block_id, access_type, req_gpu_id,
after_sharer_set, after_coh_state
```

**This file must never be mixed into the motivation CSV.**

---

## Prohibited Actions

- Do not change Definitions 1/2/3 after initial measurement.
- Do not use only `avg_relaxed_ratio` to claim ≥ 30 % if `strict_ratio` fails.
- Do not report only the `(R × view)` combination that exceeds 30 % while hiding others.
- Do not mix 1 ms periodic time-based sampling into the motivation CSV.
- Do not change REC / HMG normalization rules without updating this document.
- Do not cherry-pick workloads: per-workload numbers must be preserved alongside the mean.
