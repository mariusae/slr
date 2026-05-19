# `slr`

`slr` is a small inline terminal wrapper around:

```sh
sl sl -r 'draft() & ((::.) + (.::))'
```

It preserves Sapling's smartlog rendering, including OSC hyperlinks, and adds lightweight keyboard interaction without switching to an alternate screen.

## Features

- Inline smartlog view using Sapling's own graph output
- Working-copy status from `sl status` at the bottom of the view
- Kaleidoscope links beside diffs when run in fbsource
- `Up` / `Down` to move between draft commits and the working-copy status block
- `Enter` to `sl goto` the selected commit and exit
- `Space` to expand the selected commit description
- Expanded descriptions rendered as markdown via `github.com/mariusae/md`
- `Ctrl-G` to run `sl metaedit -r <hash>`
- `Ctrl-D` to run paged `mdiff -P -c <hash>` for a commit or `mdiff -P` for the working copy
- `Ctrl-R` to refresh the smartlog view
- `q` or `Esc` to exit while leaving the rendered content on screen

## Build

```sh
go build
```

## Usage

Run the binary from inside a Sapling repository:

```sh
./slr
```

If stdin/stdout is not a terminal, it falls back to plain smartlog and status output:

```sh
sl sl -r 'draft() & ((::.) + (.::))'
sl status
```

## Notes

- Smartlog capture runs under `script` with `--pager=off` so Sapling still emits terminal formatting and OSC links without blocking on a pager.
- Selection highlighting uses a tinted background derived from the terminal's default background when available.
- Expanded markdown is wrapped to at most 100 columns, minus the smartlog graph prefix.
