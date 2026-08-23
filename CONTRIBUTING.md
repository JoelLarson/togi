# Contributing

Start with [AGENTS.md](./AGENTS.md) for repo conventions and non-negotiables, and
[CONTEXT.md](./CONTEXT.md) for the glossary — vocabulary here is load-bearing.
[docs/language.md](./docs/language.md) is a guided tour of both.

## Running the specs

```sh
go test ./...
```

The executable acceptance specifications live in
[`features/`](./features/README.md), one directory per theme, each `.feature`
colocated with the Go step definitions that bind it.

## Editor setup

### Zed — Gherkin step navigation

[`.zed/settings.json`](./.zed/settings.json) points the Cucumber language server
at our step definitions:

```json
"glue": ["features/**/*.go"]
```

That overrides the server's default `features/**/*_test.go`, which would miss
`features/internal/harness/steps.go` — shared steps that cannot live in a
`_test.go` file, because other packages import them.

**A one-time workaround is also required.** `@cucumber/language-server` resolves
its tree-sitter grammars from a path that does not exist under a normal npm
install:

```js
// bin/cucumber-language-server.cjs
const wasmBasePath = path.resolve(`${__dirname}/../node_modules/@cucumber/language-service/dist`)
```

npm hoists `language-service` to the top level, so every grammar fails to load and
`Parser.parse` then throws for each glue file. The symptom is that every step
reads as undefined even though the logs show features and glue being found:

```
* Found 38 glue file(s) in ["features/**/*.go"]
* Found 0 step definitions in those glue files
* Step Definition errors: Error: Parsing failed ... language: go
```

Create the path it expects. Run from anywhere — every path below is absolute:

```sh
W=~/.local/share/zed/extensions/work/cucumber/node_modules/@cucumber

mkdir -p "$W/language-server/node_modules/@cucumber"
ln -s "$W/language-service" "$W/language-server/node_modules/@cucumber/language-service"
```

Redo it if the extension reinstalls its `node_modules`, which happens when
Cucumber publishes a new `language-server` release.

Verify with `debug: open language server logs` → **cucumber**:

```
* Found 38 glue file(s) in ["features/**/*.go"]
* Found 128 step definitions in those glue files
```

This is upstream, not ours. The same `ENOENT` was reported for Neovim/Mason and
global npm installs in
[cucumber/language-server#72](https://github.com/cucumber/language-server/issues/72)
in 2022, and `bin/cucumber-language-server.cjs` on `main` is still unchanged. The
fix is to resolve the package instead of assuming a layout:

```js
const wasmBasePath = path.resolve(
  path.dirname(require.resolve('@cucumber/language-service')),
  '../../../dist'
)
```
