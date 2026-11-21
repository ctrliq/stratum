package assets

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// As containerd names them on import.
func ImageNames(archives []string) (map[string]bool, error) {
	names := map[string]bool{}
	for _, archive := range archives {
		got, err := ArchiveImageNames(archive)
		if err != nil {
			return nil, fmt.Errorf("read the image names in %s: %w", archive, err)
		}
		for _, n := range got {
			names[n] = true
		}
	}
	return names, nil
}

// Reads only index.json, or a Docker archive's manifest.json RepoTags.
func ArchiveImageNames(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var repoTags []string
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return repoTags, nil
		}
		if err != nil {
			return nil, err
		}
		switch strings.TrimPrefix(h.Name, "./") {
		case "index.json":
			var index struct {
				Manifests []struct {
					Annotations map[string]string `json:"annotations"`
				} `json:"manifests"`
			}
			if err := json.NewDecoder(io.LimitReader(tr, 16<<20)).Decode(&index); err != nil {
				return nil, fmt.Errorf("index.json: %w", err)
			}
			var names []string
			for _, m := range index.Manifests {
				if n := m.Annotations["io.containerd.image.name"]; n != "" {
					names = append(names, n)
				}
			}
			return names, nil
		case "manifest.json":
			var manifest []struct {
				RepoTags []string `json:"RepoTags"`
			}
			if err := json.NewDecoder(io.LimitReader(tr, 16<<20)).Decode(&manifest); err != nil {
				return nil, fmt.Errorf("manifest.json: %w", err)
			}
			for _, m := range manifest {
				for _, t := range m.RepoTags {
					repoTags = append(repoTags, dockerImageName(t))
				}
			}
		}
	}
}

// Qualifies a Docker RepoTag the way containerd names it on import.
func dockerImageName(tag string) string {
	first, rest, ok := strings.Cut(tag, "/")
	switch {
	case !ok:
		return "docker.io/library/" + tag
	case first == "docker.io" && !strings.Contains(rest, "/"):
		return "docker.io/library/" + rest
	case strings.ContainsAny(first, ".:") || first == "localhost":
		return tag
	}
	return "docker.io/" + tag
}

// The form kubelet's preloadedImagesVerificationAllowlist matches.
func Repository(name string) string {
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name = name[:i]
	}
	if i := strings.LastIndexByte(name, ':'); i > strings.LastIndexByte(name, '/') {
		name = name[:i]
	}
	return name
}
