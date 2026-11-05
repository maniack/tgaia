package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	altsrc "github.com/urfave/cli-altsrc/v3"
	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"
)

// File is a per-command snapshot: all flags see the same validated file contents.
// altsrc's URI reader suppresses read/decode errors, so we validate locally and
// use its nested-key lookup with urfave's normal value-source precedence.
type File struct {
	Path   string
	loaded bool
	values map[any]any
	err    error
	app    *appSchema
}

type fileSchema struct {
	App      *appSchema `yaml:"app"`
	Database *struct {
		Driver *string `yaml:"driver"`
		DSN    *string `yaml:"dsn"`
	} `yaml:"database"`
	LogLevel       *string `yaml:"log_level"`
	ConnectTimeout *string `yaml:"connect_timeout"`
}

func (f *File) Load() error {
	if f.loaded {
		return f.err
	}
	f.loaded = true
	if f.Path == "" {
		return nil
	}
	data, err := os.ReadFile(f.Path)
	if err != nil {
		f.err = errors.New("cannot read config file")
		return f.err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var schema fileSchema
	if err := dec.Decode(&schema); err != nil {
		f.err = errors.New("invalid config file: expected YAML with known keys and valid types")
		return f.err
	}
	f.app = schema.App
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		f.err = errors.New("config file must contain exactly one YAML document")
		return f.err
	}
	// Decode the same bytes, not the path again, to prevent inconsistent sources.
	if err := yaml.Unmarshal(data, &f.values); err != nil {
		f.err = errors.New("invalid config mapping")
		return f.err
	}
	if f.values == nil || !stringValues(f.values) {
		f.err = errors.New("config file must contain a mapping with string values (no nulls)")
		return f.err
	}
	return nil
}

func (f *File) Value(key string) (string, bool) {
	if f.Load() != nil {
		return "", false
	}
	value, ok := altsrc.NestedVal(key, f.values)
	if !ok || value == nil {
		return "", false
	}
	return fmt.Sprint(value), true
}
func (f *File) Source(key, env string) cli.ValueSourceChain {
	return cli.NewValueSourceChain(cli.EnvVar(env), fileSource{file: f, key: key})
}

type fileSource struct {
	file *File
	key  string
}

func (s fileSource) Lookup() (string, bool) { return s.file.Value(s.key) }
func (s fileSource) String() string         { return "config key " + s.key }
func (s fileSource) GoString() string       { return s.String() }

func ValidateEnv(environ []string) error {
	allowed := map[string]bool{"TGAIA_CONFIG": true, "TGAIA_DATABASE_DRIVER": true, "TGAIA_DATABASE_DSN": true, "TGAIA_LOG_LEVEL": true, "TGAIA_CONNECT_TIMEOUT": true}
	for _, key := range strings.Fields("TGAIA_OTLP_ENDPOINT TGAIA_OBSERVABILITY_LISTEN TGAIA_AGENT_ID TGAIA_LISTEN TGAIA_PUBLIC_URL TGAIA_ROLE TGAIA_LLM_ENDPOINT TGAIA_LLM_MODEL TGAIA_LLM_API_KEY TGAIA_LLM_MOCK TGAIA_LLM_THINKING TGAIA_OIDC_MODE TGAIA_OIDC_ISSUER TGAIA_OIDC_CLIENT_ID TGAIA_OIDC_CLIENT_SECRET TGAIA_SESSION_KEY TGAIA_ADMIN_GROUPS TGAIA_TELEGRAM_MODE TGAIA_TELEGRAM_ADMIN_TOOLS TGAIA_TELEGRAM_WEBHOOK_LISTEN TGAIA_MEMORY_EMBEDDING_MODEL TGAIA_MEMORY_WINDOW TGAIA_MEMORY_INTERVAL") {
		allowed[key] = true
	}
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "TGAIA_") && !allowed[key] && !serviceLinkEnv(key) {
			return errors.New("unknown TGAIA_ environment variable; check configuration spelling")
		}
	}
	return nil
}

// serviceLinkEnv recognizes Kubernetes service-link variables injected for
// Services named tgaia or tgaia-*: TGAIA_API_SERVICE_HOST, TGAIA_PORT,
// TGAIA_DB_PORT_5432_TCP_ADDR and so on. They are never configuration.
func serviceLinkEnv(key string) bool {
	return strings.Contains(key, "_SERVICE_") || key == "TGAIA_PORT" || strings.Contains(key, "_PORT_") || strings.HasSuffix(key, "_PORT")
}

func stringValues(values map[any]any) bool {
	for key, value := range values {
		if _, ok := key.(string); !ok {
			return false
		}
		switch value := value.(type) {
		case string:
		case map[string]any:
			nested := make(map[any]any, len(value))
			for key, item := range value {
				nested[key] = item
			}
			if !stringValues(nested) {
				return false
			}
		case map[any]any:
			if !stringValues(value) {
				return false
			}
		default:
			return false
		}
	}
	return true
}
