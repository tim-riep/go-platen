# Licensing

platen is licensed under the Apache License 2.0. Every module compiled into
the binary must be compatible with that license. This document describes how
this is enforced.

## Policy

- Dependencies must be under a permissive license: Apache-2.0, MIT,
  BSD-2-Clause, BSD-3-Clause or ISC.
- Exceptions, all of them transitive dependencies of NetBird's client
  (`github.com/netbirdio/netbird/client/embed`, BSD-3-Clause):
  - **MPL-2.0**, only for the modules listed in
    [`.github/licenses/mpl-approved.txt`](../.github/licenses/mpl-approved.txt)
    (`github.com/hashicorp/errwrap`, `github.com/hashicorp/go-multierror`,
    `github.com/hashicorp/go-version`).
  - **CC0-1.0** (`github.com/zeebo/blake3`), a public domain dedication.
- Only the BSD-3-Clause parts of NetBird are used. The AGPL directories of
  the NetBird repository (`management/`, `signal/`, `relay/`, `combined/`)
  are not imported, directly or transitively.

### MPL-2.0 files must stay unmodified

The MPL-2.0 is a file-level copyleft license. platen uses the approved MPL-2.0
modules as unmodified upstream modules. **Do not modify these files, do not
vendor them with changes, and do not copy code from them into platen.**
A modified MPL-2.0 file would have to be published under the MPL-2.0. If a
change is ever needed, it has to go upstream.

When distributing binaries, THIRD_PARTY_LICENSES carries the license text of
each MPL-2.0 module and the location of its source code (module path and
version).

## CI checks

The `licenses` job in [`.github/workflows/ci.yml`](../.github/workflows/ci.yml):

1. `go-licenses check` with the allowlist above (MPL-2.0 and CC0-1.0 included).
2. [`scripts/check-mpl.sh`](../scripts/check-mpl.sh) extracts every MPL-2.0
   module from `go-licenses report` and fails on modules that are not in
   `.github/licenses/mpl-approved.txt`. Adding an MPL-2.0 module therefore
   needs an explicit review and an entry in that file.
3. THIRD_PARTY_LICENSES must be up to date
   ([`scripts/third-party-licenses.sh`](../scripts/third-party-licenses.sh)).

### Manually reviewed modules

go-licenses cannot classify the license files of
`github.com/jackpal/go-nat-pmp` and `github.com/jmespath/go-jmespath`. Both
were verified by hand as Apache-2.0. They are excluded from the check with
`--ignore`; because `--ignore` also removes them from the report, their
license files are kept in [`third_party/manual/`](../third_party/manual) and
added to THIRD_PARTY_LICENSES by the generator script.

## Updating NetBird

The NetBird version is pinned in `go.mod`. Its `replace` directives
(`github.com/cloudflare/circl`, `github.com/mailru/easyjson`) are mirrored in
platen's `go.mod`, because replace directives of dependencies do not apply.
After an update, check that the replace directives still match, re-run the
license checks and confirm that no AGPL directory is imported:

```bash
go list -deps ./... | grep -E 'netbirdio/netbird/(management|signal|relay|combined)(/|$)'
```

The command must print nothing.

## PDFium (WebAssembly)

The `pdf-to-raster` backend renders PDFs with
[go-pdfium](https://github.com/klippa-app/go-pdfium) (MIT) in WebAssembly
mode. go-pdfium embeds `pdfium.wasm`, run by
[wazero](https://github.com/tetratelabs/wazero) (Apache-2.0), so the binary
stays cgo-free. PDFium itself is BSD-3-Clause and Apache-2.0.

**Open question:** go-pdfium does not document how `pdfium.wasm` was built,
in particular which of PDFium's bundled third-party libraries (font, image
and color management libraries) are compiled into it. The license texts of
those libraries are therefore not yet listed individually in
THIRD_PARTY_LICENSES. This needs to be resolved by obtaining the build
configuration from the go-pdfium maintainers, or by building `pdfium.wasm`
ourselves from a documented configuration and loading it with
`webassembly.InitWithWASM`. The binary was deliberately not inspected.

## Container image

The runtime image is built `FROM scratch` (see [docker.md](docker.md)). Besides
the platen binary and its notices it contains only Mozilla's CA certificate
bundle as packaged by Debian (`ca-certificates`, MPL-2.0, unmodified, with
its copyright file), which platen needs for TLS to the NetBird management
server. No base image packages (such as `base-files`, `netbase` or `tzdata`)
are included.
