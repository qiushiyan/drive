package drive

import "github.com/qiushiyan/drive/internal/snippet"

// Snippet expands the owner's snippet key as a session in dir would get it,
// with text where the owner would type after it.
func (d *Driver) Snippet(dir, key, text string) (snippet.Snippet, error) {
	s, err := snippet.Expand(d.Cfg.Home, dir, key, text)
	if err != nil {
		return s, &Refused{Reason: err.Error()}
	}
	return s, nil
}

// Snippets lists the snippet keys that resolve in dir, each with its file.
func (d *Driver) Snippets(dir string) (from map[string]string, keys []string, err error) {
	return snippet.List(d.Cfg.Home, dir)
}
