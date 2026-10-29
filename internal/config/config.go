// Package config finds drive's directories and reads its settings.
//
// Settings live in ~/.config/drive/config.toml, every key optional. The
// webhook that wakes a chat driver is a file per driver under `webhooks/`,
// named by the batch it drives (`drive batch new … --webhook NAME`), mode
// 0600, as two lines: URL=<url> and AUTH=<the full Authorization header
// value>; a batch that names none reads `webhook` beside config.toml.
// DRIVE_CONFIG_DIR and DRIVE_STATE_DIR move both directories, which is how
// tests keep away from the real ones.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is drive's settings with defaults filled in.
type Config struct {
	Home      string
	ConfigDir string
	StateDir  string

	// Launcher starts Claude Code in a new block: "x" places it on the
	// least-loaded account, "x-<account>" on that one. It runs in a login
	// zsh, so it may be a shell function.
	Launcher string `toml:"launcher"`
	// PrimaryName is the name the launcher knows the primary account
	// (~/.claude) by: Launcher-<name> starts a session there. It defaults to
	// $CLAUDE_PRIMARY_NAME, then $HEADROOM_PRIMARY_NAME, as the owner's shell
	// names it; unknown, the primary is never offered for a relaunch.
	PrimaryName string `toml:"primary_name"`
	// LaunchEnv is set for every session drive opens. The default turns off
	// Bench's peer nudges, which hold a session at a prompt nobody answers.
	LaunchEnv map[string]string `toml:"launch_env"`
	// Notice is the text a session reads before the first step drive sends it.
	Notice string `toml:"notice"`
	// RexSession is where new batches open their sessions: the session drive
	// runs in ($REX_SESSION, an id a batch records by its label) when unset,
	// and Rex's default when that is empty too.
	RexSession string `toml:"rex_session"`
	// TZ is the zone times are shown in, such as "Europe/London"; empty is local.
	TZ string `toml:"tz"`
	// AfterWake runs after each wake is posted, such as archiving the chat
	// the wake went to. Its failure is logged and does not fail the wake.
	AfterWake []string `toml:"after_wake"`

	Webhook Webhook `toml:"-"`
}

// Webhook is where a chat driver is woken.
type Webhook struct {
	URL  string
	Auth string
}

// Load reads the settings. A missing config.toml or webhook file is not an error.
func Load() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, err
	}
	c := Config{
		Home:        home,
		ConfigDir:   envOr("DRIVE_CONFIG_DIR", filepath.Join(home, ".config/drive")),
		StateDir:    envOr("DRIVE_STATE_DIR", filepath.Join(home, ".local/state/drive")),
		Launcher:    "x",
		PrimaryName: envOr("CLAUDE_PRIMARY_NAME", os.Getenv("HEADROOM_PRIMARY_NAME")),
		LaunchEnv:   map[string]string{"PLANLAB_AGENT_NUDGES": "off"},
		Notice:      filepath.Join(home, ".agents/skills/drive-session/NOTICE.md"),
	}
	path := filepath.Join(c.ConfigDir, "config.toml")
	if _, err := toml.DecodeFile(path, &c); err != nil && !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("%s does not load, so none of its settings can be trusted: %w", path, err)
	}
	c.Notice = Expand(home, c.Notice)
	if c.RexSession == "" {
		c.RexSession = os.Getenv("REX_SESSION") // the Rex session drive runs in
	}
	c.Webhook, err = readWebhook(filepath.Join(c.ConfigDir, "webhook"))
	return c, err
}

// Expand resolves a leading ~/.
func Expand(home, p string) string {
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func readWebhook(path string) (Webhook, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return Webhook{}, nil
	}
	if err != nil {
		return Webhook{}, err
	}
	defer f.Close()
	var w Webhook
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "URL="):
			w.URL = strings.TrimPrefix(line, "URL=")
		case strings.HasPrefix(line, "AUTH="):
			w.Auth = strings.TrimPrefix(strings.TrimPrefix(line, "AUTH="), "Authorization: ")
		}
	}
	return w, sc.Err()
}

// WebhookPath is where the webhook file a batch names is read from; "" is
// the one file a batch that names none reads.
func (c Config) WebhookPath(name string) string {
	if name == "" {
		return filepath.Join(c.ConfigDir, "webhook")
	}
	return filepath.Join(c.ConfigDir, "webhooks", name)
}

// LoadWebhook reads the webhook a batch names, fresh each time, so a file
// written after the batch started is the one its wakes use.
func (c Config) LoadWebhook(name string) (Webhook, error) {
	if name == "" {
		return c.Webhook, nil
	}
	return readWebhook(c.WebhookPath(name))
}
