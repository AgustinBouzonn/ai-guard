# AI-Guard benchmark: fx

2026-11-10 06:02 UTC · linux/amd64, 2 cores, 8022 MB RAM · 2 measured run(s) per patch after 1 warm-up · medians

| Patch | File | Baseline ms | AI-Guard ms | Speedup | Baseline RSS MB | AI-Guard RSS MB | RAM saved | Baseline CPU | AI-Guard CPU | Verdict (baseline / AI-Guard) |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|:---:|
| ts-valid | web/src/mod_604.ts | 38703 | 1333 | 29.0x | 426 | 141 | 66.9% | 55% | 58% | pass / pass |
| ts-syntax | web/src/mod_604.ts | 4690 | 19 | 253.5x | 433 | 36 | 91.8% | 64% | 54% | FAIL / FAIL |
| ts-type | web/src/mod_604.ts | 7107 | 339 | 21.0x | 503 | 116 | 76.9% | 68% | 76% | FAIL / FAIL |
| ts-valid-shared | web/src/shared.ts | 38590 | 17263 | 2.2x | 432 | 195 | 55.0% | 55% | 93% | pass / pass |
| py-valid | api/pkg/m_454.py | 3012 | 550 | 5.5x | 74 | 62 | 15.6% | 71% | 60% | pass / pass |
| py-syntax | api/pkg/m_454.py | 1787 | 10 | 188.1x | 55 | 13 | 76.1% | 86% | 53% | FAIL / FAIL |
| py-type | api/pkg/m_454.py | 3266 | 261 | 12.5x | 74 | 62 | 16.2% | 71% | 70% | pass / FAIL ⚠ |
| py-valid-shared | api/pkg/shared.py | 2997 | 2065 | 1.5x | 73 | 65 | 11.0% | 71% | 54% | pass / pass |
| go-valid | svc/p064/p064.go | 1509 | 485 | 3.1x | 116 | 110 | 5.6% | 78% | 66% | pass / pass |
| go-syntax | svc/p064/p064.go | 142 | 5 | 28.3x | 22 | 9 | 59.1% | 56% | 50% | FAIL / FAIL |
| go-type | svc/p064/p064.go | 146 | 284 | 0.5x | 23 | 31 | -34.8% | 57% | 52% | FAIL / FAIL |
| go-valid-shared | svc/shared/shared.go | 11236 | 1867 | 6.0x | 123 | 46 | 62.4% | 89% | 83% | pass / defer |
| rs-valid | crates/core/src/m_064.rs | 625 | 592 | 1.1x | 136 | 141 | -3.3% | 57% | 50% | pass / pass |
| rs-syntax | crates/core/src/m_064.rs | 84 | 50 | 1.7x | 53 | 34 | 35.8% | 48% | 49% | FAIL / FAIL |
| rs-type | crates/core/src/m_064.rs | 112 | 199 | 0.6x | 107 | 107 | 0.5% | 50% | 42% | FAIL / FAIL |
| rs-valid-shared | crates/core/src/shared.rs | 639 | 643 | 1.0x | 138 | 142 | -3.3% | 56% | 48% | pass / pass |
| **ts** (4 patches) | | 89090 | 18953 | **24.2x** | 503 | 195 | **61.3%** | | | |
| **py** (4 patches) | | 11061 | 2885 | **11.7x** | 74 | 65 | **12.2%** | | | |
| **go** (4 patches) | | 13032 | 2641 | **4.1x** | 123 | 110 | **10.6%** | | | |
| **rs** (4 patches) | | 1459 | 1483 | **1.0x** | 138 | 142 | **-3.3%** | | | |
| **all** (16 patches) | | 114640 | 25961 | **5.8x** | 503 | 195 | **61.3%** | | | |

First run of each patch (cold caches, not included above):

| Patch | Baseline ms | AI-Guard ms | Speedup | Baseline RSS MB | AI-Guard RSS MB |
|---|---:|---:|---:|---:|---:|
| ts-valid | 38627 | 1443 | 26.8x | 537 | 141 |
| ts-syntax | 4432 | 18 | 246.2x | 428 | 35 |
| ts-type | 7179 | 384 | 18.7x | 413 | 117 |
| ts-valid-shared | 38472 | 18236 | 2.1x | 424 | 193 |
| py-valid | 3815 | 2242 | 1.7x | 74 | 65 |
| py-syntax | 1717 | 9 | 190.8x | 54 | 14 |
| py-type | 3036 | 238 | 12.8x | 74 | 62 |
| py-valid-shared | 3207 | 1886 | 1.7x | 74 | 65 |
| go-valid | 10360 | 512 | 20.2x | 122 | 114 |
| go-syntax | 133 | 5 | 26.6x | 22 | 9 |
| go-type | 140 | 271 | 0.5x | 22 | 25 |
| go-valid-shared | 11806 | 1801 | 6.6x | 120 | 47 |
| rs-valid | 1211 | 590 | 2.1x | 136 | 143 |
| rs-syntax | 90 | 50 | 1.8x | 54 | 34 |
| rs-type | 132 | 188 | 0.7x | 107 | 104 |
| rs-valid-shared | 590 | 635 | 0.9x | 137 | 144 |

- ms: wall-clock until the pipeline returns its verdict. Group rows add the patch medians; their speedup is the geometric mean of the patch speedups.
- RSS: peak resident memory of the whole process tree (for AI-Guard, plus any persistent type-check daemon). Group rows show the highest peak.
- CPU: CPU-seconds divided by wall-clock and by the 2 logical cores (100% = every core busy for the whole run).
- Verdict: whether each pipeline accepted the patch; ⚠ marks patches on which they disagree; "defer" means AI-Guard found no error but handed part of the work to the remote tier instead of running it locally.
