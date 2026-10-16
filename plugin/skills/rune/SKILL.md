---
name: rune
description: Use when working in a project open in the Rune IDE, to navigate and check code through Rune's language servers (definitions, references, implementations, symbols, diagnostics, renames) instead of searching text, and to show the user code in Rune.
---

# Working in Rune

This session runs inside the Rune IDE. The `rune` MCP server exposes the
language servers Rune runs for this workspace, so answers about code come
from the compiler's view of it rather than from text matches.

## Prefer semantic tools over Grep for code

- Where something is defined: `lsp_definition`, or `symbol_resolve` for a
  qualified name such as `pkg.Symbol`.
- Who uses it: `lsp_references`. Grep also matches comments, strings and
  unrelated names that share the text; references do not.
- Who implements an interface, or what a type implements:
  `lsp_implementation`.
- What a file contains: `lsp_document_symbols`. Finding a symbol by name:
  `lsp_workspace_symbols`.
- Renaming: `lsp_rename` returns every edit the language server would make,
  across files.
- Structural patterns (all calls with two arguments, every method on a
  type): `syntax_search`.

Keep using Grep for text that is not a symbol: log messages, configuration
values, comments, and files no language server covers.

## Check your work with diagnostics

After editing, call `lsp_diagnostics` on the files you changed and fix
what it reports before you finish. Rune also tells Claude Code about new
diagnostics after each edit.

## Showing the user code

- `rune_open` opens a file in Rune at a line.
- `rune_highlight` marks line ranges in a file with a message, for example
  the places a review comment refers to. Calling it again for the same
  file replaces the marks; an empty `ranges` list clears them.
- `rune_notify` shows the user a short notification in Rune.

Edits you propose open in Rune as a diff the user accepts, rejects or edits
before it is saved; nothing is written until they answer.