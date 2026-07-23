package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/drone/envsubst"
	"github.com/go-playground/validator/v10"
	"github.com/goccy/go-yaml"
)

type Config struct {
	Server  ServerConfig  `yaml:"server"`
	DB      DBConfig      `yaml:"db"`
	Mirakc  MirakcConfig  `yaml:"mirakc"`
	Storage StorageConfig `yaml:"storage"`
	Ingest  IngestConfig  `yaml:"ingest"`
	Encode  EncodeConfig  `yaml:"encode"`
	Log     LogConfig     `yaml:"log"`
}

type ServerConfig struct {
	Listen       string   `yaml:"listen"`
	AllowedHosts []string `yaml:"allowed_hosts"`
}

type DBConfig struct {
	Host     string `yaml:"host"     validate:"required"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"     validate:"required"`
	Password string `yaml:"password" validate:"required"`
	Database string `yaml:"database" validate:"required"`
	SSLMode  string `yaml:"sslmode"`
}

func (c DBConfig) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		c.Host, c.Port, c.User, c.Password, c.Database, c.SSLMode,
	)
}

type MirakcConfig struct {
	URL string `yaml:"url" validate:"required"`
}

type StorageConfig struct {
	MediaDir   string `yaml:"media_dir"   validate:"required"`
	ScratchDir string `yaml:"scratch_dir"`
}

type IngestConfig struct {
	Concurrency int `yaml:"concurrency"`
}

type EncodeConfig struct {
	FFmpeg   string          `yaml:"ffmpeg"`
	FFprobe  string          `yaml:"ffprobe"`
	Profiles []EncodeProfile `yaml:"profiles"`
}

type EncodeProfile struct {
	Name string `yaml:"name"`
}

type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

func defaults() Config {
	return Config{
		Server: ServerConfig{
			Listen: ":40773",
		},
		DB: DBConfig{
			Port:    5432,
			SSLMode: "disable",
		},
		Storage: StorageConfig{
			ScratchDir: "/var/tmp/rokuban",
		},
		Ingest: IngestConfig{
			Concurrency: 2,
		},
		Encode: EncodeConfig{
			FFmpeg:  "ffmpeg",
			FFprobe: "ffprobe",
		},
		Log: LogConfig{
			Level:  "info",
			Format: "json",
		},
	}
}

var vld = validator.New(validator.WithRequiredStructEnabled())

// Load reads a config file, expands ${VAR} references using environment
// variables, and parses the result with strict mode enabled.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	return loadFromString(string(raw))
}

func loadFromString(raw string) (*Config, error) {
	expanded, err := envsubst.EvalEnv(raw)
	if err != nil {
		return nil, fmt.Errorf("expanding variables: %w", err)
	}

	// defaults() の戻り値にマージすることでデフォルト値を提供する
	cfg := defaults()
	// Strict: 未知キーで起動失敗させ、typo を早期検出する
	if err := yaml.UnmarshalWithOptions([]byte(expanded), &cfg, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if err := vld.Struct(&cfg); err != nil {
		var ve validator.ValidationErrors
		if errors.As(err, &ve) {
			return nil, &ValidationError{fieldErrors: ve}
		}
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return &cfg, nil
}

type ValidationError struct {
	fieldErrors validator.ValidationErrors
}

func (e *ValidationError) Error() string {
	msgs := make([]string, len(e.fieldErrors))
	for i, fe := range e.fieldErrors {
		msgs[i] = fmt.Sprintf("%s is required", yamlFieldPath(fe))
	}
	return fmt.Sprintf("config validation failed:\n  - %s", strings.Join(msgs, "\n  - "))
}

func (e *ValidationError) FieldErrors() validator.ValidationErrors {
	return e.fieldErrors
}

func yamlFieldPath(fe validator.FieldError) string {
	ns := fe.Namespace()
	parts := strings.SplitN(ns, ".", 2)
	if len(parts) < 2 {
		return strings.ToLower(ns)
	}
	return strings.ToLower(parts[1])
}
