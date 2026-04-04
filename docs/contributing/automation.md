# Automation

We use https://taskfile.dev/ for automating various tasks:
```
task --list
task: Available tasks for this project:
* align-apply:              Runs struct alignment for efficiency.
* check-if-in-devbox:       Checks if the user is in a devbox shell - used in lefthook
* gen:                      Runs the code generation - mostly for moq
* lint:                     Runs the linters and formatters
* llm-copy:                 Copies the go files for LLMs. Uses pbcopy (mac only).
* test:                     Runs all go tests
* test-no-cache:            Runs all go tests without caching
```
