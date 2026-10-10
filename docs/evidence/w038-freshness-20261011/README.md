# W038: read-time heartbeat health

Baseline363bd2c32cf354c1eeba3db0e76776d623c661f8, tree8e94f678a1676c93662b7da45b527b3cf242c03c. Own branch codex/enterprise-w038-freshness-20261011; original goacos checkout with two untracked paths remains untouched. Product changes are internal/storage/namingstore.go plus focused tests.

RED0a7df5e ran actual dedicated PostgreSQL17.11 and real signed admin authentication for full v1/v2 HTTP routes, with no naming sweeper started. Stale, missing and future heartbeat rows were falsely healthy in single reads/all-host lists and present in healthyOnly lists; both top-level tests failed. The earlier container setup errors are recorded separately and are not counted as this product reproduction.

The read boundary derives effective ephemeral health from the stored health flag and positive heartbeat deadline configuration. Missing/future/expired timestamps fail closed. A heartbeat exactly at the configured timeout remains valid, matching the sweeper's strict older-than cutoff. Both single-get and list derive health; healthyOnly excludes effectively unhealthy rows even while the raw database flag is1. Persistent instances retain stored health. Reads do not update the database, delete rows or run cleanup. A new real heartbeat recovers the instance through the normal HTTP route.

Actual final PG race: 2 top-level/18 subtests pass, no skips. Deadline/configuration unit race:1top-level/11subtests pass. Full default tests, build, vet, and whole-source symbol govulncheck exit0 on per-command Go1.27.2; individual argv/exits are in verification-results.json and boundary-unit.json. Locks/toolchain requirement remain unchanged (go1.27), so older runtime builds are not approved by this evidence.

Dedicated PG application login has no SUPERUSER/CREATEROLE/CREATEDB/BYPASSRLS, but owns this synthetic database and created tables; this is not RLS isolation evidence. Private randomized test credentials exist only in an environment/private local helper. Integration tests require an explicit loopback database name prefixed w038_. Fixtures/cleanup are namespace-scoped. No existing database or center-host data is involved.

Only local heartbeat read behavior is accepted at this candidate stage. Namespace/service authorization, safe registration/beat ownership, actual app readiness, configuration consistency, production rollout and full W038 remain open. Integration PostgreSQL is verified; MySQL has no actual integration replay yet (same in-memory health derivation, SQL unchanged). Independent review is pending at freeze time.

Rollback reverts only this product helper/read application after restoring an equally safe freshness policy; reverting reintroduces stale healthy responses. Preserve raw RED evidence and existing registrations. No migrations or DNS writes were performed.
