# Release notes

One file per release, `vX.Y.Z.md`, added by the version commit. The release workflow (`.github/workflows/release.yml`, spec `docs/spec-release-workflow.md`) refuses a release without it and publishes it as the GitHub release text.

Start with one sentence on what the release is, then `## What is new` and `## Download and verify`, as earlier releases do.

The bridge release only (spec §5) starts with the line `signing: offline`: the workflow then publishes nothing signed and leaves a draft that Joe signs locally. The line is dropped from the published text and refused once a release was signed in CI.
