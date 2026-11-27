// Package config reads the one TOML file of headcoach. Every key is optional.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Profile is one way to launch Claude Code, for example one per account.
type Profile struct {
	Name    string
	Command string
}

// Config holds every setting, with its default when the file does not set it.
type Config struct {
	Host string
	Port int
	// DB is the path of the SQLite file. The tmux socket and the session files live next to it.
	DB string
	// Profiles are kept in the order of the file: the first one is the fallback.
	Profiles []Profile
	// RepoRoots are scanned for git repositories offered in "New task".
	RepoRoots []string
	// PrivateTmux runs the sessions the board starts in a tmux server of its own.
	PrivateTmux bool
	// TmuxSession is, without PrivateTmux, the tmux session new windows open in.
	TmuxSession string
	// JiraURL is the base of a Jira issue link. Empty means no links.
	JiraURL string
	// ContextWindow is the size of the model's context window, for the context gauge of a session
	// whose status line has not reported it.
	ContextWindow int
	// AskModel answers questions about a session. Empty means the profile's default model.
	AskModel string
	// PersonaPattern matches the skills that count as the persona a session works as.
	PersonaPattern string
	// TestPattern matches the shell commands that run a repository's tests, for the review queue.
	TestPattern string
	// Theme is the color theme of the terminal board, and the one the web board starts with.
	Theme string
	// Workdir is the folder a bare name given to `hc new --dir` resolves under.
	Workdir string
}

func xdg(env, fallback string) string {
	if dir := os.Getenv(env); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, fallback)
}

func dir(env, xdgEnv, fallback string) string {
	if path := os.Getenv(env); path != "" {
		return path
	}
	return filepath.Join(xdg(xdgEnv, fallback), "headcoach")
}

// Path is where the config file lives.
func Path() string {
	if path := os.Getenv("HC_CONFIG"); path != "" {
		return path
	}
	return filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "headcoach", "config.toml")
}

// StateDir holds the database, the daemon log and the board's tmux socket.
func StateDir() string { return dir("HC_STATE_DIR", "XDG_STATE_HOME", ".local/state") }

// DataDir holds the scripts Claude Code calls.
func DataDir() string { return dir("HC_DATA_DIR", "XDG_DATA_HOME", ".local/share") }

// Default is the configuration of a machine without a config file.
func Default() *Config {
	return &Config{
		Host:           "127.0.0.1",
		Port:           7337,
		DB:             filepath.Join(StateDir(), "board.db"),
		Profiles:       []Profile{{Name: "default", Command: "claude"}},
		PrivateTmux:    true,
		ContextWindow:  200_000,
		AskModel:       "haiku",
		PersonaPattern: "(^|[-_])(developer|reviewer|architect|researcher|planner|tester)($|[-_])",
		TestPattern:    DefaultTestPattern,
		Theme:          "terminal",
	}
}

// DefaultTestPattern matches the usual ways to run a repository's tests.
const DefaultTestPattern = `\b(make (check|test)|go test|cargo test|pytest|tox|(npm|pnpm|yarn|bun) (run )?test|mvn (verify|test)|gradlew? (check|test)|ctest|rspec|phpunit)\b`

// file mirrors the TOML file. Pointers tell a missing key from a zero value.
type file struct {
	Host           *string           `toml:"host"`
	Port           any               `toml:"port"`
	DB             *string           `toml:"db"`
	Profiles       map[string]string `toml:"profiles"`
	RepoRoots      []string          `toml:"repo_roots"`
	PrivateTmux    *bool             `toml:"private_tmux"`
	TmuxSession    *string           `toml:"tmux_session"`
	JiraURL        *string           `toml:"jira_url"`
	ContextWindow  any               `toml:"context_window"`
	AskModel       *string           `toml:"ask_model"`
	PersonaPattern *string           `toml:"persona_pattern"`
	TestPattern    *string           `toml:"test_pattern"`
	Theme          *string           `toml:"theme"`
	Workdir        *string           `toml:"workdir"`
}

// Load reads the config file at path, or at Path() when path is empty. A missing file is not an error.
func Load(path string) (*Config, error) {
	if path == "" {
		path = Path()
	}
	cfg := Default()
	var raw file
	meta, err := toml.DecodeFile(path, &raw)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return nil, fmt.Errorf("%s: %w", path, err)
	default:
		set(&cfg.Host, raw.Host)
		if cfg.Port, err = number("port", raw.Port, cfg.Port); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if raw.DB != nil {
			cfg.DB = ExpandUser(*raw.DB)
		}
		if len(raw.Profiles) > 0 {
			cfg.Profiles = nil
			for _, key := range meta.Keys() { // a map has no order, the file has one
				if len(key) == 2 && key[0] == "profiles" {
					cfg.Profiles = append(cfg.Profiles, Profile{Name: key[1], Command: raw.Profiles[key[1]]})
				}
			}
		}
		for _, root := range raw.RepoRoots {
			cfg.RepoRoots = append(cfg.RepoRoots, ExpandUser(root))
		}
		set(&cfg.PrivateTmux, raw.PrivateTmux)
		set(&cfg.TmuxSession, raw.TmuxSession)
		set(&cfg.JiraURL, raw.JiraURL)
		if cfg.ContextWindow, err = number("context_window", raw.ContextWindow, cfg.ContextWindow); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		set(&cfg.AskModel, raw.AskModel)
		set(&cfg.PersonaPattern, raw.PersonaPattern)
		set(&cfg.TestPattern, raw.TestPattern)
		set(&cfg.Theme, raw.Theme)
		if raw.Workdir != nil {
			cfg.Workdir = ExpandUser(*raw.Workdir)
		}
	}
	if theme := os.Getenv("HC_THEME"); theme != "" {
		cfg.Theme = theme
	}
	if port := os.Getenv("HC_PORT"); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil {
			return nil, fmt.Errorf("HC_PORT is not a number: %q", port)
		}
		cfg.Port = n
	}
	if db := os.Getenv("HC_DB"); db != "" {
		cfg.DB = ExpandUser(db)
	}
	return cfg, nil
}

// number reads a key that may be written as a number or as a quoted number.
func number(key string, value any, fallback int) (int, error) {
	switch v := value.(type) {
	case nil:
		return fallback, nil
	case int64:
		return int(v), nil
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n, nil
		}
	}
	return 0, fmt.Errorf("%s must be a number", key)
}

func set[T any](target *T, value *T) {
	if value != nil {
		*target = *value
	}
}

// ExpandUser turns a leading ~ into the home directory.
func ExpandUser(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[1:])
}

// RealPath resolves symlinks as far as the path exists, like realpath does.
func RealPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(RealPath(parent), filepath.Base(path))
}

// URL is the address of the daemon.
func (c *Config) URL() string { return fmt.Sprintf("http://%s:%d", c.Host, c.Port) }

// TmuxSocket is the socket of the tmux server the board starts sessions in.
// Empty means your own default server.
func (c *Config) TmuxSocket() string {
	if !c.PrivateTmux {
		return ""
	}
	// Resolved, so a state directory reached through a symlink still names one server.
	return RealPath(filepath.Join(filepath.Dir(c.DB), "tmux.sock"))
}

// TmuxConf is the config file of the board's own tmux server.
func (c *Config) TmuxConf() string { return filepath.Join(filepath.Dir(c.DB), "tmux.conf") }

// SessionsDir holds one small file per Claude session id, with the short task id. The status line reads it.
func (c *Config) SessionsDir() string { return filepath.Join(filepath.Dir(c.DB), "sessions") }

// HandoffPath is where a task's handoff file lives, next to the board's database: never in a checkout.
func (c *Config) HandoffPath(taskID string) string {
	return filepath.Join(filepath.Dir(c.DB), "handoff", taskID+".md")
}

// HasProfile tells whether a profile of that name is configured.
func (c *Config) HasProfile(name string) bool {
	for _, p := range c.Profiles {
		if p.Name == name {
			return true
		}
	}
	return false
}

// ProfileNames lists the profiles in the order of the file.
func (c *Config) ProfileNames() []string {
	names := make([]string, 0, len(c.Profiles))
	for _, p := range c.Profiles {
		names = append(names, p.Name)
	}
	return names
}

// FirstProfile is the profile used when none is named.
func (c *Config) FirstProfile() string {
	if len(c.Profiles) == 0 {
		return "default"
	}
	return c.Profiles[0].Name
}

// Launcher is the command that starts Claude Code for a profile.
func (c *Config) Launcher(profile string) string {
	for _, p := range c.Profiles {
		if p.Name == profile && p.Command != "" {
			return p.Command
		}
	}
	if len(c.Profiles) > 0 {
		return c.Profiles[0].Command
	}
	return "claude"
}

// ProfileFor maps a CLAUDE_CONFIG_DIR to a profile name: ~/.claude-team matches the team profile.
func (c *Config) ProfileFor(configDir string) string {
	name := ""
	if configDir != "" {
		name = filepath.Base(configDir)
	}
	for _, p := range c.Profiles {
		if name == p.Name || strings.HasSuffix(name, "-"+p.Name) {
			return p.Name
		}
	}
	if c.HasProfile("default") {
		return "default"
	}
	return c.FirstProfile()
}
