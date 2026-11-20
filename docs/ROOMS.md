# Voice rooms: registry, permissions, IRC role locks

quicvoiced keeps a **room registry**:

- **static** rooms from `voice.rooms` in the stack config file (`BOSON_CONFIG`, reloaded live) or else `VOICE_ROOMS` (read-only through the API; edit the file or the env), and
- **dynamic** rooms people create through the public API, optionally under an
  IRC channel (`parent`), Discord-style.

With `VOICE_ROOMS_STRICT=true` (the default) only registry rooms can be joined
(`ROOM_NOT_FOUND` otherwise). The registry is stored in `VOICE_ROOMS_DB`
(a JSON file, rewritten atomically on each change; mount a volume). Without it
the registry is in memory and dynamic rooms are lost on restart.

On every start the registry is reconciled with `VOICE_ROOMS`: each listed room
is written as static at its list position (renames apply), static rooms no
longer listed are removed, and dynamic rooms are kept. (A dynamic room whose
id is later added to `VOICE_ROOMS` becomes static.) The same reconcile runs
live whenever `voice.rooms` in the config file changes; people in a removed
static room are kicked with `voice channel "<name>" was removed from the server
config` (README: Config file).

## Who can do what

### Network roles and permissions (voice token claims)

The auth service's voice token (`POST /v1/voice/token`) can carry two claims
from its network roles:

| Claim | | |
|---|---|---|
| `roles` | `["Owner", "Moderator", "DJ"]` | Names of the network roles the user holds (display names, compared case-insensitively). Used by the `allowed_roles` lock |
| `perms` | `["voice.create", "voice.manage_any", …]` | Permission strings. quicvoiced uses the three below and ignores the rest |

| Permission | Grants |
|---|---|
| `voice.create` | Creating rooms at all, where the `voice_channel_creation` policy allows (see below). Needed in addition to the policy |
| `voice.manage_any` | Create anywhere under every policy; rename/reorder/re-lock/delete **any** dynamic room; exempt from `VOICE_ROOMS_MAX_PER_USER`; may set `oper` locks |
| `voice.bypass_locks` | Join and speak in every room regardless of `min_role`, `speak_role`, `require_channel_membership` and `allowed_roles` (also `oper` locks) |

**Old and new tokens.** A token *with* a `perms` claim (even `[]`) is a
"new" token and every network-level decision uses its perms only: the
legacy `role` claim no longer makes anyone a network admin (it still
decides `listener` vs speaker). A token *without* `perms` (minted before
network roles) is judged exactly as before: `role` `moderator`/`admin` =
network admin, which counts as `voice.manage_any` + `voice.bypass_locks`
and as `oper`/founder in every channel. A missing `roles` claim means "holds
no roles".

| Decision | Token with `perms` | Token without `perms` (legacy) |
|---|---|---|
| Network admin (manage any room, per-user limit exemption, set `oper` locks) | `voice.manage_any` | `role` `moderator`/`admin` |
| Pass every lock | `voice.bypass_locks` | `role` `moderator`/`admin` |
| `oper` role / founder of every channel | never (real IRC role only; `oper` locks are passed with `voice.bypass_locks`) | `role` `moderator`/`admin` |
| Allowed to create at all | `voice.create` or `voice.manage_any` | yes (the policy decides) |

### Guest tokens

The auth service can also mint voice tokens for IRC guests with no account
(`POST /v1/voice/guest-token`, when its `auth.guest_voice` setting is on).
They carry `"guest": true`, `sub` = `guest:<id>`, `name` = the guest's IRC
nick (unverified), `role` `speaker` and empty `roles`/`perms`. quicvoiced:

- refuses a token whose `guest` claim and `guest:` subject prefix disagree;
- never treats the nick as an Ergo account (no channel standing: the guest
  is a plain `member` everywhere, even if an account has the same name);
- never makes a guest a network admin, lock bypasser or room creator, and
  caps its role at `speaker`, whatever else the token says.

So guests can join (and speak in) open rooms only; `min_role`,
`require_channel_membership` and `allowed_roles` all keep them out, and
`speak_role` above `member` makes them listeners. Guests show up with
`"guest": true` in `participants` and in the control protocol's
`Participant.guest`.

### Creating rooms: `voice_channel_creation`

The auth service's network setting (admin panel), read from
`GET $AUTH_INTERNAL_URL/internal/v1/settings` (`X-Internal-Token:
$INTERNAL_API_TOKEN`) and cached for `SETTINGS_TTL` (30 s). If the fetch fails
the last good value stays; before the first success, `VOICE_CHANNEL_CREATION`
(default `admins`) is used.

| Setting | Network admins¹ | Everyone else |
|---|---|---|
| `admins` | anywhere | nowhere (`403 forbidden`) |
| `founders` | anywhere | only with `parent` = an IRC channel they are the **founder** of |
| `registered` | anywhere | anywhere, with or without `parent` |

¹ Network admin = `voice.manage_any` on tokens with `perms`; voice token
`role` `moderator` or `admin` on tokens without (the auth service gives its
admins `moderator`).

On tokens with `perms`, `voice.create` is needed on top of the policy.
Precedence, first match wins:

| Policy | Token with `perms` | Token without `perms` |
|---|---|---|
| any | `voice.manage_any` → anywhere | `role` moderator/admin → anywhere |
| any | no `voice.create` → `403 forbidden` | — |
| `admins` | (has `voice.create` only) → `403 forbidden` | `403 forbidden` |
| `founders` | `voice.create` + founder of `parent` → ok; else `403` (`503` if Ergo is down) | founder of `parent` → ok; else `403` (`503`) |
| `registered` | `voice.create` → anywhere | anywhere |

`permissions` in `GET /v1/rooms` follows the same table (`create_scope`
`none` without `voice.create`).

Per-user limit `VOICE_ROOMS_MAX_PER_USER` (10, network admins¹ exempt) and
total `VOICE_ROOMS_MAX` (500) → `403 limit`.

### Renaming, reordering, re-locking, deleting

Allowed for: the room's **owner** (its creator), the **founder of its parent
channel**, or a network admin¹ (`voice.manage_any`). Static rooms: nobody,
not even `voice.manage_any` (`403 static_room`).
The id never changes; the parent can't be changed.

### Roles

Roles are IRC channel privilege levels; Ergo (ChanServ + live channel modes)
is the source of truth. Lowest to highest:

| Role | IRC |
|---|---|
| `member` | anyone (no lock) |
| `voice` | `+v` |
| `halfop` | `%` / `+h` |
| `op` | `@` / `+o` |
| `admin` | `&` / `+a` (channel admin) |
| `founder` | `~` / `+q`, or the ChanServ-registered founder |
| `oper` | network admin |

A user's role in a channel is:

1. `oper` if their token is a legacy (no `perms`) network admin token;
2. `founder` if Ergo says they're the registered founder (works offline);
3. otherwise their **current** prefix mode in that channel, if their IRC client
   is in it (ChanServ `AMODE`s are applied by Ergo on join, so they show up
   here);
4. otherwise `member`.

So someone with an `AMODE +o` who isn't currently in the IRC channel counts as
`member` until they join it (Ergo 2.19's API can't list AMODEs). With the
boson client's always-on IRC connection this is normally a non-issue.

### Locks

| Field | Rooms with `parent` | Rooms without |
|---|---|---|
| `min_role` | role needed to join | `member` (any registered user) or `oper` (admins only) |
| `speak_role` | role needed to talk; below it you join as a **listener** | `member` or `oper` |
| `require_channel_membership` | also be in the IRC channel right now | not allowed |
| `allowed_roles` | hold at least one of these **network** roles (token `roles` claim, case-insensitive); `[]` = no restriction | same |

`allowed_roles` comes on top of the IRC locks (both must pass). Tokens
without a `roles` claim hold no roles, so they're refused from
`allowed_roles` rooms unless they bypass locks (`voice.bypass_locks`, or the
legacy admin `role` on a token without `perms`). Static rooms never have
`allowed_roles`.

`voice.bypass_locks` (legacy: network admin) passes every lock.
Only network admins¹ can set `oper`. Locks are checked at `Join` (refused with
`Error{FORBIDDEN, "…needs op or higher in #chan"}`, or `"…needs one of these
roles: DJ, Owner"` for `allowed_roles`; listeners get
`Participant.role = LISTENER` and their audio isn't forwarded) and reflected
in `GET /v1/rooms` as `can_join` / `can_speak`. They are not re-checked while
someone stays in the room: a later `-o` takes effect on their next join.

If Ergo can't be asked (down, misconfigured), anything needing an IRC role
fails closed: locked rooms show `can_join:false` and refuse joins, founder
checks return `503 unavailable`. Unlocked rooms keep working.

### Founder and role checks against Ergo

quicvoiced uses Ergo's HTTP API (`api` section of `ircd.yaml`, enabled on the
stack's internal network only, bearer token `ERGO_API_TOKEN`):

- `POST /v1/ns/info {"accountName": "<name>"}` → `channels`: the channels
  whose registered founder is that account;
- `POST /v1/whois {"nickname": "<name>"}` → the live client's channels with
  their highest prefix mode. The stack's Ergo forces nick = account name, and
  the response's `account` must match.

`<name>` is the voice token's `name` claim, which is the auth username = the
Ergo account. Both answers are cached together per account for
`ERGO_CACHE_TTL` (15 s), so role/founder changes reach voice within that.

## API

Base: the public listener (`PUBLIC_HTTP_ADDR`, `https://VOICE_HOST`).
Every request: `Authorization: Bearer <voice token>` (verified like the QUIC
`Hello`). Responses are `application/json`, `Cache-Control: no-store`.
Errors: `{"error": "human text", "code": "<code>"}`.

| Status | `code` | When |
|---|---|---|
| 400 | `invalid` | bad JSON, name, parent, role, position, `allowed_roles`, lock combo, or `parent` in PATCH |
| 401 | `unauthorized` | missing/invalid/expired token (+ `WWW-Authenticate: Bearer realm="quicvoice"`) |
| 403 | `forbidden` | policy or permission says no (incl. no `voice.create`, setting `oper` without being a network admin) |
| 403 | `static_room` | change to a `VOICE_ROOMS` room |
| 403 | `limit` | per-user / total room limit |
| 404 | `not_found` | no such room (or excluded by the token's `rooms` claim) |
| 409 | `conflict` | another room under the same parent has that name (case-insensitive) |
| 415 | `invalid` | `Content-Type` isn't `application/json` |
| 503 | `unavailable` | an IRC founder/role check was needed and Ergo is unreachable (`Retry-After: 5`) |

### `GET /v1/rooms` → 200

```json
{
  "permissions": {
    "policy": "founders",
    "can_create": true,
    "create_scope": "founded_channels",
    "founded_channels": ["#boson"],
    "can_manage_any": false,
    "bypass_locks": false
  },
  "rooms": [
    {"id": "general", "name": "General", "kind": "static",
     "parent": null, "owner": null, "position": 0,
     "created_at": "2026-11-13T20:00:00.000Z",
     "min_role": "member", "speak_role": "member", "require_channel_membership": false,
     "allowed_roles": [],
     "can_manage": false, "can_join": true, "can_speak": true,
     "participants": [
       {"session_id": 3, "user_id": "usr_123", "name": "alice",
        "muted": false, "deafened": false, "speaking": true}]},
    {"id": "boson-game-night", "name": "Game Night", "kind": "dynamic",
     "parent": "#boson", "owner": {"user_id": "usr_456", "name": "fred"}, "position": 1,
     "created_at": "2026-11-13T21:04:05.123Z",
     "min_role": "voice", "speak_role": "op", "require_channel_membership": false,
     "allowed_roles": ["DJ", "Owner"],
     "can_manage": true, "can_join": true, "can_speak": true,
     "participants": []}
  ]
}
```

- `permissions.create_scope`: `any` (show "+ voice channel" everywhere),
  `founded_channels` (only under the channels in `founded_channels`, always
  present for this scope, may be `[]`), or `none`. `can_create` is false for
  `none` and for `founded_channels` with no founded channels.
- `permissions.can_manage_any` / `permissions.bypass_locks`: the caller's
  effective `voice.manage_any` / `voice.bypass_locks` (for tokens without
  `perms`: true for network admins). Always present.
- `allowed_roles`: always an array (`[]` = no restriction), also on static
  and ad-hoc rooms.
- Rooms are in display order (`position`, then creation time). With
  `VOICE_ROOMS_STRICT=false`, live ad-hoc rooms follow with `kind: "adhoc"`,
  no owner/parent/`created_at` and no locks.
- `parent` is the casefolded IRC channel (`#boson`), or `null`. Group rooms
  under their channel client-side.
- `can_manage`: the caller may PATCH/DELETE it. `can_join` / `can_speak`:
  the caller's result of the locks above (and the token's `rooms` claim).
- `participants`: as before (sorted by `session_id`; `muted` = self- or
  server-muted; `speaking` = voiced frame in the last second; `guest: true`
  for IRC guests, omitted otherwise).

### `POST /v1/rooms` → 201

```json
{"name": "Game Night", "parent": "#Boson",
 "min_role": "voice", "speak_role": "op", "require_channel_membership": false,
 "allowed_roles": ["DJ", "Owner"]}
```

Only `name` is required (1–32 characters after trimming, no control
characters; unique per parent, case-insensitive). `parent`: an IRC channel
name (`#…`, ≤ 64 bytes, no spaces, commas or `:*?!@`), stored casefolded;
omit or `null` for none. Lock fields default to `member`/`member`/`false`/`[]`.
`allowed_roles`: up to 20 network role names, each 1–64 characters after
trimming, no control characters; duplicates (case-insensitive) are dropped,
keeping the first spelling and the order. Any of those rules broken → `400
invalid`. Allowed with or without `parent`.

Response: the room object as in the list (empty `participants`), plus
`Location: /v1/rooms/<id>`. The id is generated: a slug of parent + name
(`boson-game-night`), `-2`, `-3`… on collision, `room` when the name has no
ASCII letters or digits. It never changes.

### `PATCH /v1/rooms/{id}` → 200

Any subset of:

```json
{"name": "Movie Night", "position": 0,
 "min_role": "member", "speak_role": "voice", "require_channel_membership": true,
 "allowed_roles": ["DJ"]}
```

`position`: 0–1000000 (rooms are sorted by it; equal positions by creation
time). `allowed_roles` replaces the whole list (same rules as POST); `[]`
clears it; `null` or absent leaves it. Sending `parent` is a `400`. Response: the updated room with live
participants. New locks apply to the next join.

### `DELETE /v1/rooms/{id}` → 204

Removes the room. Everyone in it gets `Error{code: KICKED, message: "voice
channel \"Game Night\" was deleted by fred", fatal: false}` on the control
stream and is out of the room (their connection stays open; they can `Join`
another room).

### Try it

```sh
curl -fsS https://VOICE_HOST/v1/rooms -H "Authorization: Bearer $VOICE" | jq .permissions
curl -fsS -X POST https://VOICE_HOST/v1/rooms -H "Authorization: Bearer $VOICE" \
  -H 'Content-Type: application/json' -d '{"name":"Game Night","parent":"#boson"}'
curl -fsS -X PATCH https://VOICE_HOST/v1/rooms/boson-game-night -H "Authorization: Bearer $VOICE" \
  -H 'Content-Type: application/json' -d '{"speak_role":"voice"}'
curl -fsS -X DELETE https://VOICE_HOST/v1/rooms/boson-game-night -H "Authorization: Bearer $VOICE"
```

## Config

| Var | Default | |
|---|---|---|
| `VOICE_ROOMS_DB` | — | Registry file, e.g. `/data/rooms.json` (the image has a writable `/data`). Empty = in memory |
| `AUTH_INTERNAL_URL` | — | Auth internal listener, e.g. `http://auth:8081`. Empty = `VOICE_CHANNEL_CREATION` only |
| `INTERNAL_API_TOKEN` | — | Sent as `X-Internal-Token` |
| `VOICE_CHANNEL_CREATION` | `admins` | Policy before the first settings fetch / without `AUTH_INTERNAL_URL` |
| `SETTINGS_TTL` | `30s` | How long a fetched setting is reused |
| `ERGO_API_URL` | — | Ergo API, e.g. `http://ergo:8089`. Empty = no IRC roles (only admins pass locks and the founders policy) |
| `ERGO_API_TOKEN` | — | One of Ergo's `api.bearer-tokens`. Empty = lookups off (a warning is logged), even with `ERGO_API_URL` set |
| `ERGO_CACHE_TTL` | `15s` | Per-account cache of founder/role lookups |
| `VOICE_ROOMS_MAX_PER_USER` | `10` | Dynamic rooms one non-admin may own |
| `VOICE_ROOMS_MAX` | `500` | Dynamic rooms in total |
