package cluster

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const docsRoot = "../../docs/docs"

func TestDocsClusterConfigSamplesParse(t *testing.T) {
	samples := docsClusterConfigSamples(t)
	if len(samples) < 4 {
		t.Fatalf("found %d ClusterConfig samples in %s; the extractor does not match the docs", len(samples), docsRoot)
	}
	for _, s := range samples {
		t.Run(s.where, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(s.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(path); err != nil {
				t.Errorf("the documented sample does not parse:\n%s\n%v", s.body, err)
			}
		})
	}
}

type docSample struct{ where, body string }

var (
	fenceOpen   = regexp.MustCompile("^```ya?ml\\s*$")
	topLevelKey = regexp.MustCompile(`^([A-Za-z]+):`)
)

func docsClusterConfigSamples(t *testing.T) []docSample {
	t.Helper()
	var out []docSample
	err := filepath.WalkDir(docsRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		var buf []string
		in, start, line := false, 0, 0
		for sc.Scan() {
			line++
			text := sc.Text()
			switch {
			case !in && fenceOpen.MatchString(text):
				in, start, buf = true, line, nil
			case in && strings.HasPrefix(text, "```"):
				in = false
				if isClusterConfig(buf) {
					rel, _ := filepath.Rel(docsRoot, path)
					out = append(out, docSample{where: rel + ":" + strconv.Itoa(start), body: strings.Join(buf, "\n") + "\n"})
				}
			case in:
				buf = append(buf, text)
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatalf("walk %s: %v", docsRoot, err)
	}
	return out
}

func isClusterConfig(lines []string) bool {
	hasSpec, kind := false, ""
	for _, l := range lines {
		m := topLevelKey.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		switch m[1] {
		case "spec":
			hasSpec = true
		case "kind":
			kind = strings.TrimSpace(strings.TrimPrefix(l, "kind:"))
		}
	}
	return kind == "ClusterConfig" || (hasSpec && kind == "")
}
