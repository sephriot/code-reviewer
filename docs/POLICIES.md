# Review policies

Policies decide **which pull requests enter a workflow** and **which immutable
review profile applies**. They do not contain tokens, shell commands, or a
GitHub write capability.

Apply one complete generation at a time:

```bash
go run ./cmd/reviewctl policy apply \
  --database data/control-plane.db \
  --generation 1 \
  --rules-file examples/review-profiles/baseline-policy.json
```

Each generation replaces the active rule set. Omitted rules become disabled;
old versions remain retained for audit and repeatability.

## Rule shape

```json
{
  "key": "backend-assigned",
  "enabled": true,
  "priority": 10,
  "trigger_kind": "automatic",
  "external_action_policy": "require_confirmation",
  "profile_key": "baseline",
  "profile_version": 1,
  "match": {"repository_names":["acme/backend"],"relationships":["review_requested"]},
  "review": {},
  "publication": {"allow_automatic_approval":false}
}
```

Lowest numeric `priority` wins. Ties use stable rule ID. Only first matching,
enabled rule applies.

## `match`: selection predicates

`match` answers: “does this rule apply to current PR facts?” `{}` matches every
valid PR. Every supplied predicate combines with AND.

Supported fields:

| Field | Type | Match meaning |
| --- | --- | --- |
| `relationships` | non-empty string array | PR has **all** listed relationships, such as `review_requested`. |
| `repository_ids` | non-empty number array | PR repository ID matches any value. |
| `repository_names` | non-empty string array | `owner/repo` matches any value. `owner/*` matches every repository in one organization. |
| `authors` | non-empty string array | author login matches any value. |
| `labels` | non-empty string array | PR has **all** listed labels. |
| `is_draft` | boolean | draft state equals value. |
| `states` | non-empty string array | state matches any value, normally `open`, `closed`, or `merged`. |
| `base_refs` | non-empty string array | base branch matches any value. |

Unknown fields, duplicate fields, empty arrays, malformed values, or invalid
current facts fail closed. Use repository IDs when a rename-resistant selector
matters; use names when human readability matters.

To cover an entire organization, use the one supported wildcard form:

```json
{"repository_names":["spacelift-io/*"]}
```

Wildcard matching is case-insensitive and matches exactly one owner segment;
general glob patterns and regular expressions are not supported.

## `review`: retained execution contract

`review` is an immutable strict JSON object recorded with rule version. Today
it is retained for future engine/scheduling controls but is **not interpreted
by the runtime**. Use `{}` unless a later documented release supports a field.

For example, `{"access_mode":"diff_only"}` is retained evidence, not a
switch that changes engine access today. Actual review execution remains bound
to the profile version and trusted `REVIEWD_REVIEW_ENGINE_ARGV` configuration.

## Other controls

- `trigger_kind`: `automatic` queues eligible matched reviews; `manual` needs
  an explicit request; `track_only` records/observes without profile execution;
  `ignore` excludes from review workflow.
- `profile_key` and `profile_version`: required for `automatic` and `manual`;
  both select an immutable profile version.
- `external_action_policy`: controls policy outcome. Prefer
  `require_confirmation`; human approval remains required before publication.
- `publication`: retained policy settings. `allow_automatic_approval:false` is
  safe default. Publication mode still gates every external action.

## Safe starting policy

Start with `examples/review-profiles/baseline-policy.json`: it matches all
current canonical PRs, requires human confirmation, and cannot automatically
publish. Narrow `match` first when enabling it in a real repository.

## Validate changes safely

Use a new positive generation number. Re-applying identical content is
idempotent; changing content under an existing generation fails closed.

```bash
go run ./cmd/reviewctl policy evaluate --help
go run ./cmd/reviewctl policy apply --help
```
