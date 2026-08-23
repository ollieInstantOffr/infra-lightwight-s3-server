# Versioning and releases

Pail uses semantic versioning: **MAJOR.MINOR.PATCH**. So `1.1.0` is major 1,
minor 1, patch 0, and `1.1.1` is the first patch on top of it.

Tags carry a `v` prefix — `v1.0.0` — which is what `git describe` and every Go
tool expect. The version *inside* the binary and in image tags has no prefix.

## Which number a change earns

| | When | Example |
| --- | --- | --- |
| **MAJOR** | A change that breaks an existing deployment | A config key changes meaning; a schema change that cannot be rolled back; an S3 behaviour clients depend on |
| **MINOR** | New capability, backwards compatible | Access key scoping; versioning over the S3 API; the metrics endpoint |
| **PATCH** | Fixes and security updates that add nothing | A bug fix; a rebuild picking up a patched base image |

The deciding question for MAJOR is not how large the change is. It is whether
someone running the previous version can deploy this one without doing anything
first. If they cannot, it is MAJOR however small the diff.

## Schema compatibility

**This is the rule with teeth, because getting it wrong costs data.**

Migrations are forward-only. There are no down files, and there will not be:
writing one that is correct under load, for a schema change that has already
been running in production, is harder than it looks and the result is exercised
exactly once, in an emergency, by someone under pressure.

So:

- A **MINOR** release may change the schema, but the previous version must keep
  working against the new schema. In practice that means additive changes:
  add a column with a default, add a table, add an index. Do not drop or rename
  anything a released version still reads.
- **Dropping or renaming** is a MAJOR change, and the release notes must say
  that rolling back needs a database restore.
- A build **refuses to start** against a schema newer than it understands. It
  says which version the database is at and that going back means restoring
  from a backup taken before the upgrade. This is deliberate: an old binary
  reading columns that have moved corrupts the one thing this server exists to
  keep, and it would look like a hundred unrelated bugs.

When a MINOR release keeps that promise, rolling back is redeploying the
previous image and nothing else. That is the whole point of the rule.

Two migrations in the same release are fine. A migration in a PATCH release
should be treated as a mistake unless the patch exists *because* of the schema.

## Cutting a release

Releases are cut from `main` for anything at the tip. Patching an older line —
a fix for `1.1.x` while `main` has moved to `1.2` — needs a branch:

```bash
git checkout -b release/1.1 v1.1.0
```

Branches are created when first needed rather than for every minor, so most
releases never grow one.

Before tagging:

```bash
make check        # vet, gofmt, build
make test         # unit and integration
make test-compat  # aws-cli and boto3 against a real container
```

`make test-compat` is the one that gates an S3 server release. It is the only
check that proves real clients still work, and it is the check that has caught
the most.

Then:

```bash
git tag -a v1.2.0 -m "1.2.0"
git push origin v1.2.0
make docker              # builds and tags from git describe
```

`make docker` tags the image with the version and with `latest`. Version tags
are immutable once pushed; `latest` moves.

Nothing else derives the version. The Makefile and Dockerfile both read
`git describe`, so an untagged build reports a commit hash — which is correct,
and is why the console shows `dev` rather than a number on a working tree.

## Patching quickly

A patch release exists mostly for security, and its value is entirely in how
fast it can be produced. The path:

1. Fix on `main` if the affected version is the tip, or on the release branch
   if it is not.
2. `make test && make test-compat`.
3. Tag, push, build.

Much of what a patch fixes is not Pail's code. The Go toolchain, the distroless
base image and the module dependencies all produce advisories of their own, and
the fix is a rebuild:

```bash
go get -u ./... && go mod tidy   # dependencies
docker build --pull .            # base image
```

A rebuild of otherwise unchanged code is **not** a no-op if the base image
moved, and it needs a version of its own so deployments can tell the two apart.
That is what PATCH is for.

Nobody watches base image advisories by hand, and the first sign otherwise is a
scanner report from somewhere else. If you run image scanning anywhere, point it
at the published image.

## Changelog

Every release gets an entry in [CHANGELOG.md](../CHANGELOG.md), grouped by what
it means to somebody running Pail rather than by which files moved. The category
that earns the file is **Upgrade notes** — anything needing action, or any
release where rolling back is not simply redeploying the previous image.
