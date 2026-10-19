# Gate map

Every acceptance gate in the plan maps to one test function named `GateNN_Name`
and one file named `internal/<pkg>/gate_NN_name_test.go`. `make gates` fails when
a gate marked `implemented` has no test in the tree, and when a gate marked
`planned` is marked `implemented` without one.

Status values: `planned` (no test yet) or `implemented` (test exists and passes).

| Gate | Name | Status | Test file |
|---|---|---|---|
| G1 | Secret isolation | planned | internal/proxy/gate_01_secret_isolation_test.go |
| G2 | Token authority | planned | internal/proxy/gate_02_token_authority_test.go |
| G3 | Upstream discipline | planned | internal/proxy/gate_03_upstream_discipline_test.go |
| G4 | Budget reservation | planned | internal/ledger/gate_04_budget_test.go |
| G5 | Commit boundary | planned | internal/proxy/gate_05_commit_boundary_test.go |
| G6 | Dialect parity per client | planned | internal/provider/gate_06_dialect_parity_test.go |
| G7 | Routing guards | planned | internal/policy/gate_07_routing_guards_test.go |
| G8 | Matrix fidelity and runtime admission | planned | internal/catalog/gate_08_matrix_test.go |
| G9 | Onboarding | planned | internal/cli/gate_09_onboarding_test.go |
| G10 | Assurance contract | planned | internal/policy/gate_10_assurance_test.go |
| G11 | Lifecycle and placement | planned | internal/proxy/gate_11_lifecycle_test.go |
| G12 | Transport hardening | planned | internal/proxy/gate_12_transport_test.go |
| G13 | Cross-language parity | planned | internal/parity/gate_13_parity_test.go |
| G14 | Go quality gate | planned | internal/quality/gate_14_quality_test.go |
| G15 | Stream proof | planned | internal/proxy/gate_15_stream_test.go |
