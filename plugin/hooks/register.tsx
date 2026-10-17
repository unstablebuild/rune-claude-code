import type { EngineInterface, McpToolResult, On, PromptOrigin } from 'claude-code'

// PROTOCOL is the version of the extension's tools this mod calls; the
// extension's rune_version reports its own.
export const PROTOCOL = 2

// SERVER is the key of the extension's MCP server in this plugin's .mcp.json.
const SERVER = 'rune'

export const PANE = 'rune-diagnostics'

// RETRY_MS is how long a failed connection to Rune is remembered before
// the next hook asks again, so typing outside Rune costs nothing.
const RETRY_MS = 30_000

const MAX_REFERENCES = 100
const MAX_DIAGNOSTICS = 200
const MIN_MENTION_PREFIX = 3
// A submitted prompt waits on Rune to place at most MAX_PROMPT_NAMES of the
// names it mentions, for at most RESOLVE_MS, so a cold index cannot hold up
// Enter.
const MAX_PROMPT_NAMES = 5
export const RESOLVE_MS = 1_500

const IDENTIFIER = /^[A-Za-z_][A-Za-z0-9_]*$/
const QUALIFIED = /^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)+$/
// A qualified name in a prompt's prose (`term.Writer`, `pkg.Type.Method`):
// not one right after an @, which prompt.mention places, nor one inside a
// path or URL.
const PROSE_NAME = /(?<![\w@./\\:-])[A-Za-z_]\w*(?:\.[A-Za-z_]\w*)+(?![\w/\\-]|\.\w|:\/\/)/g
// A prompt token worth completing: a qualified prefix (`fmt.Pr`), or a bare
// one after an @ (`@ideser`), which completes to a mention.
const COMPLETABLE = /^(@?)([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z0-9_]*)*)$/

// Definition is where symbol_definition says a symbol is defined, 1-based.
type Definition = {
  path: string
  line: number
  column: number
}

type Location = { uri: string; start_line: number; start_char: number }

type Diagnostic = Location & { severity: string; message: string; source?: string }

type Version = { version: string; protocol: number }

type Diagnostics =
  | { status: 'loading' }
  | { status: 'ready'; items: Diagnostic[] }
  | { status: 'failed'; message: string }

// State lives for one load of the module: the connection to Rune and what
// the diagnostics pane draws.
type State = {
  cwd: string
  server: string | null
  failedAt: number | null
  diagnostics: Diagnostics
}

// serverOf answers the name $.mcp.call takes for Rune's server, or null
// when the session is not in a Rune workspace.
async function serverOf($: EngineInterface, state: State): Promise<string | null> {
  if (state.server !== null) {
    return state.server
  }
  const now = await $.clock.now()
  if (state.failedAt !== null && now - state.failedAt < RETRY_MS) {
    return null
  }
  const connected = await $.mcp.connect(SERVER)
  if (!connected.isConnected) {
    state.failedAt = now
    return null
  }
  state.server = connected.server
  state.failedAt = null
  return state.server
}

// call runs one of Rune's tools and parses its JSON answer; null when the
// session is not in a Rune workspace.
async function call<T>(
  $: EngineInterface,
  state: State,
  tool: string,
  args: Record<string, unknown> = {},
): Promise<T | null> {
  const server = await serverOf($, state)
  if (server === null) {
    return null
  }
  return parse<T>(await $.mcp.call(server, tool, args))
}

async function checkVersion($: EngineInterface, state: State): Promise<void> {
  const info = await call<Version>($, state, 'rune_version').catch(() => null)
  if (info === null || info.protocol === PROTOCOL) {
    return
  }
  $.ui.status(
    info.protocol > PROTOCOL
      ? `Rune's Claude Code extension ${info.version} is newer than this plugin: run claude plugin update rune@unstablebuild`
      : `Rune's Claude Code extension ${info.version} is older than this plugin: run pkg install claude-code in Rune`,
  )
}

async function refreshDiagnostics($: EngineInterface, state: State): Promise<void> {
  let next: Diagnostics
  try {
    const items = await call<Diagnostic[]>($, state, 'lsp_workspace_diagnostics')
    next =
      items === null
        ? { status: 'failed', message: 'Not connected to Rune. Run claude from a Rune terminal.' }
        : { status: 'ready', items }
  } catch (err) {
    next = { status: 'failed', message: messageOf(err) }
  }
  state.diagnostics = next
  $.ui.invalidate('ui.render')
}

async function isPaneOpen($: EngineInterface): Promise<boolean> {
  return (await $.ui.panes()).some(pane => pane.id === PANE)
}

// referencesOf lists the references of the symbols named name, or null when
// no symbol has that name. lsp_references resolves the name through the
// workspace's symbols and fails when none matches.
async function referencesOf($: EngineInterface, state: State, name: string): Promise<string | null> {
  const refs = await call<Location[]>($, state, 'lsp_references', { symbol: name, include_declaration: true })
  if (refs === null || refs.length === 0) {
    return null
  }
  const lines = refs
    .slice(0, MAX_REFERENCES)
    .map(ref => `${relativeTo(pathOf(ref.uri), state.cwd)}:${ref.start_line + 1}:${ref.start_char + 1}`)
  if (refs.length > MAX_REFERENCES) {
    lines.push(`… ${refs.length - MAX_REFERENCES} more (lsp_references lists them all)`)
  }
  return [
    `Rune's language server has ${refs.length} references to symbols named \`${name}\` across the workspace, ` +
      'declarations included. Matches above that are not among them are comments, strings or other text.',
    ...lines,
  ].join('\n')
}

// isTyped tells the person's own prompt from one a notification, a peer, a
// schedule or a plugin sent.
function isTyped(origin: PromptOrigin): boolean {
  return origin.kind === 'composer' || origin.kind === 'bridge' || origin.kind === 'sdk'
}

// definitionsNote says where Rune finds each of names defined, leaving out
// any it does not place within RESOLVE_MS; null when it places none.
async function definitionsNote($: EngineInterface, state: State, names: readonly string[]): Promise<string | null> {
  const stop = new AbortController()
  const deadline = $.clock.sleep(RESOLVE_MS, { signal: stop.signal }).then(
    () => null,
    () => null,
  )
  // One connection for every lookup.
  const connected = serverOf($, state)
  const lookup = async (name: string) =>
    (await connected) === null ? null : call<Definition[]>($, state, 'symbol_definition', { name })
  const found = await Promise.all(names.map(name => Promise.race([lookup(name).catch(() => null), deadline])))
  stop.abort()
  const lines = names.flatMap((name, i) => {
    const defs = found[i] ?? []
    return defs.length === 0
      ? []
      : [`\`${name}\` is defined at ${defs.map(d => `${d.path}:${d.line}:${d.column}`).join(', ')}.`]
  })
  return lines.length === 0 ? null : ["Rune's language servers place names in this prompt:", ...lines].join('\n')
}

// namesIn lists the first MAX_PROMPT_NAMES distinct qualified names in text.
export function namesIn(text: string): string[] {
  const names = new Set<string>()
  for (const [name] of text.matchAll(PROSE_NAME)) {
    names.add(name)
    if (names.size === MAX_PROMPT_NAMES) {
      break
    }
  }
  return [...names]
}

export function register(on: On) {
  const state: State = {
    cwd: '',
    server: null,
    failedAt: null,
    diagnostics: { status: 'loading' },
  }

  on('session.start', async ($, e, next) => {
    const result = await next(e)
    state.cwd = e.cwd
    await $.command.register({
      name: PANE,
      description: "Shows the workspace's diagnostics from Rune's language servers",
    })
    // The first prompt waits on this hook; Rune's server may still be
    // starting, so the check runs after it.
    $.clock.after(0, () => void checkVersion($, state))
    return result
  })

  on('command.run', { command: PANE }, async $ => {
    if (await isPaneOpen($)) {
      await $.ui.close({ id: PANE })
      return { text: 'Rune diagnostics hidden' }
    }
    state.diagnostics = { status: 'loading' }
    await $.ui.open({ id: PANE, title: 'Rune diagnostics' })
    void refreshDiagnostics($, state)
    return { text: 'Rune diagnostics shown' }
  }).catch(() => ({ text: "Could not show Rune's diagnostics" }))

  on('turn.complete', async ($, e, next) => {
    const result = await next(e)
    if (await isPaneOpen($)) {
      void refreshDiagnostics($, state)
    }
    return result
  })

  on('ui.render', { component: 'Pane' }, ($, e, next) => {
    if (e.requestId !== PANE) {
      return next(e)
    }
    const { Box, Text, Button } = $.ui.resolve(e)
    const width = Math.max(20, Math.floor(e.props.bodyColumns))
    const d = state.diagnostics
    const header = (
      <Box flexDirection="row" gap={1}>
        <Text bold>{d.status === 'ready' ? summaryOf(d.items) : 'Diagnostics'}</Text>
        <Button key="refresh" hotkey="r" dimColor onPress={() => void refreshDiagnostics($, state)}>
          refresh
        </Button>
      </Box>
    )
    if (d.status !== 'ready') {
      return (
        <Box flexDirection="column">
          {header}
          <Text dimColor wrap="wrap">
            {d.status === 'loading' ? 'Asking the language servers…' : d.message}
          </Text>
        </Box>
      )
    }
    const rows = []
    for (const [file, items] of groupByFile(d.items.slice(0, MAX_DIAGNOSTICS), state.cwd)) {
      rows.push(
        <Text key={file} bold wrap="truncate-start">
          {file}
        </Text>,
      )
      for (const item of items) {
        rows.push(
          <Text key={`${file}:${item.start_line}:${item.start_char}:${item.message}`} wrap="truncate-end">
            <Text color={colorOf(item.severity)}>{`  ${item.start_line + 1}:${item.start_char + 1} ${item.severity}`}</Text>
            <Text>{` ${firstLine(item.message)}`.slice(0, width)}</Text>
          </Text>,
        )
      }
    }
    if (d.items.length > MAX_DIAGNOSTICS) {
      rows.push(<Text dimColor>{`… ${d.items.length - MAX_DIAGNOSTICS} more`}</Text>)
    }
    return (
      <Box flexDirection="column">
        {header}
        {rows}
      </Box>
    )
  })

  on('prompt.autocomplete', async ($, e, next) => {
    const match = COMPLETABLE.exec(e.token)
    if (match === null) {
      return next(e)
    }
    const [, at = '', prefix = ''] = match
    if (!prefix.includes('.') && (at === '' || prefix.length < MIN_MENTION_PREFIX)) {
      return next(e)
    }
    const [names, below] = await Promise.all([
      call<string[]>($, state, 'symbol_complete', { prefix }).catch(() => null),
      next(e),
    ])
    const mine = (names ?? [])
      .filter(name => name !== prefix)
      .map(name => ({ text: at + name, description: 'Rune symbol' }))
    return { suggestions: [...below.suggestions, ...mine] }
  })

  // A prompt the person typed that names qualified symbols says where they
  // are defined, so the model starts from the definition instead of a search.
  on('prompt.submit', async ($, e, next) => {
    if (e.command !== undefined || e.isMeta || !isTyped(e.origin)) {
      return next(e)
    }
    const names = namesIn(e.text)
    const note = names.length === 0 ? null : await definitionsNote($, state, names)
    return next(note === null ? e : { ...e, context: [...(e.context ?? []), note] })
  }).catch(($, e, next) => next(e))

  // @pkg.Symbol attaches the file that defines the symbol, when no file of
  // that name is there.
  on('prompt.mention', async ($, e, next) => {
    if (!QUALIFIED.test(e.mention) || (await $.fs.exists(e.path))) {
      return next(e)
    }
    const defs = await call<Definition[]>($, state, 'symbol_definition', { name: e.mention }).catch(() => null)
    const def = defs?.[0]
    if (def === undefined) {
      return next(e)
    }
    const result = await next({ ...e, path: def.path })
    if (result.deny !== undefined || result.type === null) {
      return result
    }
    const note = `@${e.mention} is defined at ${def.path}:${def.line}:${def.column}.`
    return { ...result, context: [...(result.context ?? []), note] }
  }).catch(($, e, next) => next(e))

  // A Grep for a bare identifier keeps its own result and gains the
  // language server's references beside it, which tell uses of the symbol
  // apart from text that merely matches.
  on('tool.call', async ($, e, next) => {
    const pattern = (e as { pattern?: unknown }).pattern
    if (String(e.tool) !== 'Grep' || typeof pattern !== 'string' || !IDENTIFIER.test(pattern)) {
      return next(e)
    }
    const [result, note] = await Promise.all([next(e), referencesOf($, state, pattern).catch(() => null)])
    if (note === null || result.deny !== undefined || result.isError) {
      return result
    }
    return { ...result, context: [...(result.context ?? []), note] }
  }).catch(($, e, next) => next(e))
}

// parse reads the JSON a Rune tool answers in its first text block.
export function parse<T>(result: McpToolResult): T {
  const block = result.content.find(c => c.type === 'text')
  const text = block !== undefined && 'text' in block ? String(block.text) : ''
  if (result.isError) {
    throw new Error(text || 'the Rune tool failed')
  }
  return JSON.parse(text) as T
}

export function pathOf(uri: string): string {
  return uri.startsWith('file://') ? decodeURIComponent(new URL(uri).pathname) : uri
}

export function relativeTo(path: string, cwd: string): string {
  return cwd !== '' && path.startsWith(`${cwd}/`) ? path.slice(cwd.length + 1) : path
}

export function groupByFile(items: readonly Diagnostic[], cwd: string): Map<string, Diagnostic[]> {
  const files = new Map<string, Diagnostic[]>()
  for (const item of items) {
    const file = relativeTo(pathOf(item.uri), cwd)
    files.set(file, [...(files.get(file) ?? []), item])
  }
  for (const list of files.values()) {
    list.sort((a, b) => a.start_line - b.start_line || a.start_char - b.start_char)
  }
  return new Map([...files.entries()].sort(([a], [b]) => a.localeCompare(b)))
}

function summaryOf(items: readonly Diagnostic[]): string {
  if (items.length === 0) {
    return 'No diagnostics'
  }
  const errors = items.filter(item => item.severity === 'Error').length
  const warnings = items.filter(item => item.severity === 'Warning').length
  const notes = items.length - errors - warnings
  return [
    errors > 0 && plural(errors, 'error'),
    warnings > 0 && plural(warnings, 'warning'),
    notes > 0 && plural(notes, 'note'),
  ]
    .filter(Boolean)
    .join(', ')
}

function plural(n: number, word: string): string {
  return `${n} ${word}${n === 1 ? '' : 's'}`
}

function colorOf(severity: string): 'error' | 'warning' | 'subtle' {
  switch (severity) {
    case 'Error':
      return 'error'
    case 'Warning':
      return 'warning'
    default:
      return 'subtle'
  }
}

function firstLine(text: string): string {
  return text.split('\n', 1)[0] ?? ''
}

function messageOf(err: unknown): string {
  return err instanceof Error ? err.message : String(err)
}