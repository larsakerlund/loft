# Contributing to Loft

## Setup

Install the git hooks once per clone:

```sh
pre-commit install
```

This wires up the `pre-commit` and `commit-msg` hooks from
`.pre-commit-config.yaml`. They run on commit:

- **gitlint** checks commit-message structure (Conventional Commits grammar,
  72-char subject, blank line, body wrapped at 72, no trailing period).
- **scripts/check-commit-style** enforces the prose policy (no process/meta talk,
  no em dashes, no AI-tell wording, imperative subject).
- **go-fmt** checks Go formatting.
- **check-merge-conflict** and **check-added-large-files** are general hygiene.

Heavier checks (golangci-lint, the acceptance suite) run separately, not on every
commit.

## Conventions

The full rules for commit messages, code comments, and writing style live in
[CLAUDE.md](CLAUDE.md). The short version:

- Commits: `<type>: <description>`, imperative, 72-char subject, body explains why
  not how, no process or meta-information. No scopes for now.
- Comments: explain why, not what. Refactor or rename before adding a what-comment.
- Writing: plain prose, no em dashes, no filler or marketing words.

The Go conventions are in [CLAUDE.md](CLAUDE.md); the TypeScript client SDK keeps its own rules in
the separate `loft-js` repo.
