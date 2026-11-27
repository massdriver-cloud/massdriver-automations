package paramstogit

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

const maskedParams = `{
  "big": 12345678901234567890,
  "database": {
    "name": "app",
    "token": "[SENSITIVE]"
  },
  "password": "[SENSITIVE]",
  "size": "large"
}
`

func TestStdoutPrintsMaskedParams(t *testing.T) {
	h := newHarness(t, "defaults:\n  strategy: stdout\n", "")
	var out bytes.Buffer
	h.p.out = &out
	if code := h.deploy("ev-1", "dep-1", "large", nil); code != http.StatusNoContent {
		t.Fatalf("status %d", code)
	}
	want := "--- params-to-git write " + filePath + "\n" +
		"instance: api-prod-database\n" +
		"deployment: dep-1\n" +
		"mapping: defaults\n" +
		"branch: main\n" +
		"message: api-prod-database: provision deployment dep-1\n" +
		maskedParams +
		"--- end\n"
	if out.String() != want {
		t.Fatalf("stdout =\n%s\nwant\n%s", out.String(), want)
	}
	if strings.Contains(out.String(), "hunter2") || strings.Contains(out.String(), "tok-123") {
		t.Fatal("plaintext secret printed")
	}
	if len(h.gh.allCommits()) != 0 {
		t.Fatalf("commits = %d, want none", len(h.gh.allCommits()))
	}

	out.Reset()
	h.api.set(instanceID, "DECOMMISSIONED", "dep-1", nil, nil)
	h.send(event("ev-2", "DECOMMISSION", "dep-2", nil))
	if !strings.HasPrefix(out.String(), "--- params-to-git delete "+filePath+"\n") || !strings.HasSuffix(out.String(), "decommission deployment dep-2\n--- end\n") {
		t.Fatalf("stdout =\n%s", out.String())
	}
}

func TestStdoutOmitsAndShowsRepo(t *testing.T) {
	h := newHarness(t, "defaults:\n  strategy: stdout\n  repo: acme/platform\n  root: deploy\n  sensitive: omit\n", "")
	var out bytes.Buffer
	h.p.out = &out
	h.deploy("ev-1", "dep-1", "large", nil)
	got := out.String()
	if !strings.Contains(got, "write deploy/"+filePath+"\n") || !strings.Contains(got, "repo: acme/platform\n") {
		t.Fatalf("stdout =\n%s", got)
	}
	if strings.Contains(got, "password") || strings.Contains(got, "token") || strings.Contains(got, "md_metadata") {
		t.Fatalf("omitted key printed:\n%s", got)
	}
}

func TestStdoutKeepsMessageOnOneLine(t *testing.T) {
	h := newHarness(t, "defaults:\n  strategy: stdout\n  commit:\n    message: \"a\\n--- end\"\n", "")
	var out bytes.Buffer
	h.p.out = &out
	h.deploy("ev-1", "dep-1", "large", nil)
	if !strings.Contains(out.String(), "message: \"a\\n--- end\"\n") || strings.Count(out.String(), "--- end\n") != 1 {
		t.Fatalf("stdout =\n%s", out.String())
	}
}

func directoryHarness(t *testing.T, extra string) (*harness, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "out")
	h := newHarness(t, "debug:\n  directory: "+dir+"\ndefaults:\n  strategy: directory\n  root: params\n"+extra, "")
	return h, filepath.Join(dir, "params", filepath.FromSlash(filePath))
}

func TestDirectoryWritesSkipsUnchangedAndDeletes(t *testing.T) {
	h, file := directoryHarness(t, "")
	if code := h.deploy("ev-1", "dep-1", "large", nil); code != http.StatusNoContent {
		t.Fatalf("status %d", code)
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != maskedParams {
		t.Fatalf("params.json =\n%s\nwant\n%s", got, maskedParams)
	}
	before, _ := os.Stat(file)

	// Each write replaces the file, so the same file after a deployment means it was not written.
	h.deploy("ev-2", "dep-2", "large", nil)
	after, err := os.Stat(file)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("unchanged params rewrote the file (err %v)", err)
	}
	h.deploy("ev-3", "dep-3", "small", nil)
	if got, _ := os.ReadFile(file); !strings.Contains(string(got), `"size": "small"`) {
		t.Fatalf("params.json =\n%s", got)
	}
	if entries, _ := os.ReadDir(filepath.Dir(file)); len(entries) != 1 {
		t.Fatalf("directory has %d entries, want only params.json", len(entries))
	}

	h.api.set(instanceID, "DECOMMISSIONED", "dep-3", nil, nil)
	if code := h.send(event("ev-4", "DECOMMISSION", "dep-4", nil)); code != http.StatusNoContent {
		t.Fatalf("status %d", code)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("params.json not deleted: %v", err)
	}
	if len(h.gh.allCommits()) != 0 {
		t.Fatalf("commits = %d, want none", len(h.gh.allCommits()))
	}
}

func TestDirectoryKeepsFileOnDecommissionKeep(t *testing.T) {
	h, file := directoryHarness(t, "  on_decommission: keep\n")
	h.deploy("ev-1", "dep-1", "large", nil)
	h.api.set(instanceID, "DECOMMISSIONED", "dep-1", nil, nil)
	h.send(event("ev-2", "DECOMMISSION", "dep-2", nil))
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("params.json deleted: %v", err)
	}
}

func TestDirectoryStaysInside(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "out")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../outside/params.json", "link/params.json", "/etc/params.json"} {
		if _, err := writeLocal(dir, change{path: p, content: []byte("{}\n")}); err == nil {
			t.Errorf("%s: written, want an error", p)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote outside the directory: %v", entries)
	}
}

const mixedConfig = `
defaults:
  repo: acme/platform
mappings:
  - name: lab
    match:
      md-project: lab
    strategy: stdout
`

func TestMixedMappingsPushAndPrint(t *testing.T) {
	h := newHarness(t, mixedConfig, "")
	var out bytes.Buffer
	h.p.out = &out
	h.deploy("ev-1", "dep-1", "large", map[string]any{"md-project": "lab"})
	if len(h.gh.allCommits()) != 0 || !strings.Contains(out.String(), "write lab/prod/database/params.json\n") {
		t.Fatalf("commits %d, stdout =\n%s", len(h.gh.allCommits()), out.String())
	}
	out.Reset()
	h.deploy("ev-2", "dep-2", "large", nil)
	if _, ok := h.gh.file("acme/platform", "main", filePath); !ok || out.Len() != 0 {
		t.Fatalf("pushed %v, stdout =\n%s", ok, out.String())
	}
}

func TestGitHubTokenRequiredOnlyForGitHubStrategies(t *testing.T) {
	t.Setenv("MASSDRIVER_ORGANIZATION_ID", "acme")
	t.Setenv("MASSDRIVER_API_KEY", "md_test")
	t.Setenv("GITHUB_TOKEN", "")
	dir := t.TempDir()
	cases := []struct {
		name, body string
		wantToken  bool
	}{
		{"stdout", "defaults:\n  strategy: stdout\n", false},
		{"directory", "debug:\n  directory: " + dir + "\ndefaults:\n  strategy: directory\n", false},
		{"skipped push mapping", "defaults:\n  strategy: stdout\nmappings:\n  - {name: a, match: {x: y}, repo: a/b, strategy: push, skip: true}\n", false},
		{"push default", "defaults:\n  repo: a/b\n", true},
		{"pull request mapping", "defaults:\n  strategy: stdout\nmappings:\n  - {name: a, match: {x: y}, repo: a/b, strategy: pull_request}\n", true},
		{"stdout mapping over push", mixedConfig, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CONFIG_FILE", p)
			_, err := Definition.New()
			if tc.wantToken {
				if err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
					t.Fatalf("err = %v, want missing GITHUB_TOKEN", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDebugStrategyConfig(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"directory needs debug.directory", "defaults:\n  strategy: directory\n", "debug.directory is required by the directory strategy"},
		{"bad repo still checked", "defaults:\n  strategy: stdout\n  repo: nope\n", "defaults.repo must be owner/name"},
		{"push mapping needs repo", "defaults:\n  strategy: stdout\nmappings:\n  - {name: a, match: {x: y}, strategy: push}\n", "mappings[0].repo must be owner/name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			if err := automation.DecodeConfig([]byte(tc.body), &cfg); err != nil {
				t.Fatal(err)
			}
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	cfg := DefaultConfig()
	if err := automation.DecodeConfig([]byte("defaults:\n  strategy: directory\n  skip: true\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("skipped directory target: %v", err)
	}
}
