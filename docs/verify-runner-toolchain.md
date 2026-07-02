# Verifying the runner toolchain (post-AMI-rollout smoke test)

runs-fleet AMIs bake Python/Ruby interpreters and pre-populate the GitHub Actions
tool cache (`/opt/hostedtoolcache`, exposed to the runner via `AGENT_TOOLSDIRECTORY`)
so the `setup-*` actions resolve common versions **offline** on Amazon Linux 2023
instead of downloading (Python/Ruby *can't* download on AL2023; Node/Go/Java can but
otherwise re-download every job).

After a new runner AMI rolls out, run this workflow in a repo whose jobs target
runs-fleet runners (`workflow_dispatch`) to confirm the toolchain resolves from the
cache and native builds work. Baked set: Python 3.11/3.12/3.13, Ruby 3.2/3.4,
Node 20/22, Go 1.24/1.25, Temurin JDK 17/21.

The key signal per `setup-*` step is **"Found in cache @ ..."** with **no**
"Attempting to download" line — that proves the pre-baked entry was used.

```yaml
name: verify-runner-toolchain
on: workflow_dispatch

jobs:
  toolchain:
    runs-on: runs-fleet
    steps:
      # Direct interpreters resolve on PATH (no "command not found").
      - run: python --version && pip --version && pipx --version
      - run: ruby --version && gem --version && bundle --version
      - run: go version
      - run: node --version && java --version

      # setup-* must resolve the baked versions with NO download.
      - uses: actions/setup-python@v6
        with: { python-version: "3.12" }
      - uses: ruby/setup-ruby@v1
        with: { ruby-version: "3.2" }
      - uses: actions/setup-node@v4
        with: { node-version: "22" }
      - uses: actions/setup-go@v5
        with: { go-version: "1.25" }
      - uses: actions/setup-java@v4
        with: { distribution: temurin, java-version: "21" }

      # pipx-installed apps must be on PATH (poetry, etc.).
      - run: pipx install poetry==2.2.1
      - run: command -v poetry && poetry --version   # resolves via /opt/pipx/bin

      # Native builds exercise the -devel headers.
      - run: pip install --no-binary :all: --no-cache-dir markupsafe   # compiles a C ext
      - run: gem install --no-document json                            # native gem
```

If a `setup-*` step logs "Attempting to download" (a cache miss), that version isn't
baked; either it's outside the baked set (expected — it still works via download) or
the cache layout for that tool needs adjusting. For Java specifically, confirm the
`distribution:` is `temurin` (the baked distribution).

`pipx install <tool>` launchers land in `/opt/pipx/bin`, which the runner puts on the
job PATH (via `PIPX_BIN_DIR` + `PATH` in the runs-fleet-agent unit) — matching
GitHub-hosted, so `poetry`/`black`/etc. resolve without extra PATH wiring.

Native-gem builds (`ruby.h`) are supported against the **default Ruby (3.4)** only:
AL2023's `ruby<ver>-devel` packages share unversioned headers and can't co-install, so
headers are baked for 3.4. Ruby 3.2 is runtime-only (pure-Ruby gems still work).
Python headers are per-version (`python3.x-devel` co-install fine).
