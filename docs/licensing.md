# Licensing, third-party notices, and branding

Worms NG is an independent implementation and is not affiliated with, endorsed
by, or sponsored by Electronic Arts, David S. Maynard, or the owners of any
later *Worms* trademarks. The names *Worms?*, *Worms*, SAIL, original artwork,
sound, logos, and other original-brand assets are not granted by a source-code
license. Do not ship those assets or imply endorsement without permission.
Use the project name “Worms NG” and original artwork for this repository's
software and distributions.

The research notes identify the original Atari source repository and state that
its source was released under the MIT License. If code or a substantial portion
of that source is copied, preserve its copyright and MIT license notice and
record the exact upstream revision in the distribution notices. A gameplay idea,
mechanic, or historical description is not by itself a license to copy artwork,
sound, text, or branding.

This repository links and builds with third-party Go dependencies, including:

- Gio (`gioui.org`) and its transitive modules;
- modernc SQLite (`modernc.org/sqlite`) and its transitive modules;
- the other modules recorded in `go.mod` and `go.sum`.

Before publishing a release, run `go list -m all`, collect each module's license
and notices from its source distribution, and include them in the release
artifact (or an attached `THIRD_PARTY_NOTICES` file). Keep copyright and license
texts intact. The build tool `gogio` is also a third-party build dependency and
must be covered by the notices for a release that distributes generated WASM.

## Repeatable audit

The release command emits a machine-readable module inventory and a human
review report:

```sh
LICENSE_ARTIFACT_DIR=dist/license-audit make license-audit
cat dist/license-audit/report.txt
```

Use strict mode for publication; it fails if any downloaded module has no
top-level `LICENSE`, `COPYING`, `LICENCE`, or `NOTICE` file:

```sh
scripts/license-audit.sh --strict
```

For a module reported as missing, obtain the license from the exact module
revision in the Go module cache or source distribution, review it, and add the
text to the release's `THIRD_PARTY_NOTICES` payload. Do not mark a missing
notice as reviewed merely because the module is transitive. Archive
`report.json`, `report.txt`, and the final notices file with the checksums.

This note is a release-engineering policy, not a replacement for legal advice.
When importing external code or original-brand material, obtain a provenance
record and legal review before distribution.
