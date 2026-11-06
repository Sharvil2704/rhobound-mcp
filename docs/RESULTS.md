# Results: rhobound as a deploy gate, on real clusters (November 2026)

Every number on this page was measured on AWS, on the maze bed in [bed/](../bed/README.md):
- 32 services on k3s;
- a 120 req/s background load;
- an OpenTelemetry Collector;
- rhobound's gate in GitHub Actions.

**How each experiment was run:**
- its expected outcome was written down and committed before any run (the DECLARE files);
- its predictions were committed before any production measurement;
- its truth is a deploy bracket (`bed/verify/truthx.sh`): 300 s at baseline, the change, a 180-s warm-up, 300 s on the
  change, then a rollback.

The reports are in [bed/reports/](../bed/reports/). They name raw data folders that are not part of this repository;
the scores they were made from are (`bed/reports/muse-live/SCORE-1.md`, `SCORE-2.md`, `data3/p3/score.*`, `data4/score.*`).
For publication, wording in the reports that addressed the bed's operator was made neutral. Nothing measured, declared or
decided changed.

## The verdict

**On this bed, no verdict was wrong: the 8 passes measured clean, the 2 blocks were real regressions, and the 7
inconclusives held changes that measured clean; on changes of one or two images the gate was faster than a 5-min
bake.** It is one day on one synthetic app, 17 verdicts in all (see Limits). It is not an accuracy study of the gate,
and nothing on this page is production telemetry.

**As a way to make a coding agent better, it was net negative:** the agent wrote good code without it, and rhobound
added time and false holds.

| what the gate said, on the bed, 2026-11-03 | how many | came true? |
|---|---|---|
| pass: "the predicted change of mean latency, widened by the model's error, stays under the threshold" | 8 | **8 of 8**: production measured each one clean |
| block: "the predicted change is over the threshold" | 2 | **2 of 2**: production regressed, +35 to +38 ms |
| inconclusive: "cannot decide", so the change is held | 7 | each of the 7 measured clean: false holds |
| a pass on a change that regressed | 0 | |

- **What every verdict rested on:** one A/B of each changed service on its own node (300 s), run off production, and
  one prediction of every user-facing entry.
- **Where the false holds came from:**
  - five on fraud, a one-worker service whose A/B is noisy;
  - two on a three-image change whose true effect sits at the 4-ms limit.
- **What a hold costs:** the gate's time, then the bake you would have done anyway.

## The experiments, in order

| | what was tested | result | report |
|---|---|---|---|
| Cluster test 2 | three planted PRs (+5 ms, +1 ms, a no-op), sealed predictions against truth | the old model fit 2 of 5 entries, the route-node model (now in the engine) 5 of 5; no wrong pass for any model; it blocked the "+1 ms" pricing change that really cost +6.1 ms and passed the no-op | [CT2-RESULTS](../bed/reports/maze-live/CT2-RESULTS.md) |
| Cluster test 3 | six PRs on the fixed engine | **no wrong pass and no wrong block**; block of +6.37 ms right; the agent's pricing fix passed (partner −17.30 ms) | [CT3-RESULTS](../bed/reports/maze-live/CT3-RESULTS.md) |
| Experiment 1 | Muse Code (Meta's coding agent) with and without the rhobound plugin, 2 tickets | both arms accepted both tickets, no regression; rhobound made deploys slower (variance-doubt false holds, fixed since) | [RESULTS](../bed/reports/muse-live/RESULTS.md) |
| Experiment 2 | two identical beds: Muse alone against Muse + rhobound, 3 hard tickets | Muse alone 3/3 accepted, with rhobound 2/3; 0 regressions in both; the fraud ticket held 3 times (rule 10, measurement-limited) | [RESULTS-2](../bed/reports/muse-live/RESULTS-2.md) |
| 2b | the rule-10 fix (a longer A/B, not other code) and a harder ticket on the hottest service | the held ticket was accepted first try and measured clean; the hard ticket did not separate the arms: both wrote the efficient code | [RESULTS-3 parts 1-2](../bed/reports/muse-live/RESULTS-3.md) |
| Deploy time | rhobound's gate against a 5-min big-bang bake and a staged bake, single- and multi-image changes | **one image: about 7.5 min saved** (the gate 7.5 to 8.0 min against a 15.4-min bake). Multi-image changes then took one gate per image, one after another: slower than one bake | [RESULTS-3 part 3](../bed/reports/muse-live/RESULTS-3.md) |
| Multi-image gate + parallel A/Bs | one PR for several images; the A/Bs at once, one per A/B node | **2 images: pass in 9.2 min against a 15.5-min bake**; the costly 3-image change blocked in 9.7 min with no user exposure (the bake: users at +38 ms for 9 min); 3 A/Bs in 572 s instead of 1,400 | [RESULTS-4](../bed/reports/muse-live/RESULTS-4.md) |

**The older single-bed demo on kshop** is in [docs/mcp/RESULTS.md](mcp/RESULTS.md), kept as history.

## What the time saving is

| change | rhobound (one gate, parallel A/Bs) | 5-min big-bang bake | staged bake |
|---|---|---|---|
| 1 image, clean | 7.5 to 8.0 min (about 7.5 min saved) | 15.4 min | 15.4 min |
| 2 images, clean (pricing + media) | **9.2 min, pass** | 15.5 min | about 24.7 min |
| 3 images, one costly (the media plant) | **9.7 min to reject**, no exposure, culprit named | 15.5 min to detect, users at +38 ms for 9 min, culprit unknown | 34.1 min, users at +35 ms for 9 min |
| 3 images at the limit (rank + pricing + media) | 9.6 min, then inconclusive (false hold): about 25 min with the fallback bake | 15.5 min | 34.0 min |
| production changing or under watch | **seconds** (the rollout only) | 15.5 min | 15 to 34 min |

- **Longer bakes:** teams that bake longer than 5 min save more. At 15-min bakes, rhobound is faster on every decisive
  change measured.
- **Where rhobound's time goes:** one 300-s A/B per image, all at once up to the number of A/B nodes (3 here), plus
  about 2 minutes of readiness, flush and builds.

## Prediction accuracy

- **Bands:** 25 of 25 entry-level predictions in the multi-image runs, and 34 of 35 in the deploy-time comparison,
  landed inside their predicted bands.
- **One prediction against adding gates:** one prediction over a whole multi-image change came closer to the truth than
  adding up the gates' predictions, at 13 of 15 entries.
- **The costly change was underestimated:** +22 to +26 ms predicted, +38 ms measured. The gate itself had flagged that
  media's predicted utilization (0.80) lay beyond its validated range.

## Limits

- **Sample size:** each change ran once, and each ticket had one agent sample per arm. These results are illustrative,
  not statistics.
- **Mean latency only:** a pass says nothing about p95 or p99 ([docs/mcp/ARCHITECTURE.md, 1](mcp/ARCHITECTURE.md#1-goals-and-non-goals)).
- **Advisory by design:** the engine records every verdict as advisory until an environment has earned enforcement
  ([ARCHITECTURE.md, 6.6](mcp/ARCHITECTURE.md#66-from-advisory-to-enforcing)); deploy-on-pass is the bed's rule, not
  the engine's.
- **Not an accuracy study:** no accuracy study of the gate has been run on software we did not write; the engine's own
  accuracy figures come from our simulator and our own clusters ([ACCURACY.md](ACCURACY.md)).
- **The bed:** the maze is a synthetic app, built to break the model's assumptions, not a production system.
- **Noisy services:** a one-worker service with a 1-ms construct widening at its caller (fraud) can still be held when
  it is clean.
- **Small A/B nodes:** an arm pair that needs more cores than its A/B node has (media's plant: about 4.5 of 4) measures
  CPU queueing as well as code.
- **Not run on a bed:** a real multi-image `deploy.sh` (it is tested offline only), and the parallel longer A/B (it ran
  only in its declining branch).
