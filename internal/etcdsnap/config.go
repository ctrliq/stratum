package etcdsnap

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ctrliq/stratum/internal/boundedfile"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/objstore"
	"github.com/ctrliq/stratum/internal/version"
)

// ConfigPath holds the scheduled-snapshot config; 0600, as it may carry S3 credentials.
const ConfigPath = "/etc/stratum/snapshot.conf"

// Config separates effective S3 settings (env overlaid) from credentials as persisted on disk.
type Config struct {
	Retain int
	// Dir comes from the cluster config (snapshot.conf carries none); callers pick none of their own.
	Dir string
	// S3 is the effective target: the file's values with the environment overlaid.
	S3 objstore.Target

	// Credentials as read from disk, before env overlay; SaveConfig writes these, never S3.
	persistedAccessKey string
	persistedSecretKey string

	fromFile bool
}

// FromFile reports whether the on-disk conf was read; false means defaults or the caller's base.
func (c Config) FromFile() bool { return c.fromFile }

// HasPersistedCredentials reports whether the loaded conf carried S3 credentials of its own.
func (c Config) HasPersistedCredentials() bool {
	return c.persistedAccessKey != "" || c.persistedSecretKey != ""
}

// DefaultConfig is the built-in default: local-only, retain N.
func DefaultConfig() Config { return Config{Retain: version.SnapshotRetainDefault} }

// S3Target returns the configured S3 target, or nil if no bucket is set.
func (c Config) S3Target() *objstore.Target {
	if c.S3.Bucket == "" {
		return nil
	}
	t := c.S3
	return &t
}

// LoadConfigOr reads ConfigPath; a missing file yields base with env credentials overlaid.
func LoadConfigOr(base Config) (Config, error) { return loadConfigFrom(ConfigPath, base) }

// LoadConfigOrContext bounds the read while preserving conf-over-base precedence.
func LoadConfigOrContext(ctx context.Context, base Config) (Config, error) {
	return loadConfigFromContext(ctx, ConfigPath, base)
}

// loadConfigFrom is LoadConfigOr on an explicit path; one open, so a delete cannot yield defaults.
func loadConfigFrom(path string, missing Config) (Config, error) {
	return loadConfigFromContext(context.Background(), path, missing)
}

func loadConfigFromContext(ctx context.Context, path string, missing Config) (Config, error) {
	b, err := boundedfile.Read(ctx, path)
	if err != nil {
		if os.IsNotExist(err) {
			applyEnv(&missing)
			return missing, nil
		}
		return DefaultConfig(), err
	}
	c := DefaultConfig()
	c.fromFile = true
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"`)
		switch k {
		case "RETAIN":
			if n, err := strconv.Atoi(v); err == nil {
				c.Retain = n
			}
		case "S3_ENDPOINT":
			c.S3.Endpoint = v
		case "S3_REGION":
			c.S3.Region = v
		case "S3_BUCKET":
			c.S3.Bucket = v
		case "S3_PREFIX":
			c.S3.Prefix = v
		case "S3_ACCESS_KEY":
			c.S3.AccessKey, c.persistedAccessKey = v, v
		case "S3_SECRET_KEY":
			c.S3.SecretKey, c.persistedSecretKey = v, v
		case "S3_PATH_STYLE":
			c.S3.PathStyle = v == "true" || v == "1"
		}
	}
	applyEnv(&c)
	return c, sc.Err()
}

// applyEnv overlays S3 credentials: STRATUM_S3_* wins, AWS_* fills any gaps.
func applyEnv(c *Config) {
	if v := os.Getenv("STRATUM_S3_ACCESS_KEY"); v != "" {
		c.S3.AccessKey = v
	}
	if v := os.Getenv("STRATUM_S3_SECRET_KEY"); v != "" {
		c.S3.SecretKey = v
	}
	if c.S3.AccessKey == "" {
		c.S3.AccessKey = os.Getenv("AWS_ACCESS_KEY_ID")
	}
	if c.S3.SecretKey == "" {
		c.S3.SecretKey = os.Getenv("AWS_SECRET_ACCESS_KEY")
	}
}

// SaveConfig writes ConfigPath from persisted credentials; w follows the write tier and is quiet.
func SaveConfig(w io.Writer, c Config) error { return saveConfigTo(ConfigPath, c) }

// saveConfigTo is SaveConfig against an explicit path.
func saveConfigTo(path string, c Config) error {
	var b strings.Builder
	b.WriteString("# stratum scheduled etcd snapshot config (managed by `stratum snapshot schedule`)\n")
	fmt.Fprintf(&b, "RETAIN=%d\n", c.Retain)
	if c.S3.Bucket != "" {
		fmt.Fprintf(&b, "S3_BUCKET=%s\n", c.S3.Bucket)
		if c.S3.Endpoint != "" {
			fmt.Fprintf(&b, "S3_ENDPOINT=%s\n", c.S3.Endpoint)
		}
		if c.S3.Region != "" {
			fmt.Fprintf(&b, "S3_REGION=%s\n", c.S3.Region)
		}
		if c.S3.Prefix != "" {
			fmt.Fprintf(&b, "S3_PREFIX=%s\n", c.S3.Prefix)
		}
		if c.S3.PathStyle {
			b.WriteString("S3_PATH_STYLE=true\n")
		}
	}
	// The effective S3 values may be env-sourced; write only what disk held.
	if c.persistedAccessKey != "" {
		fmt.Fprintf(&b, "S3_ACCESS_KEY=%s\n", c.persistedAccessKey)
	}
	if c.persistedSecretKey != "" {
		fmt.Fprintf(&b, "S3_SECRET_KEY=%s\n", c.persistedSecretKey)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// A truncated conf silently reverts RETAIN and drops the S3 target; the rename also ends 0600.
	return host.WriteFileAtomically(path, []byte(b.String()), 0o600)
}
