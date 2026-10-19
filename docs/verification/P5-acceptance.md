# P5 acceptance — gateway mode, executed by the hub

Every scenario below is run by the hub against the merged tree. A lane's own
report is not evidence. Each row names the command and the observation that
decides it.

## A. Static and suite gates

| # | check | pass criterion |
|---|---|---|
| A1 | `make check` in `mgw` | green, reproducible hash printed |
| A2 | `.venv/bin/python -m pytest -q` in `opencode-harness` | all pass, count >= the pre-P5 count |
| A3 | `git diff` review | no provider credential imported in gateway mode; no routing maths duplicated in the harness |
| A4 | `bash test_oc_dispatch.sh` | 82+ cases green |

## B. Real dispatch in gateway mode

Setup: a small worktree, a registry written by `mgw matrix --write`, `MGW_REGISTRY`
exported, `MGW_GATEWAY=1`.

| # | check | pass criterion |
|---|---|---|
| B1 | dispatch settles | `status: settled`, `exit 0`, a file written by the lane |
| B2 | served seat recorded | result carries `served_model`, `served_family`, `served_effort`, `target_tier`, `achieved_tier` |
| B3 | assurance recorded | `assurance` is `full` or `reduced`, `assurance_policy_version` present, `joint_qa_eligible` a bool |
| B4 | lane env | run record lists `MGW_BASE_URL` and `MGW_RUN_TOKEN` among the lane env keys |
| B5 | **no provider key in the lane** | search the lane home, its environment, and any file the lane wrote for the provider key value: **zero** hits. This is the headline claim |
| B6 | gateway audit exists | the attempt's `mgw` audit file has one record with the served model, commit state committed, and a measured or explicitly unknown usage |
| B7 | gateway is gone | no `mgw serve` process remains after the dispatch, and none after a supervisor kill mid-attempt |
| B8 | direct mode unchanged | the same envelope with gateway mode off produces the same credential import as before, and settles |

## C. Failure paths

| # | scenario | pass criterion |
|---|---|---|
| C1 | `mgw` binary absent, gateway mode on | `gateway_binary_missing`, attempt not consumed, no silent fallback to direct mode |
| C2 | gateway killed mid-attempt | lane fails or retries per policy; no orphan process; the result does not claim success |
| C3 | expired or wrong token | gateway returns 401; the lane's failure is reported honestly, not as a quiet settle |
| C4 | provider not permitted in policy | `provider_not_permitted` before any spawn |
| C5 | registry pin mismatch | `policy` rejection, no attempt |

## D. Routing and assurance semantics

| # | check | pass criterion |
|---|---|---|
| D1 | table lookup resolves the fixture cell | the expected seat for the cell is chosen |
| D2 | a candidate rejected by the family guard falls through | review of an OpenAI-authored run never selects an OpenAI seat; the next candidate is used |
| D3 | exhaustive runtime set | an empty survivor list returns `no_eligible_seat`, not a silent cheap seat |
| D4 | degraded review | a run whose served review seat sits below the balanced requirement yields `assurance: reduced` and `joint_qa_eligible: false` |
| D5 | requested-balanced-but-degraded | yields `reduced`, not eligible — a requested strategy is not evidence |
| D6 | intent no longer decides | with routing intent saying "review satisfied", a degraded served seat still reports `reduced` |

## E. Negative evidence inside the lane

Run as the lane's own commands, recorded as evidence:

| # | probe | expected |
|---|---|---|
| E1 | `env \| grep -i api_key` | no provider key |
| E2 | `grep -r <key-prefix> ~/` inside the lane home | nothing |
| E3 | `cat /proc/self/environ` | no provider key |
| E4 | attempt to read the gateway's authority file path | permission denied or absent |
| E5 | `curl` the gateway with a model the token does not allow | 403 `model_not_allowed` |

## F. What would make me reject the merge

- any B5 hit;
- an orphan gateway process after any scenario;
- a settle reported when the lane produced no artifact;
- `review_satisfied` still derivable from routing intent;
- a lane that cannot run in gateway mode but "succeeds" by falling back silently.

## Execution note

Scenarios B and E are the ones that cannot be faked by a fake binary, so they run
against real `mgw` and a real provider. Scenarios C and D use the fake binary
where that is the honest way to force the failure.
