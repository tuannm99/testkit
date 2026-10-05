package ai

import "embed"

// PromptVersion is recorded with every request; bump it when a prompt changes.
const PromptVersion = "2026-10-05.1"

//go:embed prompts/*.md
var prompts embed.FS

func prompt(name string) string {
	b, err := prompts.ReadFile("prompts/" + name + ".md")
	if err != nil {
		panic(err)
	}
	return string(b)
}

// SystemPrompt is the rules every task starts with.
func SystemPrompt() string { return prompt("system") }
