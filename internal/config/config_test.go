package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Every wake points a chat driver's run at its brief, so where that is comes from
// the configuration a real run loads, a ~/ in it expanded.
func TestTheBriefComesFromTheConfiguration(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DRIVE_CONFIG_DIR", dir)
	t.Setenv("DRIVE_STATE_DIR", t.TempDir())
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".agents/skills/drive-session/BOT.md"); c.Brief != want {
		t.Fatalf("by default %q, not %q", want, c.Brief)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("brief = \"~/mine.md\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "mine.md"); c.Brief != want {
		t.Fatalf("set, %q, not %q", want, c.Brief)
	}
}
