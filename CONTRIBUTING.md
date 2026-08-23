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

Every step pattern must be registered in a `features/**/*_test.go` file. That is
the Cucumber language server's default Go glue, and we cannot override it — see
below. `features/internal/harness/glue_test.go` fails the build if a `Step` call
appears in any other file under `features/`, because such a step still runs under
godog but reads as undefined in the editor.

Shared step *behavior* may live in the harness; shared *registration* may not.

**A one-time workaround is required.** `@cucumber/language-server` resolves its
tree-sitter grammars from a path that does not exist under a normal npm install:

```js
// bin/cucumber-language-server.cjs
const wasmBasePath = path.resolve(`${__dirname}/../node_modules/@cucumber/language-service/dist`)
```

npm hoists `language-service` to the top level, so every grammar fails to load and
`Parser.parse` then throws for each glue file. The symptom is that every step
reads as undefined even though the logs show features and glue being found:

```
* Found 29 glue file(s) in [... ,"features/**/*_test.go"]
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
* Found 29 glue file(s) in [... ,"features/**/*_test.go"]
* Found 127 step definitions in those glue files
```

Both problems are upstream, not ours.

The grammar path was reported for Neovim/Mason and global npm installs in
[cucumber/language-server#72](https://github.com/cucumber/language-server/issues/72)
in 2022, and `bin/cucumber-language-server.cjs` on `main` is still unchanged. The
fix is to resolve the package instead of assuming a layout:

```js
const wasmBasePath = path.resolve(
  path.dirname(require.resolve('@cucumber/language-service')),
  '../../../dist'
)
```

The glue setting cannot be configured at all. Anything under
`lsp.cucumber.settings` in `.zed/settings.json` reaches the server wrapped as
`{"cucumber": {...}}` by
[thlcodes/zed-extension-cucumber](https://github.com/thlcodes/zed-extension-cucumber),
but the server reads `glue` off the top level of what it receives. It gets
`undefined`, `findUris` calls `globs.reduce` on it, and the reindex dies:

```
Client sent workspace/configuration
Failed to reindex: Cannot read properties of undefined (reading 'reduce')
```

The next reindex then silently falls back to the server's thirteen built-in
globs. Since `glue` can never land at the top level, no value in
`.zed/settings.json` fixes this, which is why the invariant above is enforced in
Go instead.
