package gateway

import (
	_ "embed"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
)

// The free tier accepts only agent-shaped requests, and its abuse checks expect
// the system prompt the official CLI sends. These assets are the CLI's own
// wording: the v1 text for /zen (OpenCode 1.x tool names) and the v2 text for
// /inference and /zen/go (OpenCode 2.x tool names). The v2 file is the v1 file
// with the tool names the 2.x CLI uses.
//
//go:embed opencode_prompt_v1.txt
var opencodePromptV1 string

//go:embed opencode_prompt_v2.txt
var opencodePromptV2 string

//go:embed opencode_skills_v2.txt
var opencodeSkillsV2 string

// opencodePromptMarker identifies a body that already carries the official CLI
// system prompt, so a client that sends it itself is left alone. The two
// generations open differently: 1.x with "You are opencode, an interactive CLI
// tool", 2.x with "You are an AI agent running in OpenCode".
const (
	opencodePromptMarker   = "You are opencode, an interactive CLI tool"
	opencodePromptMarkerV2 = "You are an AI agent running in OpenCode"
)

// opencodeSkillsV1 is the 1.x built-in skill listing the CLI appends after its
// environment block. It is static, so it is part of the injected prompt. The
// 2.x client lists different skills and puts the block before the environment,
// so its listing lives in opencode_skills_v2.txt.
const opencodeSkillsV1 = `Skills provide specialized instructions and workflows for specific tasks.
Use the skill tool to load a skill when a task matches its description.
<available_skills>
  <skill>
    <name>customize-opencode</name>
    <description>Use ONLY when the user is editing or creating opencode's own configuration: opencode.json, opencode.jsonc, files under .opencode/, or files under ~/.config/opencode/. Also use when creating or fixing opencode agents, subagents, skills, plugins, MCP servers, or permission rules. Do not use for the user's own application code, or for any project that is not configuring opencode itself.</description>
    <location>&lt;built-in&gt;</location>
  </skill>
</available_skills>`

// opencodeEnvironmentTemplate is the built-in <env> block. Placeholders keep it
// correct per request: the client's own project when its system prompt reports
// one, otherwise the gateway process directory. The platform is always the
// Windows target this build serves.
const opencodeEnvironmentTemplate = `You are powered by the model named {{model}}. The exact model ID is opencode/{{model}}
Here is some useful information about the environment you are running in:
<env>
  Working directory: {{cwd}}
  Workspace root folder: {{workspace}}
  Is directory a git repo: {{git}}
  Platform: {{platform}}
  Today's date: {{date}}
</env>`

// PromptDefaults is the built-in prompt text the Prompt inject tab shows for a
// part that has no override. The base prompts already carry Windows wording.
type PromptDefaults struct {
	BaseV1      string `json:"base_v1"`
	BaseV2      string `json:"base_v2"`
	Environment string `json:"environment"`
	Skills      string `json:"skills"`
}

// DefaultPromptParts returns the built-in prompt text for the configuration UI.
func DefaultPromptParts() PromptDefaults {
	return PromptDefaults{
		BaseV1:      opencodePromptV1Windows,
		BaseV2:      opencodePromptV2Windows,
		Environment: opencodeEnvironmentTemplate,
		Skills:      opencodeSkillsV1,
	}
}

// windowsPrompt rewrites the shell wording of the built-in CLI prompt. This
// build targets Windows, so the model must not be told it has bash.
func windowsPrompt(base string) string {
	return windowsPromptReplacer.Replace(base)
}

var (
	windowsPromptReplacer = strings.NewReplacer(
		"non-trivial bash command", "non-trivial PowerShell command",
		"tools like Bash", "tools like PowerShell",
		"with Bash if they were provided", "with PowerShell if they were provided",
		"multiple bash tool calls", "multiple PowerShell tool calls",
	)
	opencodePromptV1Windows = windowsPrompt(opencodePromptV1)
	opencodePromptV2Windows = windowsPrompt(opencodePromptV2)
)

var (
	opencodeEnvOnce sync.Once
	opencodeWorkDir string
	opencodeRootDir string
	opencodeInGit   bool
)

// opencodeEnvironmentV1 renders the <env> block the CLI puts after its base
// prompt. The directory is resolved once per process; a platform without one
// falls back to the official container path. The CLI reports the directory it
// runs in, the root of the git worktree that contains it (or the filesystem
// root when there is none) and whether that worktree exists, so the block never
// contradicts the project header the gateway sends alongside it.
func opencodeEnvironmentV1(model string) string {
	opencodeEnvOnce.Do(resolveHostPaths)
	root := opencodeRootDir
	if !opencodeInGit {
		root = workspaceRoot(opencodeWorkDir)
	}
	apiModel := strings.TrimSpace(strings.TrimPrefix(model, "opencode/"))
	if apiModel == "" {
		apiModel = "unknown"
	}
	return strings.Join([]string{
		"You are powered by the model named " + apiModel + ". The exact model ID is opencode/" + apiModel,
		"Here is some useful information about the environment you are running in:",
		"<env>",
		"  Working directory: " + opencodeWorkDir,
		"  Workspace root folder: " + root,
		"  Is directory a git repo: " + repoFlag(),
		"  Platform: " + hostPlatform(),
		"  Today's date: " + today(),
		"</env>",
	}, "\n")
}

// renderEnvironmentV2 renders the 2.x client's environment tail. It differs
// from 1.x in three ways: the date stands on its own before the block, the
// block names the temp directory the client pre-created instead of the date,
// and a note about moving the session to a worktree follows it. The 2.x client
// also names no model, because its skills listing already precedes this text.
func renderEnvironmentV2(workDir, root, repo, platform string) string {
	return "\nToday's date: " + today() +
		"\n\nHere is some useful information about the environment you are running in:\n<env>\n" +
		"  Working directory: " + workDir + "\n" +
		"  Workspace root folder: " + root + "\n" +
		"  Is directory a git repo: " + repo + "\n" +
		"  Platform: " + platform + "\n" +
		"  Prefer " + hostTempDir() + " over generic system temporary directories such as /tmp; it is pre-created and approved for external access.\n" +
		"</env>\n" +
		"When you create a worktree outside the current working directory and intend to use it as your primary working directory, consider using `execute` to call `tools.opencode.session_move` and make the worktree the session's working directory."
}

// opencodeEnvironmentV2 renders the 2.x tail for the host paths.
func opencodeEnvironmentV2() string {
	opencodeEnvOnce.Do(resolveHostPaths)
	return renderEnvironmentV2(opencodeWorkDir, opencodeRootDir, repoFlag(), hostPlatform())
}

// opencodeEnvironmentV2For renders the 2.x tail for a project detected in the
// client's own system prompt, so the block is honest about where the client
// really runs even though the gateway runs elsewhere.
func opencodeEnvironmentV2For(environment promptEnvironment) string {
	return renderEnvironmentV2(environment.WorkDir, environment.Workspace, environment.GitRepo, environmentPlatform(environment))
}

// resolveHostPaths resolves the working directory, its git worktree root and
// the repository flag once per process.
func resolveHostPaths() {
	directory := ""
	if current, err := os.Getwd(); err == nil {
		directory = filepath.Clean(strings.TrimSpace(current))
	}
	if directory == "" || directory == "." {
		directory = "/workspace"
	}
	// The CLI reports the real path, so a working directory reached through a
	// symlink must not keep the link (macOS resolves /tmp to /private/tmp).
	if resolved, err := filepath.EvalSymlinks(directory); err == nil && resolved != "" {
		directory = resolved
	}
	opencodeWorkDir = directory
	opencodeRootDir, opencodeInGit = worktreeRoot(directory)
}

// repoFlag is the value of the environment block's repository line.
func repoFlag() string {
	if opencodeInGit {
		return "yes"
	}
	return "no"
}

// today is the date both generations print. The CLI uses the host's local date,
// not UTC: a capture under TZ=Pacific/Kiritimati reported the next day while UTC
// still named the previous one.
func today() string {
	return time.Now().Format("Mon Jan 02 2006")
}

// worktreeRoot reports the root of the git worktree containing directory, or
// directory itself when it is not inside one, as the v2 client does. A worktree
// root is the nearest ancestor carrying a .git entry, a directory in an ordinary
// checkout and a file in a linked worktree.
func worktreeRoot(directory string) (string, bool) {
	for current := directory; ; {
		if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
			return current, true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return directory, false
		}
		current = parent
	}
}

// workspaceRoot is the root of the volume holding directory: "/" on Unix and
// "C:" plus the separator on Windows, the value the v1 CLI reports outside a
// git worktree.
func workspaceRoot(directory string) string {
	if volume := filepath.VolumeName(directory); volume != "" {
		return volume + string(filepath.Separator)
	}
	return string(filepath.Separator)
}

var (
	workDirOnce sync.Once
	workDir     string
)

// gatewayWorkDir resolves the process directory once, using a Windows fallback
// when the process has none.
func gatewayWorkDir() string {
	workDirOnce.Do(func() {
		directory := ""
		if current, err := os.Getwd(); err == nil {
			directory = filepath.Clean(strings.TrimSpace(current))
		}
		if directory == "" || directory == "." {
			directory = `C:\`
		}
		workDir = directory
	})
	return workDir
}

// promptEnvironment is the project information rendered into the <env> block.
type promptEnvironment struct {
	WorkDir   string
	Workspace string
	GitRepo   string
	Platform  string
}

var (
	promptWorkDirPattern   = regexp.MustCompile(`(?mi)^\s*(?:current working directory|working directory|cwd)\s*:\s*(.+?)\s*$`)
	promptWorkspacePattern = regexp.MustCompile(`(?mi)^\s*workspace root folder\s*:\s*(.+?)\s*$`)
	promptGitPattern       = regexp.MustCompile(`(?mi)^\s*is directory a git repo\s*:\s*(yes|no)\b`)
)

// detectPromptEnvironment recovers the project the client says it is working
// in. DSH and the OpenCode CLIs put it in their system prompt; using it keeps
// the injected environment honest even though the gateway runs elsewhere.
func detectPromptEnvironment(text string) promptEnvironment {
	environment := promptEnvironment{Platform: hostPlatform()}
	if match := promptWorkDirPattern.FindStringSubmatch(text); match != nil {
		environment.WorkDir = cleanPromptPath(match[1])
	}
	if match := promptWorkspacePattern.FindStringSubmatch(text); match != nil {
		environment.Workspace = cleanPromptPath(match[1])
	}
	if environment.WorkDir == "" {
		environment.WorkDir = gatewayWorkDir()
	}
	if environment.Workspace == "" {
		if root, inGit := worktreeRoot(environment.WorkDir); inGit {
			environment.Workspace = root
		} else {
			environment.Workspace = environment.WorkDir
		}
	}
	if match := promptGitPattern.FindStringSubmatch(text); match != nil {
		environment.GitRepo = strings.ToLower(match[1])
	}
	if environment.GitRepo == "" {
		environment.GitRepo = detectGitRepo(environment.WorkDir)
	}
	return environment
}

// environmentPlatform is the platform line of the <env> block: the detected
// value when the client reported one, the host platform otherwise.
func environmentPlatform(environment promptEnvironment) string {
	if environment.Platform != "" {
		return environment.Platform
	}
	return hostPlatform()
}

// applyPromptEnvironment overlays the configured downstream.prompt_environment
// values on a detected environment, so an operator can pin the directory,
// workspace root, platform or repository flag the shaped client reports.
func applyPromptEnvironment(environment promptEnvironment, override *config.PromptEnvironmentConfig) promptEnvironment {
	if override == nil {
		return environment
	}
	if override.WorkingDirectory != "" {
		environment.WorkDir = override.WorkingDirectory
		environment.Workspace = override.WorkingDirectory
	}
	if override.WorkspaceRoot != "" {
		environment.Workspace = override.WorkspaceRoot
	}
	if override.Platform != "" {
		environment.Platform = override.Platform
	}
	if override.IsGitRepo != nil {
		environment.GitRepo = "no"
		if *override.IsGitRepo {
			environment.GitRepo = "yes"
		}
	}
	return environment
}

// cleanPromptPath keeps a detected Windows path and rejects Unix or placeholder
// values, which would make the environment misleading.
func cleanPromptPath(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "\"'\x60")
	if value == "" || strings.HasPrefix(value, "/") || strings.ContainsAny(value, "<>") {
		return ""
	}
	cleaned := filepath.Clean(value)
	if cleaned == "." {
		return ""
	}
	return cleaned
}

// detectGitRepo walks up from directory looking for a repository marker. When
// no directory is known the previous "yes" answer is kept, because the clients
// this prompt imitates always run inside a repository.
func detectGitRepo(directory string) string {
	if directory == "" {
		return "yes"
	}
	for current := directory; ; {
		if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
			return "yes"
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "no"
		}
		current = parent
	}
}

// renderPromptTemplate fills the environment placeholders. Unknown placeholders
// are left untouched so a typo stays visible in the preview and the request.
func renderPromptTemplate(template, model string, environment promptEnvironment) string {
	replacer := strings.NewReplacer(
		"{{model}}", model,
		"{{cwd}}", environment.WorkDir,
		"{{workspace}}", environment.Workspace,
		"{{git}}", environment.GitRepo,
		"{{platform}}", environmentPlatform(environment),
		"{{date}}", today(),
	)
	return replacer.Replace(template)
}

// promptModel strips the lane prefix the CLI adds to the model name.
func promptModel(model string) string {
	apiModel := strings.TrimSpace(strings.TrimPrefix(model, "opencode/"))
	if apiModel == "" {
		return "unknown"
	}
	return apiModel
}

// opencodeSystemPrompt assembles the configurable CLI system prompt for model
// and the upstream generation's tool naming. The generations order their parts
// differently: 1.x puts the model line and environment block before the skills
// listing, while 2.x lists the skills first and then the date, the environment
// and the worktree note. A part with its Custom flag keeps the operator's text
// instead of the built-in wording.
func opencodeSystemPrompt(model string, v1 bool, cfg config.PromptConfig, environment promptEnvironment, override *config.PromptEnvironmentConfig) string {
	environment = applyPromptEnvironment(environment, override)
	apiModel := promptModel(model)
	if v1 {
		base := opencodePromptV1Windows
		if cfg.CustomBaseV1 {
			base = cfg.BaseV1
		}
		environmentText := opencodeEnvironmentTemplate
		if cfg.CustomEnvironment {
			environmentText = cfg.Environment
		}
		skills := opencodeSkillsV1
		if cfg.CustomSkills {
			skills = cfg.Skills
		}
		prompt := base + renderPromptTemplate(environmentText, apiModel, environment) + "\n" + skills
		if extra := strings.TrimSpace(cfg.Extra); extra != "" {
			prompt += "\n\n" + extra
		}
		return prompt
	}
	base := opencodePromptV2Windows
	if cfg.CustomBaseV2 {
		base = cfg.BaseV2
	}
	skills := opencodeSkillsV2
	if cfg.CustomSkills {
		skills = cfg.Skills
	}
	environmentText := opencodeEnvironmentV2For(environment)
	if cfg.CustomEnvironment {
		environmentText = "\n" + renderPromptTemplate(cfg.Environment, apiModel, environment)
	}
	prompt := base + skills + environmentText
	if extra := strings.TrimSpace(cfg.Extra); extra != "" {
		prompt += "\n\n" + extra
	}
	return prompt
}

// injectOfficialPrompt prepends the configured CLI system prompt to a shaped
// free-tier payload. It reports whether the payload changed; a body that
// already carries the marker is left untouched, so repeated shaping cannot
// duplicate the prompt. System One payloads are not chat bodies and are never
// touched.
func injectOfficialPrompt(payload map[string]any, protocol wire.Protocol, v1 bool, cfg config.PromptConfig, override *config.PromptEnvironmentConfig) bool {
	if payload == nil || cfg.Disabled || protocol == wire.SystemOne {
		return false
	}
	model, _ := payload["model"].(string)
	switch protocol {
	case wire.Chat:
		return injectChatPrompt(payload, model, v1, cfg, override)
	case wire.Anthropic:
		return injectAnthropicPrompt(payload, model, v1, cfg, override)
	case wire.Responses:
		return injectResponsesPrompt(payload, model, v1, cfg, override)
	default:
		return false
	}
}

// injectChatPrompt folds every system/developer message into the single system
// message the 1.x and 2.x CLIs send, then prepends it. The client's own
// environment is detected first, so its project replaces the gateway's
// directory. Client instructions keep their order and are separated from the
// official prompt by a blank line.
func injectChatPrompt(payload map[string]any, model string, v1 bool, cfg config.PromptConfig, override *config.PromptEnvironmentConfig) bool {
	messages, ok := payload["messages"].([]any)
	if !ok {
		return false
	}
	kept := make([]any, 0, len(messages)+1)
	var instructions []string
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		role, _ := message["role"].(string)
		if role != "system" && role != "developer" {
			kept = append(kept, raw)
			continue
		}
		content := message["content"]
		if carriesOpenCodePrompt(content) {
			return false
		}
		for _, part := range textFragments(content) {
			if part = strings.TrimSpace(part); part != "" {
				instructions = append(instructions, part)
			}
		}
	}
	client := strings.Join(instructions, "\n")
	system := opencodeSystemPrompt(model, v1, cfg, detectPromptEnvironment(client), override)
	if client != "" {
		system += "\n\n" + client
	}
	payload["messages"] = append([]any{map[string]any{"role": "system", "content": system}}, kept...)
	return true
}

// injectAnthropicPrompt prepends the prompt to the top-level system value,
// keeping a caller's string or block list intact after it.
func injectAnthropicPrompt(payload map[string]any, model string, v1 bool, cfg config.PromptConfig, override *config.PromptEnvironmentConfig) bool {
	system, exists := payload["system"]
	if exists && carriesOpenCodePrompt(system) {
		return false
	}
	prompt := opencodeSystemPrompt(model, v1, cfg, detectPromptEnvironment(strings.Join(textFragments(system), "\n")), override)
	if !exists {
		payload["system"] = prompt
		return true
	}
	switch value := system.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			payload["system"] = prompt
		} else {
			payload["system"] = prompt + "\n\n" + value
		}
	case []any:
		payload["system"] = append([]any{map[string]any{"type": "text", "text": prompt}}, value...)
	default:
		payload["system"] = prompt
	}
	return true
}

// injectResponsesPrompt prepends the prompt to the Responses instructions.
func injectResponsesPrompt(payload map[string]any, model string, v1 bool, cfg config.PromptConfig, override *config.PromptEnvironmentConfig) bool {
	instructions, exists := payload["instructions"]
	if exists && carriesOpenCodePrompt(instructions) {
		return false
	}
	prompt := opencodeSystemPrompt(model, v1, cfg, detectPromptEnvironment(strings.Join(textFragments(instructions), "\n")), override)
	if text, ok := instructions.(string); exists && ok {
		if strings.TrimSpace(text) == "" {
			payload["instructions"] = prompt
		} else {
			payload["instructions"] = prompt + "\n\n" + text
		}
		return true
	}
	payload["instructions"] = prompt
	return true
}

// textFragments flattens a system or instruction value into its text: a plain
// string, a list of content blocks, or a block object carrying text or content.
func textFragments(value any) []string {
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{typed}
	case []any:
		var parts []string
		for _, item := range typed {
			parts = append(parts, textFragments(item)...)
		}
		return parts
	case map[string]any:
		if text, ok := typed["text"].(string); ok {
			return textFragments(text)
		}
		if content, ok := typed["content"]; ok {
			return textFragments(content)
		}
	}
	return nil
}

// carriesOpenCodePrompt reports whether any text inside value already contains
// the marker.
func carriesOpenCodePrompt(value any) bool {
	for _, part := range textFragments(value) {
		if strings.Contains(part, opencodePromptMarker) || strings.Contains(part, opencodePromptMarkerV2) {
			return true
		}
	}
	return false
}
