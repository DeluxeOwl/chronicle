# Devbox

This project uses Devbox (https://www.jetify.com/docs/devbox/quickstart/) to manage the dev environment - ensuring everyone who contributes has the same tooling and depndencies.

See the [official installation instructions](https://www.jetify.com/docs/devbox/quickstart/).

```
git clone https://github.com/DeluxeOwl/chronicle
cd chronicle

devbox shell
```

This will automatically install and configure all required development tools:
- Go 1.24
- Go language server (gopls)
- Linting tools (golangci-lint)
- Code formatting (golines)
- Task runner (go-task)
- Git hooks (lefthook)
- Commit linting (commitlint-rs) - using semantic commits
- Code extraction tool (yek)
