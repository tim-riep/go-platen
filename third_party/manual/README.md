# Manually reviewed licenses

go-licenses cannot classify the license files of the modules in this
directory, so they are excluded from `go-licenses check` with `--ignore`.
Each module was reviewed by hand; its LICENSE file is copied here so that it
is still included in THIRD_PARTY_LICENSES.

| Module | License | Reviewed |
| --- | --- | --- |
| github.com/jackpal/go-nat-pmp | Apache-2.0 | short Apache-2.0 header in LICENSE |
| github.com/jmespath/go-jmespath | Apache-2.0 | short Apache-2.0 header in LICENSE |

When updating one of these modules, compare the new LICENSE file and update
the copy and the `version` in METADATA.
