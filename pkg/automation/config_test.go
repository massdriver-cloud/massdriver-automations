package automation

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testConfig struct {
	Channel string   `yaml:"channel"`
	Rules   []string `yaml:"rules"`
	Nested  struct {
		Key string `yaml:"key"`
	} `yaml:"nested"`
}

func (c *testConfig) Validate() error {
	if len(c.Rules) == 0 {
		return errors.New("rules: at least one rule is required")
	}
	return nil
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigReadsConfigFileAndKeepsDefaults(t *testing.T) {
	t.Setenv("CONFIG_FILE", writeConfig(t, "rules: [a, b]\n"))
	cfg := testConfig{Channel: "#default"}
	if err := LoadConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Channel != "#default" || strings.Join(cfg.Rules, ",") != "a,b" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestLoadConfigDefaultPath(t *testing.T) {
	t.Setenv("CONFIG_FILE", "")
	if ConfigPath() != "/etc/massdriver-automations/config.yaml" {
		t.Fatalf("ConfigPath() = %q", ConfigPath())
	}
}

func TestLoadConfigRefusesUnknownKeysWithPathAndKey(t *testing.T) {
	path := writeConfig(t, "rules: [a]\nnested:\n  key: x\n  extra: y\nchanel: '#typo'\n")
	err := LoadConfigFile(path, &testConfig{})
	var ce *ConfigError
	if !errors.As(err, &ce) || ce.Path != path {
		t.Fatalf("err = %v, want a ConfigError for %s", err, path)
	}
	want := path + ": line 4: field extra not found; line 5: field chanel not found"
	if err.Error() != want {
		t.Fatalf("err = %q\nwant  %q", err, want)
	}
}

func TestLoadConfigMissingFileRunsValidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")
	err := LoadConfigFile(path, &testConfig{})
	if err == nil || err.Error() != path+": file not found, and the config without it is not valid: rules: at least one rule is required" {
		t.Fatalf("err = %v", err)
	}

	type optional struct {
		Channel string `yaml:"channel"`
	}
	cfg := optional{Channel: "#default"}
	if err := LoadConfigFile(path, &cfg); err != nil || cfg.Channel != "#default" {
		t.Fatalf("cfg = %+v, err = %v", cfg, err)
	}
}

func TestLoadConfigEmptyFileIsZeroConfig(t *testing.T) {
	err := LoadConfigFile(writeConfig(t, "# nothing\n"), &testConfig{})
	if err == nil || !strings.HasSuffix(err.Error(), ": rules: at least one rule is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadConfigBadYAML(t *testing.T) {
	path := writeConfig(t, "rules: [a\n")
	if err := LoadConfigFile(path, &testConfig{}); err == nil || !strings.HasPrefix(err.Error(), path+": yaml: line ") {
		t.Fatalf("err = %v", err)
	}
}
