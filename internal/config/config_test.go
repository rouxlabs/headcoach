package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// isolate points every default path into a temporary directory.
func isolate(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HC_STATE_DIR", filepath.Join(tmp, "default-state"))
	t.Setenv("HC_DATA_DIR", filepath.Join(tmp, "default-data"))
	t.Setenv("HC_CONFIG", filepath.Join(tmp, "no-such-config.toml"))
	t.Setenv("HC_PORT", "")
	t.Setenv("HC_DB", "")
	return tmp
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, path string) *Config {
	t.Helper()
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestDefaultsWhenTheFileIsMissing(t *testing.T) {
	tmp := isolate(t)
	cfg := load(t, filepath.Join(tmp, "missing.toml"))
	if cfg.Host != "127.0.0.1" || cfg.Port != 7337 || cfg.URL() != "http://127.0.0.1:7337" {
		t.Errorf("address = %s %d %s", cfg.Host, cfg.Port, cfg.URL())
	}
	if !reflect.DeepEqual(cfg.Profiles, []Profile{{"default", "claude"}}) {
		t.Errorf("profiles = %v", cfg.Profiles)
	}
	if len(cfg.RepoRoots) != 0 || cfg.TmuxSession != "" || !cfg.PrivateTmux || cfg.Mouse {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	if want := filepath.Join(tmp, "default-state", "board.db"); cfg.DB != want {
		t.Errorf("db = %s, want %s", cfg.DB, want)
	}
}

func TestLoadFromTOML(t *testing.T) {
	tmp := isolate(t)
	path := filepath.Join(tmp, "config.toml")
	write(t, path, `host = "0.0.0.0"
port = 9001
db = "`+tmp+`/data/board.db"
repo_roots = ["~/code", "/srv/repos"]
tmux_session = "main"
private_tmux = false
mouse = true
[profiles]
team = "claude-team"
personal = "claude-personal --verbose"
`)
	cfg := load(t, path)
	home, _ := os.UserHomeDir()
	if cfg.Host != "0.0.0.0" || cfg.Port != 9001 || cfg.URL() != "http://0.0.0.0:9001" {
		t.Errorf("address = %s %d", cfg.Host, cfg.Port)
	}
	if cfg.DB != filepath.Join(tmp, "data", "board.db") || cfg.SessionsDir() != filepath.Join(tmp, "data", "sessions") {
		t.Errorf("db = %s, sessions = %s", cfg.DB, cfg.SessionsDir())
	}
	if want := []Profile{{"team", "claude-team"}, {"personal", "claude-personal --verbose"}}; !reflect.DeepEqual(cfg.Profiles, want) {
		t.Errorf("profiles = %v, want the order of the file", cfg.Profiles)
	}
	if want := []string{filepath.Join(home, "code"), "/srv/repos"}; !reflect.DeepEqual(cfg.RepoRoots, want) {
		t.Errorf("repo roots = %v", cfg.RepoRoots)
	}
	if cfg.TmuxSession != "main" || cfg.PrivateTmux || cfg.TmuxSocket() != "" {
		t.Errorf("tmux = %q %v %q", cfg.TmuxSession, cfg.PrivateTmux, cfg.TmuxSocket())
	}
	if !cfg.Mouse {
		t.Error("mouse = true was not read")
	}
}

func TestPartialTOMLKeepsDefaults(t *testing.T) {
	tmp := isolate(t)
	path := filepath.Join(tmp, "config.toml")
	write(t, path, "port = \"8123\"\ndb = \"~/x/board.db\"\n")
	cfg := load(t, path)
	home, _ := os.UserHomeDir()
	if cfg.Port != 8123 || cfg.Host != "127.0.0.1" || cfg.DB != filepath.Join(home, "x", "board.db") {
		t.Errorf("got %d %s %s", cfg.Port, cfg.Host, cfg.DB)
	}
	if !reflect.DeepEqual(cfg.Profiles, []Profile{{"default", "claude"}}) {
		t.Errorf("profiles = %v", cfg.Profiles)
	}
}

func TestBrokenFileIsAnError(t *testing.T) {
	tmp := isolate(t)
	path := filepath.Join(tmp, "config.toml")
	write(t, path, "port = [1]\n")
	if _, err := Load(path); err == nil {
		t.Error("a port that is a list was accepted")
	}
	write(t, path, "host = \n")
	if _, err := Load(path); err == nil {
		t.Error("invalid TOML was accepted")
	}
}

func TestEnvOverrides(t *testing.T) {
	tmp := isolate(t)
	path := filepath.Join(tmp, "config.toml")
	write(t, path, "port = 9001\ndb = \""+tmp+"/from-file.db\"\n")
	t.Setenv("HC_PORT", "9555")
	t.Setenv("HC_DB", filepath.Join(tmp, "from-env", "b.db"))
	cfg := load(t, path)
	if cfg.Port != 9555 || cfg.DB != filepath.Join(tmp, "from-env", "b.db") {
		t.Errorf("got %d %s", cfg.Port, cfg.DB)
	}
	t.Setenv("HC_DB", "~/elsewhere/b.db")
	home, _ := os.UserHomeDir()
	if cfg := load(t, filepath.Join(tmp, "missing.toml")); cfg.DB != filepath.Join(home, "elsewhere", "b.db") {
		t.Errorf("db = %s", cfg.DB)
	}
}

func TestProfileFor(t *testing.T) {
	two := &Config{Profiles: []Profile{{"team", "claude-team"}, {"personal", "claude-personal"}}}
	withDefault := &Config{Profiles: []Profile{{"team", "claude-team"}, {"default", "claude"}}}
	suffix := &Config{Profiles: []Profile{{"team", "claude-team"}, {"dreamteam", "claude-dreamteam"}}}
	for _, c := range []struct {
		cfg  *Config
		dir  string
		want string
	}{
		{two, "/Users/x/.claude-team", "team"},
		{two, "/Users/x/.claude-team/", "team"},
		{two, "/Users/x/.claude-personal", "personal"},
		{two, "", "team"},
		{two, "/Users/x/.claude", "team"},
		{withDefault, "", "default"},
		{withDefault, "/Users/x/.claude", "default"},
		{withDefault, "/Users/x/.claude-team", "team"},
		{&Config{}, "/x/.claude-team", "default"},
		{suffix, "/Users/x/.claude-dreamteam", "dreamteam"},
	} {
		if got := c.cfg.ProfileFor(c.dir); got != c.want {
			t.Errorf("ProfileFor(%q) = %q, want %q", c.dir, got, c.want)
		}
	}
}

func TestLauncher(t *testing.T) {
	cfg := &Config{Profiles: []Profile{{"team", "claude-team"}, {"personal", "claude-personal"}}}
	if cfg.Launcher("personal") != "claude-personal" || cfg.Launcher("gone") != "claude-team" || (&Config{}).Launcher("x") != "claude" {
		t.Error("the launcher of a profile is wrong")
	}
}
