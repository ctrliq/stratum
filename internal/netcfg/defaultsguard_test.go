package netcfg

import (
	"cmp"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
)

// defaultCallers are the files outside netcfg that may call netcfg.Default,
// each with the reason the bare default is right there. Default skips the
// value a user gave and the one a node recorded, so a new caller fails the
// guard until it is reviewed and listed here.
var defaultCallers = map[string]string{
	"internal/cluster/clusterdns.go":     "the DNS address the kubelet runs on when no service range or address is recorded, as runtime.ClusterDNSIP renders it",
	"internal/cluster/importerstacks.go": "a fallback for node state that records no service range; a controller records its own",
	"internal/cluster/worker.go":         "compares a worker's saved pod range with the default earlier releases saved for a join given none",
	"internal/operator/operator.go":      "--cluster-domain's default; stratum up always passes the node's domain",
	"internal/pki/pki.go":                "the kubernetes Service IP SAN when the caller passes none; a custom service range reaches the cert through APISANs",
	"internal/runtime/kubelet.go":        "the cluster DNS address of a worker joined without --service-cidr, which has no way to learn the cluster's",
	"internal/token/joinflags.go":        "compares the cluster's network with the default, so the join flags name only what differs",
}

// netcfgDefaultReads returns every reference in f to netcfg.Default, called or
// not, matched through the name f gives netcfg.
func netcfgDefaultReads(f *ast.File) []*ast.SelectorExpr {
	const netcfgPath = "github.com/ctrliq/stratum/internal/netcfg"
	name := ""
	for _, imp := range f.Imports {
		if importPath, err := strconv.Unquote(imp.Path.Value); err == nil && importPath == netcfgPath {
			name = "netcfg"
			if imp.Name != nil {
				name = imp.Name.Name
			}
		}
	}
	if name == "" {
		return nil
	}
	var reads []*ast.SelectorExpr
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == name && sel.Sel.Name == "Default" {
				reads = append(reads, sel)
			}
		}
		return true
	})
	return reads
}

// networkDefaultUses reports every way the file f, at file (slash-separated
// and relative to the module root), reads a network default without asking
// netcfg: an import of netdefault outside nodestate, a read of one of the
// names that left internal/version, a call of netcfg.Default from a file
// defaultCallers does not list, a string literal that spells a default value
// or a name under the default domain, and, in internal/version itself, a
// declaration of one of those names.
func networkDefaultUses(fset *token.FileSet, file string, f *ast.File) []string {
	const (
		versionPath    = "github.com/ctrliq/stratum/internal/version"
		netdefaultPath = "github.com/ctrliq/stratum/internal/netcfg/netdefault"
	)
	dir := path.Dir(file)
	defaultValues := map[string]bool{
		netdefault.PodCIDR: true, netdefault.ServiceCIDR: true, netdefault.APIServerSvcIP: true,
		netdefault.DNSClusterIP: true, netdefault.ClusterDomain: true,
	}
	var uses []string

	// The name this file gives internal/version, so a read is matched by the
	// package it names and never by a field that happens to share the name
	// (netc.PodCIDR).
	versionName := ""
	for _, imp := range f.Imports {
		importPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		switch importPath {
		case netdefaultPath:
			// nodestate may: netcfg imports nodestate, so NodeState.Domain
			// cannot ask netcfg for the default domain.
			if dir != "internal/nodestate" {
				uses = append(uses, fmt.Sprintf("%s: imports %s; ask netcfg (DomainOr, WithDefaults) instead", fset.Position(imp.Pos()), netdefaultPath))
			}
		case versionPath:
			versionName = "version"
			if imp.Name != nil {
				versionName = imp.Name.Name
			}
		}
	}

	if dir == "internal/version" {
		declares := func(name *ast.Ident) {
			if networkDefaultNames[name.Name] {
				uses = append(uses, fmt.Sprintf("%s: internal/version declares %s; the network defaults live in netcfg", fset.Position(name.Pos()), name.Name))
			}
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.ValueSpec:
						for _, name := range s.Names {
							declares(name)
						}
					case *ast.TypeSpec:
						declares(s.Name)
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil {
					declares(d.Name)
				}
			}
		}
	}

	if defaultCallers[file] == "" {
		for _, sel := range netcfgDefaultReads(f) {
			uses = append(uses, fmt.Sprintf("%s: calls netcfg.Default, which skips the value a user gave and the one the node recorded; "+
				"use DomainOr or WithDefaults, or list this file in defaultCallers with the reason the bare default is right", fset.Position(sel.Pos())))
		}
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if x, ok := n.X.(*ast.Ident); ok && versionName != "" && x.Name == versionName && networkDefaultNames[n.Sel.Name] {
				uses = append(uses, fmt.Sprintf("%s: reads internal/version's %s; ask netcfg (DomainOr, WithDefaults, NodeCIDRMask) instead", fset.Position(n.Pos()), n.Sel.Name))
			}
		case *ast.BasicLit:
			if n.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(n.Value)
			if err != nil {
				return true
			}
			if defaultValues[s] || strings.Contains(s, "."+netdefault.ClusterDomain) {
				uses = append(uses, fmt.Sprintf("%s: spells %q, a network default; ask netcfg for the cluster's value instead", fset.Position(n.Pos()), s))
			}
		}
		return true
	})
	return uses
}

// TestNetworkDefaultGuardFlagsABadFileAndPassesACleanOne: the walk below
// passes both for a clean tree and for a detector that matches nothing, so
// the detector is proven here on its own: each way of reading a default is
// flagged, and the look-alikes a real package holds are not.
func TestNetworkDefaultGuardFlagsABadFileAndPassesACleanOne(t *testing.T) {
	for _, tc := range []struct {
		name, dir, src string
		base           string   // the file's name in dir; "" is x.go
		want           []string // one substring per finding, in order
	}{
		{
			name: "a read through internal/version",
			dir:  "internal/cluster",
			src: `package cluster
import "github.com/ctrliq/stratum/internal/version"
var cidr = version.PodCIDR`,
			want: []string{"reads internal/version's PodCIDR"},
		},
		{
			name: "a read through a renamed internal/version import",
			dir:  "internal/cluster",
			src: `package cluster
import v "github.com/ctrliq/stratum/internal/version"
var domain = v.ClusterDomain`,
			want: []string{"reads internal/version's ClusterDomain"},
		},
		{
			name: "a fixed mask read through internal/version",
			dir:  "internal/staticpod",
			src: `package staticpod
import "github.com/ctrliq/stratum/internal/version"
var mask = version.NodeCIDRMaskV6`,
			want: []string{"reads internal/version's NodeCIDRMaskV6"},
		},
		{
			name: "netdefault imported outside nodestate",
			dir:  "internal/operator",
			src: `package operator
import "github.com/ctrliq/stratum/internal/netcfg/netdefault"
var domain = netdefault.ClusterDomain`,
			want: []string{"imports github.com/ctrliq/stratum/internal/netcfg/netdefault"},
		},
		{
			name: "a default value spelled out",
			dir:  "internal/runtime",
			src: `package runtime
const dns = "10.96.0.10"`,
			want: []string{`spells "10.96.0.10"`},
		},
		{
			name: "the default domain spelled out",
			dir:  "internal/pki",
			src: `package pki
var domain = "cluster.local"`,
			want: []string{`spells "cluster.local"`},
		},
		{
			name: "a name under the default domain",
			dir:  "internal/operator",
			src:  "package operator\nvar host = `webhook.stratum-system.svc.cluster.local`",
			want: []string{`spells "webhook.stratum-system.svc.cluster.local"`},
		},
		{
			name: "internal/version declaring a default",
			dir:  "internal/version",
			src: `package version
var NodeCIDRMask = 24`,
			want: []string{"internal/version declares NodeCIDRMask"},
		},
		{
			name: "internal/version declaring a default among the names it keeps",
			dir:  "internal/version",
			src: `package version
const (
	APIServerPort = 6443
	DNSClusterIP  = dnsIP
)
func ServiceCIDR() string { return "" }`,
			want: []string{"internal/version declares DNSClusterIP", "internal/version declares ServiceCIDR"},
		},
		{
			name: "netcfg.Default outside the files that may call it",
			dir:  "internal/addons",
			src: `package addons
import "github.com/ctrliq/stratum/internal/netcfg"
var domain = netcfg.Default().ClusterDomain`,
			want: []string{"calls netcfg.Default"},
		},
		{
			name: "netcfg.Default through a renamed import",
			dir:  "internal/addons",
			src: `package addons
import nc "github.com/ctrliq/stratum/internal/netcfg"
var cidr = nc.Default().ServiceCIDR`,
			want: []string{"calls netcfg.Default"},
		},
		{
			name: "netcfg.Default taken as a value",
			dir:  "internal/addons",
			src: `package addons
import "github.com/ctrliq/stratum/internal/netcfg"
var network = netcfg.Default`,
			want: []string{"calls netcfg.Default"},
		},
		{
			name: "netcfg.Default in another file of a package that may call it",
			dir:  "internal/token",
			src: `package token
import "github.com/ctrliq/stratum/internal/netcfg"
var def = netcfg.Default()`,
			want: []string{"calls netcfg.Default"},
		},
		{
			name: "netcfg.Default in a file that may call it",
			dir:  "internal/token",
			base: "joinflags.go",
			src: `package token
import "github.com/ctrliq/stratum/internal/netcfg"
var def = netcfg.Default()`,
		},
		{
			name: "a Default that is not netcfg's",
			dir:  "internal/addons",
			src: `package addons
import "github.com/ctrliq/stratum/internal/profile"
var p = profile.Default()`,
		},
		{
			name: "fields and netcfg's own names",
			dir:  "internal/cluster",
			src: `package cluster
import "github.com/ctrliq/stratum/internal/netcfg"
func ranges(c netcfg.Config) (string, string, int) {
	return c.PodCIDR, c.ClusterDomain, netcfg.NodeCIDRMask
}`,
		},
		{
			name: "a variable named version that is not the package",
			dir:  "internal/upgrade",
			src: `package upgrade
type release struct{ PodCIDR, ClusterDomain string }
func f() string {
	version := release{}
	return version.PodCIDR + version.ClusterDomain
}`,
		},
		{
			name: "internal/version's other names",
			dir:  "internal/cluster",
			src: `package cluster
import "github.com/ctrliq/stratum/internal/version"
var port, path = version.APIServerPort, version.NetConfigPath`,
		},
		{
			name: "netdefault in nodestate",
			dir:  "internal/nodestate",
			src: `package nodestate
import "github.com/ctrliq/stratum/internal/netcfg/netdefault"
var domain = netdefault.ClusterDomain`,
		},
		{
			name: "prose that names the default domain",
			dir:  "pkg/cli",
			src: `package cli
const usage = "the cluster's DNS domain, when not cluster.local"`,
		},
		{
			name: "another package declaring the names",
			dir:  "internal/token",
			src: `package token
type Grant struct{ PodCIDR, ServiceCIDR, ClusterDomain string }
const DNSClusterIP = "dns"`,
		},
		{
			name: "internal/version's own constants",
			dir:  "internal/version",
			src: `package version
const (
	APIServerPort = 6443
	EtcdName      = "stratum"
)`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := tc.dir + "/" + cmp.Or(tc.base, "x.go")
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, file, tc.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			got := networkDefaultUses(fset, file, f)
			if len(got) != len(tc.want) {
				t.Fatalf("networkDefaultUses = %q; want %d findings matching %q", got, len(tc.want), tc.want)
			}
			for i, want := range tc.want {
				if !strings.Contains(got[i], want) {
					t.Errorf("finding %d = %q; want it to contain %q", i, got[i], want)
				}
			}
		})
	}
}

// TestNoPackageOutsideNetcfgReadsANetworkDefault: netcfg decides a cluster's
// network. A range or domain the user gives wins, then what the node recorded,
// and only then the built-in default. A package that reads a default for
// itself skips that order, and a custom range or domain loses to the default
// without an error. So every package outside netcfg asks netcfg's resolver;
// only nodestate, which netcfg imports, reads netdefault itself, only the
// files defaultCallers lists, each for its stated reason, call netcfg.Default,
// and internal/version declares no network defaults. Test
// files are exempt: they pin the default values on purpose.
func TestNoPackageOutsideNetcfgReadsANetworkDefault(t *testing.T) {
	root := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("the module root is not two directories up: %v", err)
	}
	w, err := walkForNetworkDefaults(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, use := range w.uses {
		t.Error(use)
	}
	if len(w.checked) == 0 {
		t.Fatal("no non-test Go file found: the check would pass without checking anything")
	}
	if !slices.ContainsFunc(w.checked, func(file string) bool { return path.Dir(file) == "internal/version" }) {
		t.Fatal("no non-test Go file found in internal/version: a re-declared default would pass unseen")
	}
	for file := range defaultCallers {
		if !w.callers[file] {
			t.Errorf("%s is listed in defaultCallers but does not call netcfg.Default; remove it from the list", file)
		}
	}
}

// guardWalk is what walkForNetworkDefaults found: each way a file reads a
// network default, the files it checked, and the files in defaultCallers
// that call netcfg.Default. Paths are slash-separated and relative to the
// module root.
type guardWalk struct {
	uses    []string
	checked []string
	callers map[string]bool
}

// walkForNetworkDefaults checks every non-test Go file of the module rooted at
// root, under each top-level directory that holds Go code. Only netcfg's own
// files and netdefault's are exempt; a package below them is checked like any
// other. testdata, node_modules and nested modules are not this module's code.
func walkForNetworkDefaults(root string) (guardWalk, error) {
	exempt := map[string]bool{"internal/netcfg": true, "internal/netcfg/netdefault": true}
	w := guardWalk{callers: map[string]bool{}}
	fset := token.NewFileSet()
	for _, top := range []string{"cmd", "dash", "internal", "pkg", "tools"} {
		if _, err := os.Stat(filepath.Join(root, top)); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || d.Name() == "node_modules" {
					return filepath.SkipDir
				}
				// A directory with its own go.mod is another module.
				if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			file := filepath.ToSlash(rel)
			if exempt[path.Dir(file)] {
				return nil
			}
			f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			w.checked = append(w.checked, file)
			if defaultCallers[file] != "" && len(netcfgDefaultReads(f)) > 0 {
				w.callers[file] = true
			}
			w.uses = append(w.uses, networkDefaultUses(fset, file, f)...)
			return nil
		})
		if err != nil {
			return guardWalk{}, err
		}
	}
	return w, nil
}

// TestNetworkDefaultGuardWalksEveryPackageButNetcfgItself: the walk exempts
// netcfg's own files and netdefault's and nothing below or beside them, so a
// package added under internal/netcfg, or Go code outside internal, is
// checked, while vendored JavaScript dependencies and testdata are not.
func TestNetworkDefaultGuardWalksEveryPackageButNetcfgItself(t *testing.T) {
	root := t.TempDir()
	const bad = "package x\nimport \"github.com/ctrliq/stratum/internal/netcfg/netdefault\"\nvar d = netdefault.ClusterDomain\n"
	for file, src := range map[string]string{
		"go.mod":                    "module github.com/ctrliq/stratum\n",
		"internal/netcfg/netcfg.go": bad,
		"internal/netcfg/netdefault/netdefault.go": "package netdefault\nconst ClusterDomain = \"cluster.local\"\n",
		"internal/netcfg/sub/sub.go":               bad,
		"internal/version/version.go":              "package version\nconst APIServerPort = 6443\n",
		"dash/embed.go":                            "package dash\nconst domain = \"cluster.local\"\n",
		"dash/node_modules/m/m.go":                 bad,
		"internal/addons/testdata/x.go":            bad,
		"internal/addons/addons_test.go":           bad,
		"tools/other/go.mod":                       "module other\n",
		"tools/other/other.go":                     bad,
	} {
		p := filepath.Join(root, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	w, err := walkForNetworkDefaults(root)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(w.checked)
	if want := []string{"dash/embed.go", "internal/netcfg/sub/sub.go", "internal/version/version.go"}; !slices.Equal(w.checked, want) {
		t.Errorf("checked %q, want %q", w.checked, want)
	}
	for _, want := range []string{"internal/netcfg/sub/sub.go", "dash/embed.go"} {
		if !slices.ContainsFunc(w.uses, func(use string) bool { return strings.Contains(use, want) }) {
			t.Errorf("no finding for %s; findings: %q", want, w.uses)
		}
	}
	if len(w.uses) != 2 {
		t.Errorf("findings = %q, want one each for internal/netcfg/sub/sub.go and dash/embed.go", w.uses)
	}
}
