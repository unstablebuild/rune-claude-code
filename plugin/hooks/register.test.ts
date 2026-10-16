import type { McpToolResult, On, PromptAutocompleteInput, PromptAutocompleteResult, RenderPropsOf } from 'claude-code'
import { describe, expect, mock, test } from 'claude-code/testing'

import { PANE, PROTOCOL } from './register'

const PLUGIN = 'rune'
const SERVER = 'plugin:rune:rune'
const SESSION = { surface: 'terminal', isInteractive: true, cwd: '/work' } as const

type Answer = unknown | ((args: Record<string, unknown>) => unknown)

// World is Rune's side of the session, in memory: the tools' answers, and
// what the plugin asked of the engine.
type World = {
  settle: () => Promise<void>
  calls: { tool: string; args: Record<string, unknown> }[]
  statuses: (string | undefined)[]
  commands: string[]
  opened: string[]
  open: Set<string>
}

function text(value: unknown): McpToolResult {
  return { content: [{ type: 'text', text: JSON.stringify(value) }], isError: false }
}

function failure(message: string): McpToolResult {
  return { content: [{ type: 'text', text: message }], isError: true }
}

// worldOf seats Rune beneath the plugin: connected when tools is given,
// outside Rune when it is null.
function worldOf(on: On, tools: Record<string, Answer> | null, files: readonly string[] = []): World {
  const clock = mock.clock(on, { now: 1_000_000 })
  const world: World = { settle: clock.settle, calls: [], statuses: [], commands: [], opened: [], open: new Set() }
  on('session.start', ($, e) => ({ cwd: e.cwd }))
  on('mcp.connect', () =>
    tools === null
      ? { value: { isConnected: false, reason: 'failed', message: 'rune-claude exited' } }
      : { value: { isConnected: true, server: SERVER } },
  )
  on('mcp.call', ($, e) => {
    expect(e.server).toBe(SERVER)
    world.calls.push({ tool: e.tool, args: e.args })
    const answer = tools?.[e.tool]
    if (answer === undefined) {
      return { value: failure(`unknown tool ${e.tool}`) }
    }
    const value = typeof answer === 'function' ? answer(e.args) : answer
    return { value: (value as McpToolResult).content !== undefined ? (value as McpToolResult) : text(value) }
  })
  on('fs.exists', ($, e) => ({ value: files.includes(e.path) }))
  on('ui.status', ($, e) => {
    world.statuses.push(e.text)
    return { value: undefined }
  })
  on('command.register', ($, e) => {
    world.commands.push(e.name)
    return { value: { command: e.name } }
  })
  on('ui.open', ($, e) => {
    world.opened.push(e.id)
    world.open.add(e.id)
    return { value: { isPlaced: true } } as never
  })
  on('ui.close', ($, e) => {
    world.open.delete(e.id)
    return { value: undefined }
  })
  on('ui.panes', () => ({
    value: [...world.open].map(id => ({ id, title: id, isPlaced: true, isFocused: false })) as never,
  }))
  return world
}

function command(name: string) {
  return {
    command: name,
    args: '',
    origin: { kind: 'composer' as const },
    presentation: { isFullscreen: true, columns: 180 },
  }
}

const PANE_PROPS: RenderPropsOf['Pane'] = {
  title: 'Rune diagnostics',
  isFocused: false,
  bodyColumns: 80,
  placement: 'dock',
  scroll: { offset: 0, bodyRows: 40 },
  view: {},
}

// autocomplete raises prompt.autocomplete, which the kit's typings leave
// off the engine's prompt noun.
function autocomplete($: { prompt: object }, input: PromptAutocompleteInput): Promise<PromptAutocompleteResult> {
  return ($.prompt as { autocomplete: (e: PromptAutocompleteInput) => Promise<PromptAutocompleteResult> }).autocomplete(input)
}

describe('version check', () => {
  test('a matching protocol says nothing, and the pane command is registered', async ($, on) => {
    const world = worldOf(on, { rune_version: { version: '0.1.0', protocol: PROTOCOL } })
    await $.session.start(SESSION)
    await world.settle()

    expect(world.commands).toEqual([PANE])
    expect(world.calls.map(c => c.tool)).toEqual(['rune_version'])
    expect(world.statuses).toEqual([])
  })

  test('a newer extension asks to update the plugin', async ($, on) => {
    const world = worldOf(on, { rune_version: { version: '0.9.0', protocol: PROTOCOL + 1 } })
    await $.session.start(SESSION)
    await world.settle()

    expect(world.statuses).toHaveLength(1)
    expect(world.statuses[0]).toContain('claude plugin update rune@unstablebuild')
  })

  test('an older extension asks to update Rune', async ($, on) => {
    const world = worldOf(on, { rune_version: { version: '0.0.1', protocol: PROTOCOL - 1 } })
    await $.session.start(SESSION)
    await world.settle()

    expect(world.statuses[0]).toContain('pkg install claude-code')
  })

  test('outside Rune the check is silent', async ($, on) => {
    const world = worldOf(on, null)
    await $.session.start(SESSION)
    await world.settle()

    expect(world.calls).toEqual([])
    expect(world.statuses).toEqual([])
  })
})

describe('completion', () => {
  const prompt = (token: string) => ({ text: `see ${token}`, cursor: 4 + token.length, token, start: 4 })

  test('a qualified prefix completes from Rune, after the rows beneath', async ($, on) => {
    const world = worldOf(on, { symbol_complete: ['fmt.Printf', 'fmt.Println'] })
    on('prompt.autocomplete', () => ({ suggestions: [{ text: 'fmt.go' }] }))

    const result = await autocomplete($, prompt('fmt.Pr'))

    expect(world.calls).toEqual([{ tool: 'symbol_complete', args: { prefix: 'fmt.Pr' } }])
    expect(result.suggestions.map(s => s.text)).toEqual(['fmt.go', 'fmt.Printf', 'fmt.Println'])
  })

  test('an @ prefix completes to mentions, and a completed name is not offered again', async ($, on) => {
    worldOf(on, { symbol_complete: ['ideserver', 'ideserver.Start'] })
    on('prompt.autocomplete', () => ({ suggestions: [] }))

    const result = await autocomplete($, prompt('@ideserver'))

    expect(result.suggestions.map(s => s.text)).toEqual(['@ideserver.Start'])
  })

  test('plain words and short @ prefixes are left to the engine', async ($, on) => {
    const world = worldOf(on, { symbol_complete: ['the.Thing'] })
    on('prompt.autocomplete', () => ({ suggestions: [] }))

    for (const token of ['the', '@ab', 'path/to.go', '#12']) {
      expect((await autocomplete($, prompt(token))).suggestions).toEqual([])
    }
    expect(world.calls).toEqual([])
  })

  test('outside Rune completion is the engine alone', async ($, on) => {
    worldOf(on, null)
    on('prompt.autocomplete', () => ({ suggestions: [{ text: 'fmt.go' }] }))

    const result = await autocomplete($, prompt('fmt.Pr'))

    expect(result.suggestions.map(s => s.text)).toEqual(['fmt.go'])
  })
})

describe('mentions', () => {
  test('@pkg.Symbol attaches the file that defines it and says where', async ($, on) => {
    const world = worldOf(on, {
      symbol_resolve: [{ path: '/work/internal/ideserver/server.go', line: 42, column: 6, display: 'func Start' }],
    })
    const read: string[] = []
    on('prompt.mention', ($, e) => {
      read.push(e.path)
      return { type: 'file' }
    })

    const result = await $.prompt.mention({ mention: 'ideserver.Start', path: '/work/ideserver.Start' })

    expect(world.calls).toEqual([{ tool: 'symbol_resolve', args: { name: 'ideserver.Start' } }])
    expect(read).toEqual(['/work/internal/ideserver/server.go'])
    expect(result.type).toBe('file')
    expect(result.context?.[0]).toContain('/work/internal/ideserver/server.go:42:6')
    expect(result.context?.[0]).toContain('func Start')
  })

  test('a file that is there is read as named', async ($, on) => {
    const world = worldOf(on, { symbol_resolve: [] }, ['/work/main.go'])
    const read: string[] = []
    on('prompt.mention', ($, e) => {
      read.push(e.path)
      return { type: 'file' }
    })

    const result = await $.prompt.mention({ mention: 'main.go', path: '/work/main.go' })

    expect(world.calls).toEqual([])
    expect(read).toEqual(['/work/main.go'])
    expect(result.context).toBeUndefined()
  })

  test('a name Rune does not know is read as named', async ($, on) => {
    worldOf(on, { symbol_resolve: [] })
    const read: string[] = []
    on('prompt.mention', ($, e) => {
      read.push(e.path)
      return { type: null }
    })

    await $.prompt.mention({ mention: 'nope.Missing', path: '/work/nope.Missing' })

    expect(read).toEqual(['/work/nope.Missing'])
  })
})

describe('Grep', () => {
  const grepResult = { result: { mode: 'files_with_matches', numFiles: 1, filenames: ['/work/a.go'] }, text: '/work/a.go' }

  test('a bare identifier keeps its matches and gains the references', async ($, on) => {
    const world = worldOf(on, {
      lsp_references: [
        { uri: 'file:///work/a.go', start_line: 9, start_char: 5, end_line: 9, end_char: 10 },
        { uri: 'file:///work/b/c.go', start_line: 0, start_char: 0, end_line: 0, end_char: 5 },
      ],
    })
    on('tool.call', () => grepResult as never)

    const result = await $.tool.call({ tool: 'Grep', pattern: 'Start' } as never)

    expect(world.calls).toEqual([{ tool: 'lsp_references', args: { symbol: 'Start', include_declaration: true } }])
    expect((result as { result?: unknown }).result).toEqual(grepResult.result)
    const note = (result as { context?: readonly string[] }).context?.[0] ?? ''
    expect(note).toContain('2 references')
    expect(note).toContain('a.go:10:6')
    expect(note).toContain('b/c.go:1:1')
  })

  test('a regular expression, or a name that is no symbol, is Grep alone', async ($, on) => {
    const world = worldOf(on, { lsp_references: failure('symbol "Nope" not found') })
    on('tool.call', () => grepResult as never)

    const regex = await $.tool.call({ tool: 'Grep', pattern: 'func .*Start' } as never)
    const unknown = await $.tool.call({ tool: 'Grep', pattern: 'Nope' } as never)

    expect(world.calls.map(c => c.args.symbol)).toEqual(['Nope'])
    expect((regex as { context?: unknown }).context).toBeUndefined()
    expect((unknown as { context?: unknown }).context).toBeUndefined()
  })
})

describe('diagnostics pane', () => {
  const diagnostics = [
    {
      uri: 'file:///work/b.go',
      severity: 'Warning',
      start_line: 3,
      start_char: 1,
      end_line: 3,
      end_char: 4,
      message: 'unused variable',
      source: 'compiler',
      code: '',
    },
    {
      uri: 'file:///work/a.go',
      severity: 'Error',
      start_line: 9,
      start_char: 0,
      end_line: 9,
      end_char: 3,
      message: 'undefined: foo',
      source: 'compiler',
      code: '',
    },
  ]

  test('the command opens the pane and it lists the diagnostics by file', async ($, on) => {
    const world = worldOf(on, { rune_version: { version: '0.1.0', protocol: PROTOCOL }, lsp_workspace_diagnostics: diagnostics })
    await $.session.start(SESSION)

    expect(await $.command.run(command(PANE))).toEqual({ text: 'Rune diagnostics shown' })
    await world.settle()
    expect(world.opened).toEqual([PANE])

    const ui = await $.ui.mount({ plugin: PLUGIN, surface: 'terminal', component: 'Pane', requestId: PANE, props: PANE_PROPS })

    expect(await ui.find({ type: 'Text', text: '1 error, 1 warning' })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: 'undefined: foo' })).toBeDefined()
    const files = (await ui.findAll({ type: 'Text', text: /^[ab]\.go$/ })).map(f => f.text)
    expect(files).toEqual(['a.go', 'b.go'])

    expect(await $.command.run(command(PANE))).toEqual({ text: 'Rune diagnostics hidden' })
    expect(world.open.size).toBe(0)
  })

  test('a language server without workspace diagnostics says so in the pane', async ($, on) => {
    const world = worldOf(on, {
      rune_version: { version: '0.1.0', protocol: PROTOCOL },
      lsp_workspace_diagnostics: failure('method not supported: workspace/diagnostic'),
    })
    await $.session.start(SESSION)
    await $.command.run(command(PANE))
    await world.settle()

    const ui = await $.ui.mount({ plugin: PLUGIN, surface: 'terminal', component: 'Pane', requestId: PANE, props: PANE_PROPS })

    expect(await ui.find({ type: 'Text', text: /workspace\/diagnostic/ })).toBeDefined()
  })

  test('refresh asks again', async ($, on) => {
    let items: unknown[] = []
    const world = worldOf(on, {
      rune_version: { version: '0.1.0', protocol: PROTOCOL },
      lsp_workspace_diagnostics: () => items,
    })
    await $.session.start(SESSION)
    await $.command.run(command(PANE))
    await world.settle()

    const ui = await $.ui.mount({ plugin: PLUGIN, surface: 'terminal', component: 'Pane', requestId: PANE, props: PANE_PROPS })
    expect(await ui.find({ type: 'Text', text: 'No diagnostics' })).toBeDefined()

    items = diagnostics
    await ui.press({ key: 'refresh' })
    await world.settle()

    expect(await ui.find({ type: 'Text', text: 'undefined: foo' })).toBeDefined()
  })
})