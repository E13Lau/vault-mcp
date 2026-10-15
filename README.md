# vault-mcp

A lightweight MCP server that exposes a Markdown / Obsidian vault for read and write access by AI agents.

- No RAG. No vector index. Plain files and keyword search.
- One instance serves many agents over Streamable HTTP (stdio also supported).
- Git-aware: writes are serialized and synced to the vault's git remote.
- Obsidian-aware: `[[wikilink]]` resolution, YAML frontmatter, tags.

Status: planning. See the Multica project for scope and tasks.

## References

- [MCPVault](https://github.com/bitbonsai/mcpvault) — filesystem-based Obsidian MCP server (stdio)
- Vault-as-MCP style tools — expose an Obsidian vault as an MCP server

## License

MIT
