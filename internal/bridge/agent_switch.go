package bridge

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/core"
	"github.com/HunterLzap/slopguard/skills"
	"github.com/pelletier/go-toml/v2"
)

type format uint8

const (
	jsonc format = iota
	tomlConfig
	json5Config
	dshConfig
)

type agentDefinition struct {
	id, config, skillDir, detect string
	format                       format
	keys                         []string
}

var standardMCPKey = []string{"mcpServers", "slopguard"}

var agentDefinitions = []agentDefinition{
	{id: "codex", config: ".codex/config.toml", skillDir: ".codex/skills", format: tomlConfig},
	{id: "claude", config: ".claude.json", skillDir: ".claude/skills", detect: ".claude/settings.json", keys: standardMCPKey},
	{id: "cursor", config: ".cursor/mcp.json", skillDir: ".cursor/skills", keys: standardMCPKey},
	{id: "antigravity", config: ".gemini/config/mcp_config.json", skillDir: ".gemini/config/skills", keys: standardMCPKey},
	{id: "antigravity", config: ".gemini/antigravity/mcp_config.json", skillDir: ".gemini/antigravity/skills", keys: standardMCPKey},
	{id: "antigravity", config: ".gemini/antigravity-cli/mcp_config.json", skillDir: ".gemini/antigravity-cli/skills", keys: standardMCPKey},
	{id: "gemini", config: ".gemini/settings.json", skillDir: ".gemini/skills", keys: standardMCPKey},
	{id: "dsh", config: ".dsh/cordis.patch.yml", detect: ".dsh", skillDir: ".dsh/skills", format: dshConfig},
	{id: "openclaw", config: ".openclaw/openclaw.json", skillDir: ".openclaw/skills", format: json5Config, keys: []string{"mcp", "servers", "slopguard"}},
}

type AgentSwitch struct {
	Home       string
	ConfigDir  string
	Executable string
}

type Agent struct {
	ID         string `json:"id"`
	ConfigPath string `json:"config_path"`
	SkillPath  string `json:"skill_path"`
	Enabled    bool   `json:"enabled"`
}

type detectedAgent struct {
	Agent
	definition agentDefinition
	file       fileState
}

type MCPConfig struct {
	JSON string `json:"json"`
	TOML string `json:"toml"`
}

func ExportGenericMCPConfig(command string) (MCPConfig, error) {
	data, err := json.MarshalIndent(map[string]any{"mcpServers": map[string]mcpCommand{"slopguard": newCommand(command)}}, "", "  ")
	return MCPConfig{JSON: string(data) + "\n", TOML: tomlSnippet(command)}, err
}

type mcpCommand struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

func newCommand(command string) mcpCommand {
	return mcpCommand{Command: command, Args: []string{"mcp"}}
}

func (a AgentSwitch) List() ([]Agent, error) {
	found, err := a.detect("")
	if err != nil {
		return nil, err
	}
	out := make([]Agent, 0, len(found))
	for _, item := range found {
		out = append(out, item.Agent)
	}
	return out, nil
}

func (a AgentSwitch) detect(only string) ([]detectedAgent, error) {
	home, err := core.ProjectPath(a.Home)
	if err != nil {
		return nil, err
	}
	var out []detectedAgent
	seen := map[string]bool{}
	for _, def := range agentDefinitions {
		if (only != "" && only != def.id) || seen[def.id] {
			continue
		}
		path := filepath.Join(home, filepath.FromSlash(def.config))
		f, err := readFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", def.id, err)
		}
		if !f.exists {
			if def.detect == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(def.detect))); errors.Is(err, fs.ErrNotExist) {
				continue
			} else if err != nil {
				return nil, err
			}
		}
		skill := filepath.Join(home, filepath.FromSlash(def.skillDir), "slopguard", "SKILL.md")
		enabled, err := def.enabled(f.data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", def.id, err)
		}
		out = append(out, detectedAgent{Agent: Agent{ID: def.id, ConfigPath: path, SkillPath: skill, Enabled: enabled}, definition: def, file: f})
		seen[def.id] = true
	}
	return out, nil
}

func (a AgentSwitch) Set(id string, enable bool) (err error) {
	if a.Executable == "" && enable {
		return fmt.Errorf("SlopGuard executable path is required")
	}
	found, err := a.detect(id)
	if err != nil {
		return err
	}
	var agent detectedAgent
	for _, entry := range found {
		if entry.ID == id {
			agent = entry
			break
		}
	}
	if agent.ID == "" {
		return fmt.Errorf("agent %q was not detected", id)
	}
	home, err := core.ProjectPath(a.Home)
	if err != nil {
		return err
	}
	for _, path := range []string{agent.ConfigPath, agent.SkillPath} {
		if _, err := containedPath(home, path); err != nil {
			return err
		}
	}
	base := filepath.Join(a.ConfigDir, "backups", id)
	if err := os.MkdirAll(base, 0700); err != nil {
		return err
	}
	lockPath := filepath.Join(base, "edit.lock")
	lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("lock %s config: %w", id, err)
	}
	defer func() { err = errors.Join(err, lock.Close(), os.Remove(lockPath)) }()
	config, err := readFile(agent.ConfigPath)
	if err != nil {
		return err
	}
	skill, err := readFile(agent.SkillPath)
	if err != nil {
		return err
	}
	active := filepath.Join(base, "active")
	current, snapshot, err := activeSnapshot(base, active, agent.Agent)
	if err != nil {
		return err
	}
	if enable {
		if current != "" && snapshot.Command == a.Executable && snapshot.InstalledConfig == fileHash(config.data) && bytes.Equal(skill.data, []byte(skills.Markdown)) {
			return nil
		}
		edited, err := agent.definition.patch(config.data, a.Executable)
		if err != nil {
			return err
		}
		if current == "" {
			// Publish the complete snapshot before touching the user's configuration.
			dir, err := os.MkdirTemp(base, time.Now().UTC().Format("20060102T150405")+"-")
			if err != nil {
				return err
			}
			current = filepath.Base(dir)
			if err := core.WriteFileAtomic(filepath.Join(dir, "config.original"), config.data, 0600, true, nil); err != nil {
				return err
			}
			if err := core.WriteFileAtomic(filepath.Join(dir, "skill.original"), skill.data, 0600, true, nil); err != nil {
				return err
			}
			snapshot = agentSnapshot{ConfigPath: agent.ConfigPath, SkillPath: agent.SkillPath, Command: a.Executable,
				ConfigExists: config.exists, SkillExists: skill.exists, ConfigMode: config.mode, SkillMode: skill.mode,
				InstalledConfig: fileHash(edited)}
			metadata, err := json.Marshal(snapshot)
			if err != nil {
				return err
			}
			if err := core.WriteFileAtomic(filepath.Join(dir, "snapshot.json"), metadata, 0600, true, nil); err != nil {
				return err
			}
		}
		if err := applyFile(agent.ConfigPath, config, fileState{data: edited, mode: config.mode, exists: true}); err != nil {
			return err
		}
		if err := applyFile(agent.SkillPath, skill, fileState{data: []byte(skills.Markdown), mode: skill.mode, exists: true}); err != nil {
			return errors.Join(err, config.restore(agent.ConfigPath))
		}
		if err := core.WriteFileAtomic(active, []byte(current), 0600, false, nil); err != nil {
			return errors.Join(err, config.restore(agent.ConfigPath), skill.restore(agent.SkillPath))
		}
		return nil
	}
	desired := config
	targetSkill := skill
	if config.exists && current != "" && snapshot.InstalledConfig == fileHash(config.data) {
		desired, err = backupFile(base, current, "config", snapshot.ConfigExists, snapshot.ConfigMode)
		if err != nil {
			return err
		}
	}
	if desired.exists {
		present, err := agent.definition.enabled(desired.data)
		if err != nil {
			return err
		}
		if present {
			desired.data, err = agent.definition.patch(desired.data, "")
			if err != nil {
				return err
			}
		}
	}
	if current != "" && bytes.Equal(skill.data, []byte(skills.Markdown)) {
		targetSkill, err = backupFile(base, current, "skill", snapshot.SkillExists, snapshot.SkillMode)
		if err != nil {
			return err
		}
	}
	if err := applyFile(agent.ConfigPath, config, desired); err != nil {
		return err
	}
	if err := applyFile(agent.SkillPath, skill, targetSkill); err != nil {
		return errors.Join(err, config.restore(agent.ConfigPath))
	}
	if current != "" {
		if err := os.Remove(active); err != nil {
			return errors.Join(err, config.restore(agent.ConfigPath), skill.restore(agent.SkillPath))
		}
	}
	return nil
}

type agentSnapshot struct {
	ConfigPath, SkillPath, Command, InstalledConfig string
	ConfigExists, SkillExists                       bool
	ConfigMode, SkillMode                           fs.FileMode
}

func activeSnapshot(base, active string, current Agent) (string, agentSnapshot, error) {
	var s agentSnapshot
	data, err := os.ReadFile(active)
	if errors.Is(err, fs.ErrNotExist) {
		return "", s, nil
	}
	if err != nil {
		return "", s, err
	}
	folder := strings.TrimSpace(string(data))
	if !filepath.IsLocal(folder) || filepath.Base(folder) != folder || folder == "." {
		return "", s, fmt.Errorf("invalid agent snapshot location")
	}
	raw, err := os.ReadFile(filepath.Join(base, folder, "snapshot.json"))
	if err != nil {
		return "", s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", s, err
	}
	if s.ConfigPath != current.ConfigPath || s.SkillPath != current.SkillPath {
		return "", s, fmt.Errorf("active snapshot belongs to a different %s installation", current.ID)
	}
	return folder, s, nil
}

func backupFile(base, folder, kind string, exists bool, mode fs.FileMode) (fileState, error) {
	data, err := os.ReadFile(filepath.Join(base, folder, kind+".original"))
	return fileState{data: data, exists: exists, mode: mode}, err
}

func fileHash(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func applyFile(path string, original, wanted fileState) error {
	if original.exists == wanted.exists && original.mode == wanted.mode && bytes.Equal(original.data, wanted.data) {
		return nil
	}
	current, err := readFile(path)
	if err != nil {
		return err
	}
	if current.exists != original.exists || current.mode != original.mode || !bytes.Equal(current.data, original.data) {
		return fmt.Errorf("%s changed during edit", path)
	}
	if !wanted.exists {
		return os.Remove(path)
	}
	return core.WriteFileAtomic(path, wanted.data, wanted.mode, !original.exists, nil)
}

func (a agentDefinition) enabled(data []byte) (bool, error) {
	if len(data) == 0 {
		return false, nil
	}
	switch a.format {
	case tomlConfig:
		var value map[string]any
		if err := toml.Unmarshal(data, &value); err != nil {
			return false, err
		}
		servers, _ := value["mcp_servers"].(map[string]any)
		_, found := servers["slopguard"]
		return found, nil
	case dshConfig:
		doc, err := parseDSH(data)
		return findDSHRow(doc), err
	default:
		root, err := parseJSONConfig(data, a.format == json5Config)
		if err != nil {
			return false, err
		}
		return jsonNodeAt(root, a.keys) != nil, nil
	}
}

func (a agentDefinition) patch(data []byte, command string) ([]byte, error) {
	switch a.format {
	case tomlConfig:
		return patchTOML(data, command)
	case dshConfig:
		return patchDSH(data, command)
	default:
		var config []byte
		if command != "" {
			var err error
			config, err = json.Marshal(newCommand(command))
			if err != nil {
				return nil, err
			}
		}
		return patchJSON(data, a.format == json5Config, a.keys, config)
	}
}
