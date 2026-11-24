package pki

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

var rotationNameFragment = regexp.MustCompile(`-new\.(crt|key|pub)\b|\.pre-carotate\b`)

// Drift from pki's manifest is silent, so every reader outside pki asks pki for the name.
func TestNoPackageOutsidePKISpellsARotationFileName(t *testing.T) {
	// The bare staged base names ("ca-new") a pair reader names, from the manifest itself.
	stagedBases := map[string]bool{}
	for _, f := range TrustLayout() {
		if f.StagedAs != "" {
			stagedBases[strings.TrimSuffix(f.StagedAs, filepath.Ext(f.StagedAs))] = true
		}
	}
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	for _, dir := range []string{"cmd", "internal", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if p == filepath.Join(root, "internal", "pki") || d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				if rotationNameFragment.MatchString(s) || stagedBases[s] {
					t.Errorf("%s spells the rotation file name %q; ask pki for it", fset.Position(lit.Pos()), s)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestRotationNameAccessorsNameWhatTheRotationWrites(t *testing.T) {
	old := version.PKIDir
	t.Cleanup(func() { version.PKIDir = old })
	version.PKIDir = t.TempDir()
	if err := GenerateAll("cp1", "10.0.0.9", nil); err != nil {
		t.Fatal(err)
	}
	if err := RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{StagedRootCAName + ".crt", StagedRootCAName + ".key", StagedTrustName("front-proxy-ca.key")} {
		if _, err := os.Stat(filepath.Join(version.PKIDir, name)); err != nil {
			t.Errorf("trust-new staged no %s: %v", name, err)
		}
	}
	if err := PromoteCA(); err != nil {
		t.Fatal(err)
	}
	if err := PromoteSA(); err != nil {
		t.Fatal(err)
	}
	for _, live := range []string{"ca.crt", "ca.key", "front-proxy-ca.key", "sa.pub", "sa.key"} {
		if _, err := os.Stat(filepath.Join(version.PKIDir, PreRotationBackupName(live))); err != nil {
			t.Errorf("sign-new backed up %s under another name than PreRotationBackupName says: %v", live, err)
		}
	}
}
