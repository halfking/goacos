# Casdoor registry authentication and scoped authorization

Implementation status: the isolated211 fixture has exercised this mode against its private Casdoor realm. That realm advertises the public issuer but has a different signing-key set, so its thirty-check business acceptance and scoped registry tests do not prove public issuer interoperability. Public-authority migration requires separate identity, policy and runtime verification.

## Explicit provider selection

`GOACOS_AUTH_PROVIDER=local` is the default (empty programmatic Config also means local). Existing local authentication and its startup validation remain unchanged. Select `GOACOS_AUTH_PROVIDER=casdoor` together with `GOACOS_AUTH_ENABLED=true` to use Casdoor exclusively. Unknown providers, incomplete settings and Casdoor with authentication disabled fail before opening storage.

| Environment variable | Meaning |
| --- | --- |
| `GOACOS_CASDOOR_ENDPOINT` | HTTPS Casdoor origin; no userinfo, path, query or fragment |
| `GOACOS_CASDOOR_ISSUER` | Exact accepted issuer |
| `GOACOS_CASDOOR_CLIENT_ID` | Registry verifier application client ID; required audience for human users |
| `GOACOS_CASDOOR_CLIENT_SECRET` | Registry verifier private application credential; provision privately |
| `GOACOS_CASDOOR_ORGANIZATION` | Exact accepted organization |
| `GOACOS_CASDOOR_PERMISSION` | `<organization>/<permission>` for a single permission; mutually exclusive with MODEL |
| `GOACOS_CASDOOR_MODEL` | `<organization>/<model>` aggregating granular permissions; exactly one of MODEL/PERMISSION required |
| `GOACOS_CASDOOR_MODEL_RESPONSE_GROUP` | Optional exact model/empty-adapter response group; defaults to `<model-short-name>/`. Only that value or `<organization>/<model>/` is valid, and MODEL must be set |
| `GOACOS_CASDOOR_APPLICATIONS` | Optional JSON object mapping explicitly trusted machine application names to their client IDs; duplicate keys/invalid JSON rejected |
| `GOACOS_CASDOOR_ALLOW_HTTP` | Default false; use true only for the isolated mesh lab |

Application allowlisting grants no resource permission. Each machine still needs an exact Casdoor permission. Casdoor applications used with the verifier must use a compatible signing certificate; this integration uses the existing v2.63 RS256/introspection contract, not an arbitrary OIDC provider contract. Do not place secrets or tokens in URLs, Git, image build contexts or diagnostic logs.

Casdoor mode disables local login/user/role APIs and rejects local tokens. Local account seed/checks are skipped; existing local account rows are preserved and cannot authenticate through this mode. Switching back to local requires its complete configuration and persisted credential validation. No automatic fallback occurs when Casdoor is unavailable.

## Admission sequence

1. Require a single Authorization Bearer header; cookies/query tokens do not authenticate.
2. Resolve a bounded, canonical, exact resource from a supported request shape.
3. Send the exact RS256-shaped token to Casdoor introspection with `access_token` hint. Validate active state, issuer, audience, type and time bounds.
4. Re-read the current user identity/flags or explicitly trusted machine application. Bind all returned identity fields to the introspected token.
5. Call Casdoor enforce with `[owner/name, resource, action]`; require exactly one true result. Model mode additionally requires exactly one `data2` group matching `GOACOS_CASDOOR_MODEL_RESPONSE_GROUP`, or `<model-short-name>/` when unset. This binds the dedicated model and built-in empty adapter; no fallback between short and qualified forms occurs. Never assign global administrator status.
6. Continue through existing configuration ownership/storage admission and then the handler.

No positive identity/policy cache is used. Revocation is checked at admission, not serialized with the subsequent database commit. Upstream failure returns503; invalid identity, unsupported request shape or denied policy returns403. Only exact GET health/readiness/liveness endpoints are public in this mode.

## Initial route contract

| Method | Path | Resource/action |
| --- | --- | --- |
| GET | `/nacos/v1/cs/configs` | `goacos/config/<tenant>/<group>/<dataId>` / read |
| POST, DELETE | `/nacos/v1/cs/configs` | same / write |
| GET | `/nacos/v1/ns/instance/list` | `goacos/service/<namespaceId>/<groupName>/<serviceName>` / read |
| POST, DELETE | `/nacos/v1/ns/instance` | same / write |
| PUT | `/nacos/v1/ns/instance/beat` | same / write |

All scope components are explicit,1–255 ASCII letters/digits/dot/underscore/hyphen; `.` and `..` are invalid. No wildcard, slash, implicit default, alias, group-in-service `@@`, duplicate parameter or mixed scope source is allowed. Naming uses query parameters and an empty body. Configuration publication uses a known-length form body, at most1MiB, preserving downstream parsing. Config reads with `search` or `pageNo` are rejected because they would switch the handler to a broad list.

HEAD, v2, broad service/config lists, history, listener batches, namespace administration, Goacos app/route/certificate management and console login are not yet supported in Casdoor mode. They are denied rather than inheriting global read/admin access. Add each API only with a resource contract and tests that authorization matches the actual executed target. Namespace catalog provisioning therefore precedes this initial mode or requires a future scoped management API.

## Exact policy example

Use an exact-equality Casbin model (`r.sub == p.sub && r.obj == p.obj && r.act == p.act`) selected by `GOACOS_CASDOOR_MODEL`. A single Permission expands subjects/resources/actions as a Cartesian product; do not place all four roles and read/write into one record. Do not use wildcard matching to compensate for missing grants. For the reference lab:

- Publisher: read and write `goacos/config/wm-auth-chain/WM_CHAIN/wm-auth-chain.json` for compare-and-preserve publication/rollback; also read `goacos/service/wm-auth-chain/WM_CHAIN/ask-service` for the current deployment readiness check.
- Gateway: read the configuration above and `goacos/service/wm-auth-chain/WM_CHAIN/ask-service`.
- Service: write the service resource above (registration, heartbeat, deregistration).
- Verifier: read both resources; no writes.

Give each machine a separate Casdoor application and policy subject. A service write grant applies to that service's instances, not individual instance ownership. If instance-level ownership is required, extend the resource model before accepting untrusted registrants.

Gateway configuration requests use `tenant`; naming discovery must use `namespaceId`. The wm-deploy ask client already sends canonical query-only instance requests. Token files support rotation but token issuance/renewal automation and live federation acceptance are separate remaining tasks.

## Verification and rollout

Run `go test -race ./internal/config ./internal/core ./internal/api ./internal/storage ./internal/app` and `go vet ./...`. The default tests use HTTP fixtures and a bounded SQL test driver; they do not prove live database/Casdoor interoperability. Integration-tagged database tests must be run explicitly against dedicated fixtures.

Before211 rollout, provision exact Casdoor policies and independent machine applications, mint private role token files, preserve the current registry container/config/credential files and prepare rollback. Verify cross-scope read/write denial, machine grants, identity/policy revocation, configuration publishing and dynamic A/B discovery on the real stack. Keep shared8848 and public ingress unchanged. Never reinterpret prior local-token acceptance as proof of this new mode.

Reference: [Casdoor v2.63 introspection controller](https://github.com/casdoor/casdoor/blob/v2.63.0/controllers/token.go).


### Granular permission aggregation

For organization `wm-registry-lab`, model `registry-exact`, set `GOACOS_CASDOOR_MODEL=wm-registry-lab/registry-exact` and leave PERMISSION unset. The model defines `r=sub,obj,act`, `p=sub,obj,act,eft`, allow-if-any-allow effect and exact subject/object/action equality. Permission records use `adapter:""` and `effect:"Allow"`. Their model field and the expected response group must match the selected Casdoor authority:

- The private lab uses `model:"registry-exact"` and returns `registry-exact/`; leaving MODEL_RESPONSE_GROUP unset preserves that behavior.
- The native authority uses `model:"wm-registry-lab/registry-exact"` and returns `wm-registry-lab/registry-exact/`. Set `GOACOS_CASDOOR_MODEL_RESPONSE_GROUP=wm-registry-lab/registry-exact/` explicitly. With this setting the short response is rejected, just as the default rejects the qualified response.

The group includes the final slash for the empty adapter. Another organization/model, an external adapter, missing group or multiple groups is rejected; the setting is invalid without MODEL. This compatibility setting selects an exact response representation, not a different policy or identity authority. Verify public issuer metadata/signing keys before provisioning; matching group text alone does not establish trust.

| Permission | Subjects | Resources | Actions |
| --- | --- | --- | --- |
| registry-read | publisher, gateway, verifier machine application subjects | exact config and service resources above | read |
| registry-publish | publisher machine application subject | exact config resource | write |
| registry-register | service machine application subject | exact service resource | write |

Each subject is `admin/<application-name>`. `/api/enforce?modelId=wm-registry-lab/registry-exact` combines these records; empty, multiple or differently grouped results fail closed. External adapters are not supported in this initial model mode. In the inspected v2.63 policy aggregation path, `isEnabled` is not a sufficient revocation mechanism: remove the permission or its matching grants and verify subsequent denial. Deployment must prove the whole role/resource/action matrix, including negative cross-scope cases.

Sources: [v2.63 selector and aggregation](https://github.com/casdoor/casdoor/blob/v2.63.0/controllers/casbin_api.go), [policy expansion](https://github.com/casdoor/casdoor/blob/v2.63.0/object/permission_enforcer.go).

The 2026-11-09 isolated public-authority reference on211 uses native Casdoor28001 via HTTPS auth.itestu.cn and a separate registry28450. It passed32 business checks,39 resource-scope case rows and144 model decisions; scheduled four-role token rotation followed by old-token revocation retained live gateway sync and service heartbeats without container restart. This covers the ask-service reference only; it does not certify other application migrations or a production ingress cutover. Detailed evidence is maintained in wm-deploy docs/31 and docs/evidence/public-native-chain-211-2026-11-09.json. Earlier28000/28449 evidence remains private-realm-only.
