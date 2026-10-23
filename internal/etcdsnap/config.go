package etcdsnap

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/objstore"
	"github.com/ctrliq/stratum/internal/version"
)

// ConfigPath holds the scheduled-snapshot config (retention + optional S3 target)
// read by the `stratum snapshot save --scheduled` oneshot the timer runs. 0600
// because it may carry S3 credentials.
const ConfigPath = "/etc/stratum/snapshot.conf"

// Config is the scheduled-snapshot configuration.
//
// It deliberately distinguishes the config as EFFECTIVE (what to use when
// talking to S3, environment included) from the config as PERSISTED (what
// belongs in the on-disk conf). Credentials are the only field where those two
// differ, and conflating them is how env-sourced secrets ended up on disk.
type Config struct {
	Retain int
	// Dir is the local directory snapshots are written to and listed from.
	// snapshot.conf carries no directory; the resolved settings get it from
	// the cluster config (spec.snapshots.dir, else the built-in default), so
	// every caller that reads the settings acts in the directory the
	// scheduled loop owns instead of picking one of its own.
	Dir string
	// S3 is the effective target: the file's values with the environment
	// overlaid. Use it to reach the bucket; do not assume it mirrors disk.
	S3 objstore.Target

	// persisted{Access,Secret}Key are the credentials exactly as they appeared
	// in the on-disk conf, captured before the environment is overlaid.
	// SaveConfig writes these and never S3.AccessKey/S3.SecretKey.
	//
	// Unexported because stratum has no path that legitimately puts a NEW
	// credential into the file: there is no --s3-access-key flag, and
	// `snapshot schedule` takes credentials from the environment only. So the
	// only credentials ever written back are ones that were already there, and
	// a Config built anywhere else persists none at all.
	persistedAccessKey string
	persistedSecretKey string

	// fromFile is set when the conf file existed and was read, as opposed to
	// the missing-file fallback.
	fromFile bool
}

// FromFile reports whether this Config was read from the on-disk conf rather
// than being the defaults (or the caller's fallback) for a missing file.
func (c Config) FromFile() bool { return c.fromFile }

// HasPersistedCredentials reports whether the on-disk conf this Config was
// loaded from carries S3 credentials of its own.
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

// loadConfig reads ConfigPath (KEY=VALUE lines); a missing file yields defaults.
// S3 credentials may instead come from the environment (STRATUM_S3_* or the standard
// AWS_* vars) so they need not be written to disk.
//
// Unexported so a new caller cannot read the conf directly and bypass the
// resolver: the effective snapshot settings — retention, object store and
// directory — come from cluster.LoadSnapshotConfig, which resolves this file
// over the cluster config the controller was started with.
//
// LoadConfigOr reads ConfigPath, except that a missing file
// yields base (with the environment's S3 credentials overlaid) instead of the
// built-in defaults. An existing file still wins outright, so base never mixes
// into it. It is the read the resolver (cluster.LoadSnapshotConfig) is built on.
func LoadConfigOr(base Config) (Config, error) { return loadConfigFrom(ConfigPath, base) }

// loadConfigFrom is LoadConfig against an explicit path (ConfigPath is a fixed
// absolute path, so tests need a seam). missing is what a nonexistent path
// yields; the file is opened once, so a delete racing the load cannot swap it
// for the built-in defaults. An existing file always starts from DefaultConfig.
func loadConfigFrom(path string, missing Config) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			applyEnv(&missing)
			return missing, nil
		}
		return DefaultConfig(), err
	}
	c := DefaultConfig()
	c.fromFile = true
	defer f.Close()
	sc := bufio.NewScanner(f)
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

// applyEnv overlays S3 credentials from the environment (so creds can stay out of
// the on-disk conf). STRATUM_S3_* wins; the standard AWS_* vars fill any gaps.
// It touches only the effective c.S3 — never the persisted* fields, which is
// what keeps an env-sourced secret from being written back out.
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

// SaveConfig writes ConfigPath at 0600 (may hold credentials). Credentials are
// written from the config as PERSISTED, so an env-sourced secret that LoadConfig
// overlaid for use is never serialized to disk, and a credential the operator
// put in the file by hand is preserved verbatim. The writer leads, per the
// write-tier convention: it prints nothing today, and the pin holds it to that
// on whatever its caller hands it.
func SaveConfig(w io.Writer, c Config) error { return saveConfigTo(ConfigPath, c) }

// saveConfigTo is SaveConfig against an explicit path (see loadConfigFrom).
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
	// Preserve file credentials even when no bucket is configured. These are
	// NOT c.S3.{Access,Secret}Key: the effective values may be env-sourced.
	if c.persistedAccessKey != "" {
		fmt.Fprintf(&b, "S3_ACCESS_KEY=%s\n", c.persistedAccessKey)
	}
	if c.persistedSecretKey != "" {
		fmt.Fprintf(&b, "S3_SECRET_KEY=%s\n", c.persistedSecretKey)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Write atomically (temp file in the same directory, fsync, rename, dir
	// fsync): a crash mid-write must never leave a truncated conf — a parsed
	// truncated one silently reverts RETAIN and drops the S3 target, so the
	// next prune deletes backups. The rename also replaces the file wholesale,
	// so an existing conf with a wider mode ends 0600.
	return host.WriteFileAtomically(path, []byte(b.String()), 0o600)
}
