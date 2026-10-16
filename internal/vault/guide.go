package vault

import (
	"errors"
	"io/fs"
)

const InstructionRunes = 16000

type Guide struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Guide is intentionally read on every call; connection factories also call it
// so newly initialized clients observe external edits without a restart.
func (v *Vault) Guide() (Guide, error) {
	for _, name := range []string{"CLAUDE.md", "AGENTS.md", "README.md"} {
		text, err := v.ReadNote(name)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrDenied) {
			continue
		}
		if err != nil {
			return Guide{}, err
		}
		return Guide{Path: name, Content: text}, nil
	}
	return Guide{Content: "No vault guide found. Add CLAUDE.md, AGENTS.md, or README.md at the vault root."}, nil
}

func (g Guide) Instructions() string {
	text := []rune(g.Content)
	prefix := "Vault guide"
	if g.Path != "" {
		prefix += " (" + g.Path + ")"
	}
	prefix += ":\n\n"
	if len(text) > InstructionRunes {
		return prefix + string(text[:InstructionRunes]) + "\n\n[Guide truncated. Use get_vault_guide or resource vault://guide for the full text.]"
	}
	return prefix + g.Content
}
