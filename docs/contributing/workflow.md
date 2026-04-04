# Workflow

1. Always work in a devbox shell
```
devbox shell
```

2. After making changes:
```
task lint
task test
```

3. Before commiting
- git hooks (via `lefthook`) run some checks automatically
- commits must follow conventional commit format (enforced by `commitlint-rs`)
