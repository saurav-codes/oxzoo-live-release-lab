# release-lab

Deployed with [ox](https://deploywithox.com): deploy a repo to your own server with one command, no Docker. [Docs](https://deploywithox.com/docs) · [Stack guides](https://deploywithox.com/docs/guides)

**Live demo:** https://release-lab.s4.zoo.sorv.dev

> **Role in the zoo:** project `release-lab` of [oxzoo-live](https://github.com/saurav-codes/oxzoo-live-control/blob/main/zoo/README.md#projects), deployed with [ox](https://deploywithox.com) on server s4 at https://release-lab.s4.zoo.sorv.dev. The contract it follows is [DESIGN.md](https://github.com/saurav-codes/oxzoo-live-control/blob/main/zoo/DESIGN.md).

A Go app with no dependencies and no `ox.toml` that shows which release is answering. It is the zoo's lab for deploys, staging, branch previews, promote, and rollback.

## What it proves

- **Zero config.** `go.mod` alone gives ox the Go version, the build (`go build -o .ox/bin/app .`), and the start command.
- **Zero-downtime deploys.** On SIGTERM the server stops accepting, finishes every in-flight request (up to 25 s), then exits. Hold `/slow?ms=8000` open during a deploy: it still answers, from the old release.
- **Release identity.** The page and `/_zoo/health` show `OX_RELEASE`, `RELEASE_LABEL`, `OX_ENV`, the pid, and the start time. The page polls health every 500 ms and counts failures and release changes, so a deploy with downtime shows up as a failed count above 0.
- **Build info.** `/_zoo/health` has `build` with the Go version, module, the binary's build time, and `vcs` from `debug.ReadBuildInfo` when the build had a VCS stamp. ox builds from `git archive`, which has no `.git`, so on ox the VCS stamp is absent and `OX_RELEASE` (the commit) is the release id.

## Routes

| Route | What |
|---|---|
| `GET /` | the release page |
| `GET /slow?ms=N` | sleeps N ms (integer, clamped to 0..10000; anything else is 400) and answers with the release and pid |
| `GET /_zoo/health` | name, stack, server, release, env, label, pid, uptime, started_at, build |
| `GET /_zoo/probe` | checks: `loopback-slow` (`/slow?ms=50` over 127.0.0.1:$PORT, at least 50 ms, same release), `release` (OX_RELEASE set), `env` (OX_ENV is production, staging, or preview) |

The probe runs one at a time (a second caller waits up to 5 s, then gets 429), each check has 5 s, the whole probe 20 s. CORS on `/_zoo/*` answers only `ZOO_PANEL_ORIGIN`.

## Variables

| Variable | Who sets it | What |
|---|---|---|
| `PORT`, `HOST`, `OX_ENV`, `OX_RELEASE`, `PUBLIC_HOST` | ox | listen address, environment, release, server label |
| `RELEASE_LABEL` | you, per environment | a word shown on the page, for example `blue` on production and `green` on staging |
| `ZOO_PANEL_ORIGIN` | you | `https://zoo-control.s1.zoo.sorv.dev` |

## The drill

Production is `release-lab.s4.zoo.sorv.dev`. Keep the page open in a tab during every step and watch the failed counter stay at 0.

1. **Staging copy.** `ox staging release-lab --domain release-lab-staging.s4.zoo.sorv.dev --wait` makes the staging environment and deploys it. Give it its own label: `echo green | ox vars set release-lab --env staging RELEASE_LABEL --wait`. `ox environments release-lab` lists production and staging with their commits.
2. **Branch preview.** `ox previews release-lab on`, then push a branch (or `ox previews release-lab start try-label`). The preview gets its own `*.pv.zoo.sorv.dev` address and shows `OX_ENV=preview`. `ox previews release-lab remove try-label` deletes it.
3. **Zero-downtime deploy.** Open `/slow?ms=8000` on staging, then push a commit to staging's branch (or `ox deploy release-lab --env staging --wait`). The slow request answers with the old release; the page counts one release change and no failures.
4. **Promote.** `ox promote release-lab` prints the commits staging has that production does not and asks y/N; `ox promote release-lab --yes --wait` skips the question and exits with the deploy's result. Production gets staging's commit and keeps its own `RELEASE_LABEL`, because variables do not move.
5. **Rollback.** `ox rollback release-lab --wait` goes back to the release that was live before, without a rebuild; `--release ID` picks one. The page's release changes back, still with no failures.

## Tests

```sh
go vet ./... && go test ./...
```

`main_test.go` covers `ms` validation and clamping, the drain (a 400 ms request in flight when the server is told to stop still gets 200, and the listener is closed afterwards), the release and server label, and CORS for the panel origin versus an unlisted one.

Recorded on 2026-10-09: `ok oxzoo/release-lab 0.684s`.

A local run with `PORT=18471 OX_ENV=staging OX_RELEASE=abc123def4567890 RELEASE_LABEL=blue` answered the probe with all three checks ok (loopback 60 ms), `/slow?ms=x` with 400, and a `/slow?ms=1500` sent just before SIGTERM with `{"slept_ms":1500,...}`; the log showed `SIGTERM: draining in-flight requests` then `drained and stopped`.

## ox check

```
ox check . (manifest: none)

  app.start                  .ox/bin/app                                          detected:go.mod
  build.commands[0]          go build -o .ox/bin/app .                            detected:go.mod
  tools.go                   1.27                                                 detected:go.mod

  Provided by ox: PORT, HOST, OX_ENV, OX_PROJECT, OX_RELEASE, OX_DATA_DIR, PUBLIC_URL, PUBLIC_HOST
  Set on the dashboard before the first deploy: RELEASE_LABEL, ZOO_PANEL_ORIGIN

Ready to deploy.
```
