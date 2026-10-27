# Contributing

sklp is source-available under the [Business Source License 1.1](LICENSE).
It is not open source. Contributions are welcome, under the following rules.

## Pull requests only

The only way to contribute is a pull request against `main` of the official
repository. Publishing a modified version of sklp anywhere else is not a
contribution and is not covered by the license.

## Contributor License Agreement

Every contributor must accept the [Contributor License Agreement](CLA.md)
before a pull request can be merged. Acceptance is explicit: the first line
of your pull request description must be

```
I have read and accept the Contributor License Agreement (CLA.md).
```

and every commit must carry a `Signed-off-by:` trailer with your real name
and email (`git commit -s`). A pull request without both is closed without
review.

## What we merge

- Bug fixes, with a test that fails before the fix.
- Small, focused changes. Open an issue first for anything larger than a few
  files so we can agree on the design before you write it.
- Code that follows the existing style: English only, minimal comments,
  `gofmt`.

Maintainers may decline a contribution for any reason.
