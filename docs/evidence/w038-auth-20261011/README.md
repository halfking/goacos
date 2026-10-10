# W038/W039 bounded authentication admission, 2026-10-11

Base: `ff28237270777a7f161e2b0b0a3b840bc42dd86e`, branch `codex/enterprise-goacos-auth-20261011`. This slice closes the reproduced request-admission defects below. It does not close W038/W039 or authorize deployment.

## Observed failures and implementation

`5be47d0` records tests before the first product fix. Signed tokens with missing/wrong issuer, missing expiry, future issued-at, padded subjects or HS384/HS512 were accepted. Ordinary users could mutate v1/v2 registry, configuration and namespace state; disabled/deleted users and demoted administrators retained token authority. The real PostgreSQL HTTP replay also checks durable history, so publish-then-delete cannot hide a denied write.

Protected requests now require HS256, issuer `goacos`, expiry and a nonblank unpadded subject. A single storage statement reloads the enabled user and current roles; token role labels no longer grant request authority. Users/roles management and all state changes require a live global administrator, including GET/HEAD v1 heartbeat. Only the exact two POST configuration-listener routes are read exceptions. Unknown mutating routes fail closed for ordinary users. Enabled accounts with no roles remain valid read principals.

`d3717ad` preserves the subsequent RED for nine unsafe public-path exceptions. Public health/state/static paths now allow GET/HEAD reads only; unknown health/actuator paths do not inherit public access. The known GET/POST login routes remain public.

`30dfd0f` preserves the subsequent real startup/HTTP RED for unsafe bootstrap configuration. Auth-enabled startup now validates configuration before opening storage. Login and token parsing apply the same validation: an explicit JWT key of at least 32 bytes (the old public repository key and padded keys are rejected), nonblank unpadded seed username, seed password of at least 12 Unicode characters and at most 72 bcrypt bytes without padding, and TTL 1–86400 seconds. The environment loader no longer falls back to the public signing key. These checks cannot attest key entropy or that an operator kept a value private.

The existing multi-replica claim was also corrected: ConfigHub's timer reads its process-local index, not the database. The documentation now requires one active configuration instance; a server-enforced routing fence and cross-replica consistency remain future work.

Independent review rejected `534de422`: existing databases retained the public `nacos` administrator hash even with private bootstrap configuration. `7989cbe` preserves the real PostgreSQL upgrade RED, including unchanged stored hashes. Startup now checks every enabled ROLE_ADMIN account and refuses a public seed credential or malformed bcrypt hash before serving. It does not replace existing hashes from environment values. The same 12-character/72-byte/unpadded, valid-UTF-8 contract now gates auth-enabled login and administrator account create/update. Disabled legacy accounts do not grant admission; an explicit rotation performed only inside the synthetic fixture restores startup/login. Existing non-admin weak hashes are retained but cannot log in with a below-contract password.

## Authoritative verification

Use `upgrade-verification.json`, the matching `upgrade-*.log` files, `upgrade-coverage.out` and `upgrade-coverage-functions.txt` for the final source hashes and actual commands/exits. Files named `green-*`, `final-*`, `candidate-*` and `release-*` are preserved earlier checkpoints; their names do not signify acceptance of the later persisted-credential fixes. The earlier release manifest retains the separate unchanged-module audit and scanner download failure/retry.

Go 1.27.2 on macOS arm64 ran the real loopback PostgreSQL 17.11 fixture under a NOSUPERUSER/NOCREATEDB/NOCREATEROLE/NOBYPASSRLS test role. The role owns this dedicated test database/tables; this is not an RLS or MySQL acceptance claim. `GOACOS_TEST_PG_DSN` is supplied privately; the fixture refuses non-loopback endpoints and database names outside `w038_*`. Passwords and tokens stay in memory and are excluded from logs and committed manifests.

Final five-package race/coverage replay passed 35 top-level tests and 139 nested tests, with zero skips, including nine API tests. Full unit tests, vet, module verification and the Linux arm64 binary build passed. New/changed admission functions have measured coverage: public-path and admin policy 100%, configuration/password validation 100%, token extraction 83.3%, JWT parsing 91.7%, live authentication 93.3%, persisted admin-state validation 83.3%, live role storage 85.7%. The selected five-package total is 47.4%; no repository-wide 80% claim is made.

The first final source vulnerability scan failed fetching the public vulnerability index with EOF; its exit 1 and log are retained. The recorded retry passed with zero reachable/imported vulnerability findings. The built Linux arm64 binary scan passed. The separate module-level JSON scan reported one required-module finding, `GO-2026-5932` (unmaintained OpenPGP in x/crypto, no upstream fix), with JSON-mode exit 0; this is not a clean module audit. Historical/fixed/withdrawn OSV payloads received from the database are distinct from affected findings. A rejected module-scan command and collector exit 1 are retained as a tool failure, not a successful scan.

To replay the essential real-storage check, provide a dedicated private loopback fixture and run:

```sh
go test -race -count=1 -tags=integration -v ./internal/api ./internal/core ./internal/storage ./internal/config ./internal/app
go test -count=1 ./...
go vet ./...
go mod verify
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o /tmp/goacos-auth-review .
govulncheck ./...
govulncheck -mode=binary /tmp/goacos-auth-review
```

The parser options are defined in the pinned `github.com/golang-jwt/jwt/v5` source and its [official parser documentation](https://github.com/golang-jwt/jwt/blob/main/_autodocs/api-reference/parser.md). The recorded product hashes identify the tested source despite the final evidence commit occurring afterward.

## Compatibility, rollback and remaining gates

Auth-enabled installations need private bootstrap configuration, acceptable persisted admin state, and administrators for registration/configuration writes; the old ordinary-user write and self-password-change behavior is deliberately removed. Unsafe existing accounts require an explicitly authorized credential rotation; changing seed environment variables does not rotate them. No database migration or production credential change is part of this slice. Reverting these product commits would restore the demonstrated admission vulnerabilities and must not be treated as a safe enterprise rollback.

Auth-disabled compatibility mode remains unsecured and is the environment default. Read routes still lack namespace/service ACL enforcement, registration ownership is absent, and concurrent role revocation is checked at request admission rather than serialized with a later business commit. MySQL replay, configuration single-active fencing, cross-node consistency, container/image qualification and production deployment remain unverified. No production host, provider or business side effect was used.
