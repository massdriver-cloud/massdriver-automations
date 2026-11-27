package paramstogit

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"strconv"
	"strings"
)

// toStdout prints what push would write, as one block for each sync. Logs go to stderr, so stdout holds only these
// blocks.
func (p *ParamsToGit) toStdout(log *slog.Logger, rt *route, instance, deploymentID string, c change) {
	action := "write"
	if c.content == nil {
		action = "delete"
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "--- params-to-git %s %s\n", action, line(c.path))
	for _, kv := range [][2]string{
		{"instance", instance},
		{"deployment", deploymentID},
		{"mapping", rt.name},
		{"repo", rt.repo},
		{"branch", rt.branch},
		{"message", c.message},
	} {
		if kv[1] != "" {
			fmt.Fprintf(&b, "%s: %s\n", kv[0], line(kv[1]))
		}
	}
	b.Write(c.content)
	b.WriteString("--- end\n")
	if _, err := p.out.Write(b.Bytes()); err != nil {
		log.Error("sync failed", "err", err)
		return
	}
	log.Info("params printed", "strategy", StrategyStdout)
}

// line keeps a value on one line, so a newline in a commit message cannot end the block early.
func line(s string) string {
	if strings.ContainsAny(s, "\r\n") {
		return strconv.Quote(s)
	}
	return s
}

// toDirectory writes the file under debug.directory, as push would write it to the branch. Disk errors are logged
// and not retried: a full disk or a missing permission does not fix itself in the minutes that Massdriver retries.
func (p *ParamsToGit) toDirectory(log *slog.Logger, c change) {
	done, err := writeLocal(p.cfg.Debug.Directory, c)
	switch {
	case err != nil:
		log.Error("sync failed", "strategy", StrategyDirectory, "err", err)
	case !done:
		log.Debug("sync skipped", "reason", "unchanged")
	case c.content == nil:
		log.Info("params deleted", "strategy", StrategyDirectory)
	default:
		log.Info("params written", "strategy", StrategyDirectory)
	}
}

// writeLocal writes or deletes the change's file under dir when the file there differs, and reports whether it did.
// os.Root keeps every path inside dir, even through a symlink.
func writeLocal(dir string, c change) (bool, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return false, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false, err
	}
	defer root.Close()

	old, err := root.ReadFile(c.path)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if same(file{exists: exists, content: old}, c.content) {
		return false, nil
	}
	if c.content == nil {
		return true, root.Remove(c.path)
	}
	if err := root.MkdirAll(path.Dir(c.path), 0o750); err != nil {
		return false, err
	}
	// A reader never sees half a file: the content goes to a temporary file that replaces the old one in one step.
	tmp := c.path + ".tmp"
	if err := root.WriteFile(tmp, c.content, 0o600); err != nil {
		return false, err
	}
	if err := root.Rename(tmp, c.path); err != nil {
		_ = root.Remove(tmp)
		return false, err
	}
	return true, nil
}
