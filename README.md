# zitadel-netbird-sync

Mirrors [Zitadel](https://zitadel.com) projects and their members into
[NetBird](https://netbird.io) groups.

> **Status: experimental.** It works against real accounts, but it is young.
> Start with the dry run, which is the default.

## Why

NetBird can take groups from a JWT claim, but only on self-hosted installations.
NetBird Cloud offers IdP sync for a fixed list of providers and generic SCIM, and
Zitadel cannot push SCIM. This tool closes that gap with the plain NetBird API:
it works on NetBird Cloud and self-hosted alike.

## What it does

On every run it reads both sides completely and applies the difference. It keeps
no state of its own.

- One NetBird group per Zitadel project, named `<prefix><project>`. The group
  is created with its first member; a project nobody in NetBird belongs to gets
  no group unless `CREATE_EMPTY_GROUPS=true`.
- A NetBird user is put into the group if the Zitadel user with the same email
  address holds an active grant on that project, whatever the role.
- When the grant goes, the membership goes at the next run.

## What it never does

- It never touches a group whose name does not start with the prefix. Groups
  you maintain by hand stay exactly as they are, and so do a user's memberships
  in them.
- It never deletes a group. A group whose project disappeared is emptied and
  reported in the log.
- It never creates, blocks or deletes users, and never changes a role.
- It never writes to Zitadel.
- It never writes anything unless `DRY_RUN=false` is set.

## Safety nets

- **Dry run by default.** The plan is logged; nothing changes.
- **Refuses empty input.** If Zitadel returns no projects, the run fails instead
  of removing every membership.
- **Refuses incomplete input.** If Zitadel delivers fewer entries than it
  reports, the run fails.
- **Removal limit.** A run that would remove more than `MAX_REMOVALS`
  memberships changes nothing and fails.
- **Verified addresses only.** A Zitadel user whose email address is not
  verified gets no membership.

## Configuration

Everything is set through environment variables.

| Variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `ZITADEL_URL` | yes | | Base URL of the Zitadel instance, `https://…` |
| `ZITADEL_TOKEN` or `ZITADEL_TOKEN_FILE` | yes | | Personal access token of a Zitadel service user |
| `ZITADEL_ORG_ID` | no | the token's organization | Organization whose projects and grants are read |
| `NETBIRD_URL` | no | `https://api.netbird.io` | NetBird management URL |
| `NETBIRD_TOKEN` or `NETBIRD_TOKEN_FILE` | yes | | NetBird access token |
| `GROUP_PREFIX` | yes | | Marks the groups this tool owns, e.g. `idp-` |
| `GROUP_NAME_LOWERCASE` | no | `true` | Lower-case the project name in the group name |
| `INCLUDE_PROJECTS` | no | all | Comma-separated project names to sync |
| `EXCLUDE_PROJECTS` | no | none | Comma-separated project names to leave out |
| `CREATE_EMPTY_GROUPS` | no | `false` | Also create groups that would have no member |
| `DRY_RUN` | no | `true` | Set to `false` to write |
| `SYNC_INTERVAL` | no | `10m` | Pause between runs, at least `1m` |
| `RUN_ONCE` | no | `false` | Run once and exit; exit code 1 on failure |
| `MAX_REMOVALS` | no | `50` | Largest number of removals one run may make, `0` for no limit |

Project names in the include and exclude lists are matched without regard to
case.

## Permissions

**Zitadel:** a service user that can read projects, users and user grants of the
organization. The built-in role `ORG_OWNER_VIEWER` is meant for this.

**NetBird:** a service user with the `admin` role. Changing a user's groups is
part of user management, and NetBird has no narrower role that allows it.

## Run it

Dry run, once:

```sh
docker run --rm \
  -e ZITADEL_URL=https://id.example.org \
  -e ZITADEL_TOKEN \
  -e NETBIRD_TOKEN \
  -e GROUP_PREFIX=idp- \
  -e RUN_ONCE=true \
  ghcr.io/marcobockelbrink/zitadel-netbird-sync:latest
```

Read the plan in the log. When it is what you expect, run it continuously with
`DRY_RUN=false`.

Kubernetes, tokens mounted from a secret:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: zitadel-netbird-sync
spec:
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels: { app: zitadel-netbird-sync }
  template:
    metadata:
      labels: { app: zitadel-netbird-sync }
    spec:
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        seccompProfile: { type: RuntimeDefault }
      containers:
        - name: sync
          image: ghcr.io/marcobockelbrink/zitadel-netbird-sync:<version>@sha256:<digest>
          env:
            - { name: ZITADEL_URL, value: "https://id.example.org" }
            - { name: GROUP_PREFIX, value: "idp-" }
            - { name: DRY_RUN, value: "true" }
            - { name: ZITADEL_TOKEN_FILE, value: /secrets/zitadel-token }
            - { name: NETBIRD_TOKEN_FILE, value: /secrets/netbird-token }
          volumeMounts:
            - { name: secrets, mountPath: /secrets, readOnly: true }
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: { drop: ["ALL"] }
          resources:
            requests: { cpu: 10m, memory: 16Mi }
            limits: { memory: 64Mi }
      volumes:
        - name: secrets
          secret: { secretName: zitadel-netbird-sync }
```

Run a single replica. The tool needs no inbound traffic; an egress policy that
allows only DNS, your Zitadel and the NetBird API is a good fit.

## Good to know

- **People are matched by email address.** Zitadel and NetBird must know a
  person under the same address.
- **A person must exist in NetBird first.** NetBird creates a user at the first
  login. Until then there is nothing to put into a group; the next run after the
  login picks the person up.
- **Groups reach peers through NetBird's own setting.** Enable *user group
  propagation* in NetBird so that a user's groups apply to that user's peers.
- **A group does nothing by itself.** Use it in an access policy.
- **Roles are ignored.** Any active grant on a project means membership.
- **Logs carry no email addresses**, only NetBird user IDs and group names.

## Verify the image

```sh
gh attestation verify oci://ghcr.io/marcobockelbrink/zitadel-netbird-sync:<tag> \
  --owner marcobockelbrink
```

## Development

```sh
go test -race ./...
```

The tool has no dependencies outside the Go standard library.

## License

[Apache-2.0](LICENSE)
