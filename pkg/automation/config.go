package automation

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultConfigFile is where an automation reads its config when CONFIG_FILE is not set.
const DefaultConfigFile = "/etc/massdriver-automations/config.yaml"

// ConfigPath returns the path of the config file: CONFIG_FILE, or DefaultConfigFile.
func ConfigPath() string { return Env("CONFIG_FILE", DefaultConfigFile) }

// ConfigError is a config file that cannot be read, decoded, or validated. The runner logs it as one
// "invalid config" record and exits.
type ConfigError struct {
	Path string
	Err  error
}

func (e *ConfigError) Error() string { return e.Path + ": " + e.Err.Error() }
func (e *ConfigError) Unwrap() error { return e.Err }

// Validator is a config that checks itself, and fills its defaults, after LoadConfig decodes it.
type Validator interface {
	Validate() error
}

// LoadConfig decodes the YAML file at ConfigPath into cfg, then calls cfg.Validate when cfg implements Validator.
//
// A key that cfg does not have is an error. A missing or empty file is not: cfg keeps the values it had, and
// Validate decides whether that is a valid config. Set defaults in cfg before the call.
func LoadConfig(cfg any) error {
	return LoadConfigFile(ConfigPath(), cfg)
}

// LoadConfigFile is LoadConfig for the file at path.
func LoadConfigFile(path string, cfg any) error {
	b, err := os.ReadFile(path)
	missing := errors.Is(err, fs.ErrNotExist)
	if err != nil && !missing {
		return &ConfigError{Path: path, Err: err}
	}
	if !missing {
		if err := DecodeConfig(b, cfg); err != nil {
			return &ConfigError{Path: path, Err: err}
		}
	}
	v, ok := cfg.(Validator)
	if !ok {
		return nil
	}
	if err := v.Validate(); err != nil {
		if missing {
			err = fmt.Errorf("file not found, and the config without it is not valid: %w", err)
		}
		return &ConfigError{Path: path, Err: err}
	}
	return nil
}

// DecodeConfig decodes YAML into cfg and refuses unknown keys. It does not call Validate.
func DecodeConfig(b []byte, cfg any) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return cleanYAMLError(err)
	}
	return nil
}

var (
	goTypeName = regexp.MustCompile(` in type .*$`)
	// yaml.v3 quotes the value that does not fit, and a config value must never reach a log.
	wrongType = regexp.MustCompile("cannot unmarshal !!(\\w+)( `[^`]*`)? into .*$")
)

// yaml.v3 puts each field error on its own line, quotes values, and names Go types. One line with only line numbers
// and keys is safe to log and means something to the person who wrote the file.
func cleanYAMLError(err error) error {
	msg := strings.TrimPrefix(err.Error(), "yaml: unmarshal errors:\n")
	lines := strings.Split(msg, "\n")
	for i := range lines {
		l := goTypeName.ReplaceAllString(strings.TrimSpace(lines[i]), "")
		lines[i] = wrongType.ReplaceAllString(l, "a $1 value is the wrong type here")
	}
	return errors.New(strings.Join(lines, "; "))
}
