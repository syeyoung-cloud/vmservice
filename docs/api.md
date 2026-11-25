# VMService HTTP API

Everything is served by the one binary on one origin. The UI is at `/`, the API is under `/api/`, and
sign-in is under `/auth/`. JSON in and out. An error is `{"detail": "<message for the user>"}` with a
4xx/5xx status.

The session is an encrypted cookie that holds the user's stable id (derived from the issuer and the
ID token's `sub`), the username and an expiry; nothing is stored on the server. Machines belong to the
stable id, so a username that is freed and signed up again doesn't inherit anything. The group is checked once, at sign-in. Every `/api/` request without a valid session gets `401`.
A write (POST, DELETE) must send `X-Requested-With: XMLHttpRequest`, and if it carries an `Origin`,
that must be the public origin. Otherwise it gets `403`.

## Sign-in

| | |
| --- | --- |
| `GET /auth/login?next=<path>` | redirects to the OIDC provider; after the callback, back to `next` (a local path; default `/`) |
| `GET /auth/callback` | the OIDC redirect URI |
| `POST /auth/logout` | clears the cookie; `204` |
| `GET /config.json` | `{"title": "VMService", "loginProvider": "Keycloak"}`: what the UI is told about the deployment |

## Session and catalog

`GET /api/session` → `200 {"username": "alice", "expires": "2026-11-19T10:00:00Z"}` or `401`.

`GET /api/catalog` →

```json
{
  "images": [{"id": "ubuntu-24.04", "name": "Ubuntu 24.04", "defaultUser": "ubuntu"}],
  "sizes": [{"id": "small", "name": "Small", "cpu": 1, "memoryMiB": 2048}],
  "disk": {"minGiB": 10, "maxGiB": 100, "defaultGiB": 20},
  "limits": {"vms": 3, "cpu": 8, "memoryMiB": 16384, "diskGiB": 200}
}
```

The limits cover every VM the caller owns, running or not. The UI adds up usage from the machine list.

## Machines

A machine:

```json
{
  "id": "vm-7k2q9x",
  "name": "my box",
  "image": "ubuntu-24.04",
  "imageName": "Ubuntu 24.04",
  "defaultUser": "ubuntu",
  "size": "small",
  "cpu": 1,
  "memoryMiB": 2048,
  "diskGiB": 20,
  "state": "Running",
  "detail": "",
  "createdAt": "2026-11-12T10:00:00Z"
}
```

`state` is one of:
- `Provisioning`: the disk is being cloned
- `Starting`
- `Running`
- `Stopping`
- `Stopped`
- `Error`
- `Deleting`

`detail` is a human-readable reason when there is one, e.g. a failure message.

| | |
| --- | --- |
| `GET /api/vms` | `{"vms": [machine, ...]}`: the caller's machines, newest first |
| `GET /api/vms/{id}` | a machine; `404` if it doesn't exist or isn't the caller's |
| `POST /api/vms` | body `{"name", "image", "size", "diskGiB", "password"}` or, from media, `{"name", "media", "profile", "size", "diskGiB", "password"}` → `201` machine. `400` for a bad field, `403` when over a limit. Creates run one at a time, so the limits hold however many arrive at once |
| `POST /api/vms/{id}/start` | `204` |
| `POST /api/vms/{id}/stop` | `204` |
| `POST /api/vms/{id}/restart` | `204` |
| `DELETE /api/vms/{id}` | `204`; the machine and its disk are deleted |

Field rules for `POST /api/vms`:
- **`name`:** 1–63 characters of any text. It's only a display name.
- **`image` and `size`:** ids from the catalog.
- **`diskGiB`:** an integer within `disk.minGiB` to `disk.maxGiB`.
- **`password`:** empty, or 8–128 characters: the default user's password for the console login, stored only as a SHA-512 crypt hash. Empty is allowed only when the caller has an SSH key (`400` otherwise): the user then signs in with SSH keys only.

### Live updates

`GET /api/vms/events` is a server-sent event stream of the caller's machines:
- **`snapshot`:** sent first. Data is `{"vms": [machine, ...]}`.
- **`vm`:** a machine was created or changed. Data is the machine.
- **`deleted`:** data is `{"id": "vm-7k2q9x"}`.

The stream ends when the session expires. `EventSource` reconnects by itself, and a `401` on reconnect means the user has to sign in again.

### Consoles

- **`GET /api/vms/{id}/serial`:** websocket. The serial console (ttyS0) as raw bytes in binary frames, both ways.
- **`GET /api/vms/{id}/vnc`:** websocket. The VNC (RFB) stream, for noVNC.

A member may have `-max-streams` event streams and consoles open together (default 8); one more gets
`429`.

Both must be opened with the subprotocol `plain.kubevirt.io`. They are KubeVirt's VMI `console`/`vnc`
subresources passed through as they are. Both need the machine to be `Running`; otherwise the upgrade
gets `409`. Both close when the session expires.

## SSH keys

A member's public keys: put into each machine they create from then on (cloud-init
`ssh_authorized_keys`), and accepted by the SSH bastion. A key belongs to one member only, so it
identifies them at the bastion.

A key:

```json
{"id": "3f1c…64 hex…", "fingerprint": "SHA256:Pz6…", "type": "ssh-ed25519", "label": "laptop", "createdAt": "2026-11-12T10:00:00Z"}
```

| | |
| --- | --- |
| `GET /api/keys` | `{"keys": [key, ...]}`: the caller's keys, newest first |
| `POST /api/keys` | body `{"publicKey": "ssh-ed25519 AAAA… comment", "label": "laptop"}` → `201` key. `400` for a key that isn't one OpenSSH public key line (ed25519, ECDSA, or RSA ≥ 2048 bits; no options) or a bad label, `409` when the key is already registered (by anyone), `403` at 20 keys |
| `DELETE /api/keys/{id}` | `204`; `404` if it isn't the caller's. Machines made earlier keep the key; the bastion stops accepting it |

`label`: 0–63 characters, display only. The comment on the key line is dropped; the label is shown instead (the comment is the default label when none is given).

`GET /config.json` also says how to reach the bastion: `"ssh": {"host": "cloud.example.org", "port": 222}`, or `"ssh": null` when it's off.

## SSH bastion

`ssh -J <host>:<port> <user>@<machine id>`, e.g. `ssh -J cloud.example.org:222 ubuntu@vm-7k2q9x`.
The jump host takes any username: the key says who you are. It allows nothing but forwarding to port
22 of a running machine of yours (no shell, no other ports or hosts). The host name is the machine id,
or its display name when that's a valid host name and no other machine of yours has it (ignoring case).

## Media

ISOs (installers) and disk images members bring, plus shared ones from the catalog. A machine can be
made from one (`POST /api/vms` with `media`).

A medium:

```json
{
  "id": "md-3kq8zp",            // shared ones: the catalog id, e.g. "win11-eval"
  "name": "Windows 11",
  "shared": false,
  "source": "upload",           // "upload", "url" or "shared"
  "kind": "iso",                // "iso", "disk" or "" (not known yet)
  "profile": "windows",         // "windows", "linux", "uefi", "bios" or "" (detected; a machine may override)
  "sizeBytes": 6123456789,      // the file; for disks, "virtualBytes" is the disk it holds
  "virtualBytes": 6123456789,
  "state": "Ready",             // "Uploading" (waiting for the file), "Importing", "Inspecting", "Ready", "Failed"
  "progress": "45.2%",          // while importing/uploading, else ""
  "detail": "",                 // why it failed, when it did
  "createdAt": "2026-11-13T10:00:00Z"
}
```

| | |
| --- | --- |
| `GET /api/media` | `{"media": [medium, ...]}`: the caller's and the shared ones, newest first |
| `GET /api/media/{id}` | a medium (the caller's or a shared one) |
| `POST /api/media` | upload: body `{"name", "source": "upload", "sizeBytes", "header"}` where `header` is the base64 of the file's first 64 KiB (the browser reads it locally; it says whether this is an ISO or a disk image and how big the disk is). URL: `{"name", "source": "url", "url"}` (http or https; CDI downloads it in the cluster). → `201` medium |
| `PUT /api/media/{id}/upload` | the file as the raw body, `Content-Length` set, once the medium was created with `source: "upload"`. Stream it (e.g. `XMLHttpRequest` with `upload.onprogress`); answered when the upload is done. `409` if it isn't waiting for a file |
| `DELETE /api/media/{id}` | `204`; the caller's own only. Machines made from it keep their copy |

`name`: 1–63 characters, display only. Media changes come over the same event stream as machines:
event `medium` (data: the medium) and `medium-deleted` (data: `{"id"}`).

Machines from media:
- **`kind: "disk"`:** the disk is copied into the machine's root disk and booted, with the same
  cloud-init disk as images (keys, password and host name apply if the image runs cloud-init).
  `diskGiB` must be at least the image's `virtualBytes`.
- **`kind: "iso"`:** the machine gets a blank root disk of `diskGiB` and the ISO as a CD-ROM to
  install from (over VNC). `profile: "windows"` adds the virtio driver CD, UEFI with Secure Boot, a
  TPM and Hyper-V settings, and no cloud-init (the installer sets the password); others attach the
  cloud-init disk too.
- `profile` (optional) overrides the detected one.
- Such machines carry `"media"`, `"profile"` and `"installMedia": true` while the CD-ROMs are attached.

`POST /api/vms/{id}/eject` → `204`: removes the CD-ROMs (the ISO and the driver CD) so the machine boots
from its disk; it restarts if it's running.

## v0.6: images, presets, admin (supersedes the image/media split above)

**Images.** Everything a machine is made from is an *image* (the Media endpoints, under their new
name): an ISO or a disk image, uploaded or imported from a URL, owned by a member, optionally
**public**. The catalog no longer lists images; `GET /api/catalog` has no `images`, and
`POST /api/vms` takes `{"name", "image", "profile"?, "size", "diskGiB", "password"}` where `image` is an
image id (`md-…`). The old `/api/media` paths stay as aliases of `/api/images`.

An image (as a medium before, plus):

```json
{
  "owner": "alice",              // the owner's username (for public images and admins)
  "mine": true,                  // the caller owns it
  "public": true,
  "cloudConfig": {"apt": {}},    // extras merged into machines' #cloud-config (a "preset"); {} if none
}
```

| | |
| --- | --- |
| `GET /api/images` | `{"images": [...]}`: the caller's, then everyone's public ones (admins: all) |
| `PATCH /api/images/{id}` | owner or admin: body any of `{"name", "public", "cloudConfig"}` → image. `cloudConfig` is an object; keys VMService sets itself (`hostname`, `users`, `chpasswd`, `ssh_pwauth`, `ssh_authorized_keys`) are refused (`400`) |
| `DELETE /api/images/{id}` | owner or admin |

Events: `image` / `image-deleted` (the old `medium` / `medium-deleted` names are gone).

**Your account on machines.** Every machine made from a disk image (or a Linux ISO) gets an
account named after your username, with your SSH keys, your password if you set one, passwordless
sudo and bash (cloud-init `users:`), so `ssh -J … <username>@<machine>` works on every distro whose
image runs cloud-init. Windows: none (Setup makes the account).

| | |
| --- | --- |
| `GET /api/account` | `{"username": "alice", "uid": 10001 \| null}` |
| `PUT /api/account` | `{"uid": 10001 \| null}` (1000–60000; null: the distro picks): used for machines made afterwards |

Machines carry `"user": "alice"` (the account to sign in as) instead of `defaultUser` (kept, = user).
Password: optional with an SSH key, required without one, as before; not for Windows ISOs.

**Admin** (the session's `"admin": true` in `GET /api/session`, from the OIDC group configured as
admins). Admins:
- `GET /api/admin/vms` → `{"vms": [machine + "owner"...]}` (every member's), and every machine
  endpoint (`GET`, actions, `DELETE`, consoles, eject) works on any machine for them;
- `GET /api/admin/members` → `{"members": [{"owner", "username", "usage", "quota", "signedInAt"}]}`
  where `quota` is `{"vms", "cpu", "memoryMiB", "diskGiB"}` with `null` fields meaning the default;
- `PUT /api/admin/members/{owner}/quota` → body the quota (nulls: default) → `204`;
- see, edit and delete every image, and make any public;
- have no limits themselves.
`GET /api/catalog` also returns `"limits"` = the caller's effective quota. Admins also get every
machine's events (`vm` events of other members' machines carry `"owner"`).

### v0.6 addendum: machine names are hostnames; the bastion is open

- A machine's `name` is its hostname: a DNS label (lowercase letters, digits, `-`; 1–63; not
  starting or ending with `-`; not of the form `vm-xxxxxx`), unique across all members. It's set at
  create and used as the VM's hostname. `POST /api/vms` answers `409` when it's taken.
- `GET /api/names/{name}` → `{"available": true|false, "reason": "..."}` for the create form (reason
  when not available: invalid or taken).
- The bastion takes anyone: `ssh -J <host>:<port> <user>@<machine name>` (or the machine id). It
  doesn't check keys at the jump hop; the machine's own sshd signs you in (your account and keys from
  cloud-init). SSH keys registered in the portal are what new machines get; they no longer matter
  to the jump hop.

## v0.7: identities and the metadata service

A machine wears an **identity**: what it is to other services. An identity is a ServiceAccount of
the machines' namespace, `identity-<name>-<8 random characters>`, never reused; its tokens are issued
by the cluster (`https://kubernetes.syeyoung.dev`) with subject
`system:serviceaccount:<namespace>:<account>`. A member owns it; several machines may wear one and
share what is granted to it; every machine wears exactly one. Deleted and made again under the same
name, it is another identity, with another subject.

Identity: `{"name", "account", "subject", "issuer", "owner", "mine", "usedBy": [machine names], "createdAt"}`.

| Call | |
|---|---|
| `GET /api/identities` | `{"identities": [identity, ...]}`: the caller's (admins: all), oldest first |
| `POST /api/identities` | body `{"name"}` (a DNS label, 1–63) → `201` identity. `409` if the caller has one by that name, `403` at 100 |
| `DELETE /api/identities/{account}` | `204`; owner or admin. `409` while a machine wears it |
| `POST /api/vms` | also takes `"identity"`: the name of one of the caller's identities. Absent or `""`: a new identity named after the machine (`409` if the caller already has one by that name) |
| `PUT /api/vms/{id}/identity` | body `{"identity": "<name>"}`, one of the machine owner's → `204`. From the machine's next start (Restart or Stop/Start here) |
| `DELETE /api/vms/{id}?identity=delete` | also deletes its identity, once the machine is gone, if no other machine wears it then |
| `PUT /api/vms/{id}/size` | body `{"size": "<catalog size id>"}` → `204`, within the owner's limits; from the machine's next start |

A machine's JSON carries `"identity"` (its name) and `"identityAccount"`. Machines from before
identities get one of their own, named after them, when the service starts.

### Tokens, from inside a machine

```sh
curl -s -H 'Metadata: true' 'http://169.254.169.254/v1/token?audience=https://files.drive.syeyoung.dev'
# → {"token": "<JWT>", "expiresIn": 3599, "issuer": "https://kubernetes.syeyoung.dev",
#    "subject": "system:serviceaccount:syeyoung-cloud:identity-build-k2m9xqtz"}
```

Exactly one `audience`; the token is made out to it only, lasts one hour, and is bound to the
machine's running instance (it stops being valid when the machine stops). `Accept: text/plain`
answers the token alone. Asking again within the hour gives the same token. Without the
`Metadata: true` header, or through a proxy (`Forwarded`, `X-Forwarded-*`, `Via`), it's refused
(`400`). Windows and Linux machines alike.
