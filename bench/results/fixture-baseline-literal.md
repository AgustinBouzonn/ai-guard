# AI-Guard benchmark: fx

2026-11-10 05:52 UTC · linux/amd64, 2 cores, 8022 MB RAM · 3 measured run(s) per patch after 1 warm-up · medians

| Patch | File | Baseline ms | AI-Guard ms | Speedup | Baseline RSS MB | AI-Guard RSS MB | RAM saved | Baseline CPU | AI-Guard CPU | Verdict (baseline / AI-Guard) |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|:---:|
| ts-valid | web/src/mod_604.ts | 7325 | 1496 | 4.9x | 461 | 141 | 69.4% | 68% | 62% | pass / pass |
| ts-syntax | web/src/mod_604.ts | 4662 | 20 | 233.1x | 446 | 40 | 91.0% | 65% | 53% | FAIL / FAIL |
| ts-type | web/src/mod_604.ts | 7252 | 346 | 21.0x | 437 | 115 | 73.7% | 67% | 78% | FAIL / FAIL |
| ts-valid-shared | web/src/shared.ts | 8135 | 18359 | 0.4x | 468 | 193 | 58.8% | 68% | 94% | pass / pass |
| py-valid | api/pkg/m_454.py | 3275 | 552 | 5.9x | 74 | 62 | 16.2% | 71% | 60% | pass / pass |
| py-syntax | api/pkg/m_454.py | 1861 | 12 | 155.1x | 54 | 14 | 74.1% | 89% | 46% | FAIL / FAIL |
| py-type | api/pkg/m_454.py | 3353 | 267 | 12.6x | 74 | 62 | 16.2% | 72% | 74% | pass / FAIL ⚠ |
| py-valid-shared | api/pkg/shared.py | 3454 | 2300 | 1.5x | 73 | 65 | 11.0% | 69% | 54% | pass / pass |
| go-valid | svc/p064/p064.go | 326 | 558 | 0.6x | 72 | 115 | -59.7% | 64% | 67% | pass / pass |
| go-syntax | svc/p064/p064.go | 133 | 5 | 26.6x | 23 | 9 | 60.9% | 59% | 50% | FAIL / FAIL |
| go-type | svc/p064/p064.go | 151 | 245 | 0.6x | 22 | 24 | -9.1% | 56% | 49% | FAIL / FAIL |
| go-valid-shared | svc/shared/shared.go | 1029 | 11587 | 0.1x | 68 | 126 | -85.3% | 82% | 91% | pass / pass |
| rs-valid | crates/core/src/m_064.rs | 228 | 602 | 0.4x | 116 | 140 | -20.7% | 51% | 49% | pass / pass |
| rs-syntax | crates/core/src/m_064.rs | 72 | 46 | 1.6x | 53 | 34 | 35.8% | 49% | 52% | FAIL / FAIL |
| rs-type | crates/core/src/m_064.rs | 104 | 191 | 0.5x | 107 | 105 | 1.9% | 50% | 42% | FAIL / FAIL |
| rs-valid-shared | crates/core/src/shared.rs | 221 | 572 | 0.4x | 116 | 141 | -21.6% | 51% | 49% | pass / pass |
| **ts** (4 patches) | | 27374 | 20221 | **10.1x** | 468 | 193 | **58.8%** | | | |
| **py** (4 patches) | | 11943 | 3131 | **11.5x** | 74 | 65 | **12.2%** | | | |
| **go** (4 patches) | | 1639 | 12395 | **1.0x** | 72 | 126 | **-75.0%** | | | |
| **rs** (4 patches) | | 625 | 1411 | **0.6x** | 116 | 141 | **-21.6%** | | | |
| **all** (16 patches) | | 41581 | 37158 | **2.9x** | 468 | 193 | **58.8%** | | | |

First run of each patch (cold caches, not included above):

| Patch | Baseline ms | AI-Guard ms | Speedup | Baseline RSS MB | AI-Guard RSS MB |
|---|---:|---:|---:|---:|---:|
| ts-valid | 8296 | 3304 | 2.5x | 431 | 159 |
| ts-syntax | 4424 | 23 | 192.3x | 447 | 39 |
| ts-type | 6922 | 343 | 20.2x | 428 | 117 |
| ts-valid-shared | 7074 | 17660 | 0.4x | 433 | 197 |
| py-valid | 7193 | 4148 | 1.7x | 83 | 65 |
| py-syntax | 1876 | 12 | 156.3x | 54 | 14 |
| py-type | 3470 | 248 | 14.0x | 73 | 62 |
| py-valid-shared | 3308 | 1982 | 1.7x | 73 | 65 |
| go-valid | 1304 | 504 | 2.6x | 68 | 118 |
| go-syntax | 126 | 4 | 31.5x | 22 | 9 |
| go-type | 177 | 243 | 0.7x | 23 | 25 |
| go-valid-shared | 995 | 11961 | 0.1x | 69 | 125 |
| rs-valid | 1769 | 1763 | 1.0x | 129 | 165 |
| rs-syntax | 99 | 52 | 1.9x | 54 | 34 |
| rs-type | 179 | 208 | 0.9x | 110 | 108 |
| rs-valid-shared | 217 | 684 | 0.3x | 119 | 142 |

- ms: wall-clock until the pipeline returns its verdict. Group rows add the patch medians; their speedup is the geometric mean of the patch speedups.
- RSS: peak resident memory of the whole process tree (for AI-Guard, plus any persistent type-check daemon). Group rows show the highest peak.
- CPU: CPU-seconds divided by wall-clock and by the 2 logical cores (100% = every core busy for the whole run).
- Verdict: whether each pipeline accepted the patch; ⚠ marks patches on which they disagree.
