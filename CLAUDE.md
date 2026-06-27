# Loft conventions for Claude

These are the working rules for this repo. The git hooks in `.pre-commit-config.yaml`
enforce the mechanical parts; the rest is on you to follow.

## Writing style (commits, comments, docs, not chat)

Write plain, direct prose that does not read as machine generated. This applies to
everything that lands in the repo (commit messages, code comments, READMEs and docs).
It does not apply to chat replies.

- No em dashes or en dashes. Use a comma, a colon, parentheses, or two sentences.
- No filler or marketing words: comprehensive, robust, seamless, powerful,
  cutting-edge, game-changer, elevate, unleash, leverage (use "use"), delve,
  "harness the power of".
- No throat-clearing: "it's worth noting", "it is important to note", "in
  conclusion", "at the end of the day".
- Skip rule-of-three padding ("fast, simple, and reliable") unless each word earns
  its place.
- Prefer the concrete over the grand. Say what a thing does, not how impressive it is.

## Commit messages

Conventional Commits grammar with Linux-kernel message discipline. The grammar gives
tooling a stable prefix; the kernel rules keep the message useful to someone reading
`git log` two years from now.

Format:

```
<type>: <description>

<optional body, wrapped at 72 columns>

<optional footer>
```

Types:

| Type       | Use for                                                       |
|------------|---------------------------------------------------------------|
| `feat`     | A new capability.                                             |
| `fix`      | A bug fix.                                                    |
| `perf`     | A change that improves performance.                          |
| `refactor` | A change that neither fixes a bug nor adds a feature.        |
| `docs`     | Documentation only.                                          |
| `test`     | Adding or correcting tests.                                  |
| `style`    | Formatting only, no change in behavior.                      |
| `build`    | The build system or build scripts (Dockerfile, Makefile).   |
| `ci`       | CI configuration and pipelines.                             |
| `deps`     | Dependency version bumps (go.mod/go.sum, npm, base images). |
| `chore`    | Maintenance that touches no source logic or tests.          |
| `revert`   | Reverts an earlier commit.                                  |

`deps` is separate from `build` on purpose: `build` is how we build, `deps` is what
we build against.

No scopes for now. Write `<type>: <description>` with no `(scope)`. There is no
approved scope list yet, so any scope would be ad hoc. If we adopt an enumerated
allowlist later, it will be enforced and documented here.

Subject line:

- Imperative mood: `add`, `fix`, `remove`, not `added` / `fixes` / `removing`. Read
  it as completing "applying this commit will ...".
- 72 characters or fewer, including the type.
- No trailing period. Lowercase the description.

Body:

- Blank line after the subject, wrapped at 72 columns.
- Explain the motivation: what problem this solves and why this approach. The diff
  already shows what changed and how.
- Self-contained. Summarize a referenced bug or discussion rather than relying on a
  link that may rot.

Never put process or meta-information in a commit. A commit describes a change to the
code, not the work that produced it. Leave out:

- Audit and review framing: audit, OWASP, CWE/CVE, vulnerability, remediation,
  finding.
- Project narration: roadmap, sprint, milestone, phase, progress, plan status.
- Review counts ("23 reviewed, 2 confirmed") and tooling or workflow chatter.

State the change and why it matters. For a fix, name the failure mode and the
behavior after the fix.

Good:

```
fix: keep token reservation when upstream omits usage

A 2xx response with absent or zero usage refunded the full reservation
while still returning billable output, so the daily budget did not count
those replies. Reconcile only when usage is reported, matching the
streaming path.
```

Avoid:

```
fix: security audit remediation (P0/P1)    # process and meta
feat: Added new realtime hub.              # past tense, capital, period
fix(ai): handle empty usage                # no scopes for now
chore: bump deps                           # this is deps:, and says nothing
```

## Code comments

Comment the why, not the what. The code already states what it does; a comment earns
its place only when it adds what the code cannot: intent, a constraint, a consequence,
or context. Fewer, higher-value comments age better than many obvious ones.

Write these:

- The rationale for a non-obvious choice (why this constant, ordering, or algorithm).
- A warning about a consequence or gotcha a future editor would not foresee.
- An invariant or precondition the code relies on.
- Why an alternative was not taken, so nobody "fixes" it back.
- A reference to external context (a spec, a bug) that explains the strangeness.
- A one-line summary above a dense block so a reader can skip the detail.
- Doc comments on exported APIs: the contract, not the implementation.

Delete these:

- Restating the code: `i++ // increment i`.
- End-of-line noise: `return total // return the total`.
- A comment made redundant by a good name.
- Commented-out code (git remembers it).
- Changelog or meta narration in the source ("added by ... to fix ...").

If a comment is needed to explain what a line does, the code is usually unclear.
Rename the variable, extract a named helper, or introduce a named constant before
reaching for a comment. Keep comments next to what they describe and update them in
the same change, or they drift and mislead.

### File and module headers

A header comment describes the abstraction the file provides and why it exists, not a
list of what it contains. Treat it as the answer to "is this the file I want, and what
mental model do I read it with?".

Write:

- The responsibility: one or two sentences on the role the file plays and what it owns.
- The non-obvious invariant or contract the whole file rests on. This is the highest
  value a header carries, because it holds across every function and has nowhere else
  to live.

Drop:

- A table of contents of the types and functions inside. It duplicates the code and
  rots the moment someone adds one.
- Author, date, and change history. Git owns that, and it goes stale like any other
  changelog-in-a-comment.
- Boilerplate that restates the filename ("the database code").

The idiom differs by language; the Go specifics are in the "Go conventions" section
below. The TypeScript client SDK lives in its own repo (`loft-js`) with its own rules.

## Go conventions

This repo is a Go module at the root (`cmd/`, `internal/`), with the web root site under
`web/` and the acceptance suite under `test/`.

### File headers

A Go file header is the package doc comment: the comment directly above the `package`
clause, starting with `// Package <name> ...`. Follow the shared header guidance above
(describe the abstraction and the load-bearing invariant, not a list of contents).

- One package doc comment per package, not a header on every file. Put it on the file
  that best represents the package, or in a `doc.go` if it would be long.
- Other files in the package start straight at their code, with doc comments on the
  exported identifiers they declare.
- Exported identifiers get a doc comment that starts with the identifier name and reads
  as a sentence (`// Site is the calling tenant.`).

`internal/db/db.go` and `internal/ai/ai.go` are the reference examples: each leads with
the role of the package and the invariant that holds across it.

### Formatting and linting

`gofmt` is enforced by the pre-commit `go-fmt` hook. Run the full linter before pushing:
`golangci-lint run ./...` (config in `.golangci.yml`). Keep it at zero issues.

## Hooks

`.pre-commit-config.yaml` runs gitlint (commit grammar, subject length, body wrap),
`scripts/check-commit-style` (the prose policy above), go-fmt, and a couple of hygiene
checks. Enable once per clone with `pre-commit install`.
