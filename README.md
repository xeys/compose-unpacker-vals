# compose-unpacker-vals

A drop-in replacement image for Portainer's `compose-unpacker`, adding SOPS/age
secret decryption via [docker-vals](https://github.com/estie-inc/docker-vals)
and, optionally, native Compose secrets (`secrets: <name>: file: ...` /
`environment: ...`) sourced from the same encrypted file.

## Usage

Set it as Portainer's unpacker image:

```
COMPOSE_UNPACKER_IMAGE=ghcr.io/xeys/compose-unpacker-vals:latest
```

Requires **Enable relative path volumes** on the stack, so the repo (and its
`secrets.enc.yaml`) actually lands on disk where this image can reach it.

## What's in the image

Built on top of `compose-unpacker` (`BASE_IMAGE`, a fork with `docker/compose`
bumped to v5 for native `rawsetenv` support):

- `docker-vals`, `vals`, `sops` — the official release binaries, used as-is by
  Compose's own `provider:` mechanism (`type: vals`) to resolve
  `ref+sops://...` refs into environment variables at deploy time.
- `entrypoint-wrapper` — a small static Go binary, this image's `ENTRYPOINT`.

## The wrapper

`provider:`-injected variables aren't visible to Compose's *native* secrets
mechanism (`secrets: <name>: environment: VAR` / `file: ...`): that resolution
happens when the compose file is loaded, before any `provider` plugin has run.
So if a stack wants `secrets:`/`/run/secrets/...` instead of a plain
environment variable, something has to decrypt `secrets.enc.yaml` *before*
`docker compose up` starts.

For the `deploy` subcommand, the wrapper:

1. Clones the stack's repo itself (shallow, same ref/credentials Portainer
   passed), just to reach `secrets.enc.yaml`.
2. If that file exists next to the compose file, decrypts it with the bundled
   `sops`, resolving `SOPS_AGE_KEY_FILE` from (in order) the repo's own
   `.env`, the wrapper's own environment, or
   `/mnt/stacks/portainer-compose-unpacker/.age-key`.
3. Depending on `SECRET_OUTPUT_MODE` (see below), writes each decrypted key to
   a file and/or adds it as a `--env KEY=VALUE` flag.
4. `exec()`s into the real `compose-unpacker` binary with the original args
   (plus any injected `--env` flags) — unmodified otherwise.

Any other subcommand (`undeploy`, `remove-dir`, ...) passes straight through.
A stack with no `secrets.enc.yaml` is a no-op.

### SECRET_OUTPUT_MODE

Set at image build time (`ARG`/`ENV` in the Dockerfile) — Portainer controls
the container's actual `deploy ...` args, so this can't be a runtime flag.

| Mode   | Files | `--env` |
|--------|-------|---------|
| `both` (default) | yes | yes |
| `file` | yes | no |
| `env`  | no  | yes |

`env` is the one that never touches the host disk.

### Conventions

Given a stack named `<project>` with `secrets.enc.yaml` next to its compose
file, key `<key>` (e.g. `db_password`):

- **File secret**: `secrets: <name>: file: <destination>/decrypted-secrets/<project>/<key>`
  — `<destination>` is the same path Portainer passes as the deploy
  destination (typically `/mnt/stacks/portainer-compose-unpacker`), a real
  host mount, required because `secrets: file:` is a genuine bind mount
  resolved by the daemon, not something the wrapper's own container-local
  filesystem can satisfy. Written outside `<destination>/stacks/<project>`, so
  it survives the real clone (which wipes that directory on every deploy).
- **Environment secret**: `secrets: <name>: environment: <KEY_UPPERCASE>` —
  e.g. `api_key` → `API_KEY`, matching the naming already used in `provider:`
  blocks.

Both mechanisms bake the value directly into the *consuming* container's own
filesystem at creation time (confirmed via `docker inspect`: `"Mounts": []`
for the `environment:` case) — a container that only reads `/run/secrets/...`
never has the value in its own `Env`, and it survives a plain restart without
re-running compose-unpacker. The residual exposure for `env`/`both` mode is
narrower than a normal service environment variable: the value only appears
in `compose-unpacker`'s own live process args (`docker top`, `ps aux` on the
host) while it's running, never in `docker inspect` on any container.

### Example

```yaml
services:
  secrets:
    provider:
      type: vals
      options:
        env:
          - "API_KEY=ref+sops:///mnt/stacks/portainer-compose-unpacker/stacks/mystack/secrets.enc.yaml#/api_key"

  app:
    image: alpine
    secrets:
      - api_key
    command: sh -c "cat /run/secrets/api_key"

secrets:
  api_key:
    file: /mnt/stacks/portainer-compose-unpacker/decrypted-secrets/mystack/api_key
```

## Building

```bash
docker build \
  --build-arg BASE_IMAGE=ghcr.io/xeys/compose-unpacker:compose-v5-migration \
  --build-arg SECRET_OUTPUT_MODE=both \
  -t compose-unpacker-vals:local .
```

`SECRET_OUTPUT_MODE`, `DOCKER_VALS_VERSION`, `VALS_VERSION`, `SOPS_VERSION` and
`BASE_IMAGE` are all build args, mirrored as `workflow_dispatch` inputs in
[`.github/workflows/build-image.yml`](.github/workflows/build-image.yml),
which publishes to `ghcr.io/<owner>/compose-unpacker-vals:<tag>`.

Each downloaded binary is checksum-verified (`DOCKER_VALS_SHA256`,
`VALS_SHA256`, `SOPS_SHA256` build args) — the build fails outright on a
mismatch. Bumping `*_VERSION` requires supplying the matching new checksum
too: `sops` and `vals` publish their own `checksums.txt` per release;
`docker-vals` doesn't, so that one has to be computed by hand
(`sha256sum` on the downloaded binary) and reviewed before pinning.
