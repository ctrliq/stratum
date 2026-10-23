package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	for name, contents := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "cli")
}

func wantLeak(t *testing.T, violations []string, name, path string) {
	t.Helper()
	want := "exported " + name + " references internal package \"" + path + "\""
	for _, v := range violations {
		if strings.Contains(v, want) {
			return
		}
	}
	t.Errorf("missed %s; got:\n%s", name, strings.Join(violations, "\n"))
}

// The fixture must compile: the check reads go/types and compiler export
// data, so a fixture that does not build fails before any leak is reported.
func TestInternalAPILeaks(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"go.mod": "module example.com/boundary\n\ngo 1.23.0\n",
		"internal/state/state.go": `package state
type Config struct { Path string }
type Kind int
const Current Kind = 1
func New() *Config { return &Config{} }
func Pair() (string, *Config) { return "ok", New() }
func Text() string { return "ok" }
`,
		"cli/cli.go": `package cli
import "example.com/boundary/internal/state"

func Direct(c *state.Config) {}
type Alias = state.Config
type Defined state.Config
var Explicit *state.Config
type Callback func(*state.Config)
func WithCallback(fn func(*state.Config)) {}
func CallbackResult() func() *state.Config { return state.New }
type Hooks struct { Callback func(*state.Config) }
type Nested interface { Register(func() *state.Config) }
func InlineInterface(x interface { Use(*state.Config) }) {}
type Parenthesized (func(*state.Config))

type Receiver struct{}
func (Receiver) Leaky() *state.Config { return nil }

// These have no explicit type on the variable/constant declaration.
var DefaultConfig = state.Config{}
var Constructed = state.New()
var NewConfig = state.New
var Safe, Second = state.Pair()
var Configs = map[string]*state.Config{}
const DefaultKind = state.Current

// Referencing an internal package in an initializer does not itself leak a type.
var PublicText = state.Text()
var private = state.Config{}
type privateWrapper struct { value state.Config; Next *privateWrapper }
var Wrapped = privateWrapper{}
`,
	})
	violations, err := checkDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Direct", "Alias", "Defined", "Explicit", "Callback", "WithCallback",
		"CallbackResult", "Hooks", "Nested", "InlineInterface", "Parenthesized",
		"Receiver", // its leaky method is reached via the receiver type, not by name
		"DefaultConfig", "Constructed", "NewConfig", "Second", "Configs", "DefaultKind",
	}
	for _, name := range want {
		t.Run(name, func(t *testing.T) {
			wantLeak(t, violations, name, "example.com/boundary/internal/state")
		})
	}
	if len(violations) != len(want) {
		t.Errorf("got %d violations, want %d (no false positives for safe or private exports):\n%s",
			len(violations), len(want), strings.Join(violations, "\n"))
	}
}

func TestPublicAPIIsAllowed(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"go.mod": "module example.com/boundary\n\ngo 1.23.0\n",
		"internal/state/state.go": `package state
type Config struct { Path string }
func Text() string { return "ok" }
`,
		"public/public.go": `package public
import "example.com/boundary/internal/state"
type Config struct { hidden state.Config }
func New() Config { return Config{} }
`,
		"cli/cli.go": `package cli
import (
	"errors"
	"example.com/boundary/internal/state"
	"example.com/boundary/public"
)
type rootConfig struct { hidden state.Config }
type Option func(*rootConfig)
func WithText(s string) Option { return func(c *rootConfig) { c.hidden.Path = s } }
var ErrNoCommand = errors.New("no command")
var Text = state.Text()
var Config = public.New()
`,
	})
	violations, err := checkDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("rejected a public API: %v", violations)
	}
}

// A directory name and its declared package name can differ.
func TestImportPackageNameMismatch(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"go.mod": "module example.com/boundary\n\ngo 1.23.0\n",
		"internal/weird/weird.go": `// Package renamed deliberately does not match its directory name "weird".
package renamed
type Secret struct{}
`,
		"cli/cli.go": `package cli
import "example.com/boundary/internal/weird"
func Get() *renamed.Secret { return nil }
`,
	})
	violations, err := checkDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	wantLeak(t, violations, "Get", "example.com/boundary/internal/weird")
}

// TestGenericConstraintLeaks covers three ways a generic declaration's
// type-parameter constraint can reference an internal type: an exported
// generic function, an exported generic type, and a union constraint term
// (~int | internal.Foo).
func TestGenericConstraintLeaks(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"go.mod": "module example.com/boundary\n\ngo 1.24.0\n",
		"internal/state/state.go": `package state
type Secret struct{}
type Constraint interface{ ~int | Secret }
`,
		"cli/cli.go": `package cli
import "example.com/boundary/internal/state"

func Make[T state.Constraint]() T { var zero T; return zero }

type Container[T state.Constraint] struct{ Items []T }

func MakeUnion[T ~int | state.Secret]() T { var zero T; return zero }
`,
	})
	violations, err := checkDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Make", "Container", "MakeUnion"} {
		t.Run(name, func(t *testing.T) {
			wantLeak(t, violations, name, "example.com/boundary/internal/state")
		})
	}
}

// TestIndirectionCannotHideALeak covers the ways a leak can hide behind
// indirection that a pure syntax match would miss: a local, unexported type
// alias with no internal-looking name in sight at the use site, and a
// public sibling package's own alias of an internal type (so the only
// "internal" mention is two hops away from the exported declaration that
// actually leaks it). go/types resolves both the same way it resolves
// everything else — through to the true underlying type — with no special
// case needed for either.
func TestIndirectionCannotHideALeak(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"go.mod": "module example.com/boundary\n\ngo 1.23.0\n",
		"internal/state/state.go": `package state
type Config struct{}
type Secret struct{}
`,
		"public/public.go": `package public
import "example.com/boundary/internal/state"
type Alias = state.Secret
`,
		"cli/cli.go": `package cli
import (
	"example.com/boundary/internal/state"
	"example.com/boundary/public"
)

type hidden = state.Config
func ViaLocalAlias() hidden { var h hidden; return h }

func ViaPublicAlias() *public.Alias { return nil }
`,
	})
	violations, err := checkDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	wantLeak(t, violations, "ViaLocalAlias", "example.com/boundary/internal/state")
	wantLeak(t, violations, "ViaPublicAlias", "example.com/boundary/internal/state")
}

// A dot import binds every exported name from the imported package
// directly into scope with no qualifier — go/types resolves the identifier
// to its real declaring package regardless, so an actual leak through one
// is still caught, and (unlike a blanket "any dot import of internal/* is
// suspicious" rule) a dot-imported internal package that's never actually
// used in an exported signature is correctly left alone.
func TestDotImports(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"go.mod": "module example.com/boundary\n\ngo 1.23.0\n",
		"internal/state/state.go": `package state
type Config struct{}
type Unused struct{}
`,
		"cli/cli.go": `package cli
import . "example.com/boundary/internal/state"
func Get() *Config { return nil }
`,
	})
	violations, err := checkDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	wantLeak(t, violations, "Get", "example.com/boundary/internal/state")
	if len(violations) != 1 {
		t.Errorf("got %d violations, want exactly 1 (Unused is never referenced): %v", len(violations), violations)
	}
}

func TestDotImportOfPublicPackageIsAllowed(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"go.mod": "module example.com/boundary\n\ngo 1.23.0\n",
		"public/public.go": `package public
type Config struct{}
`,
		"cli/cli.go": `package cli
import . "example.com/boundary/public"
func Get() *Config { return nil }
`,
	})
	violations, err := checkDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Errorf("flagged a dot import of a non-internal package: %v", violations)
	}
}

func TestTypeLoadingFailureIsAnError(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"go.mod":     "module example.com/boundary\n\ngo 1.23.0\n",
		"cli/cli.go": "package cli\nvar Config = missingConstructor()\n",
	})
	if _, err := checkDir(dir); err == nil {
		t.Fatal("reported a clean API when the package could not be built")
	}
}

// An import that can never resolve makes the whole package fail to build,
// which loadExportedTypes needs to succeed even to check the syntactically
// simplest declaration — same failure path as TestTypeLoadingFailureIsAnError,
// via a different kind of build break.
func TestUnresolvableImportIsAnError(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"go.mod":     "module example.com/boundary\n\ngo 1.23.0\n",
		"cli/cli.go": "package cli\n\nimport _ \"example.com/boundary/does/not/exist\"\n",
	})
	if _, err := checkDir(dir); err == nil {
		t.Fatal("reported success when an import path could not be resolved")
	}
}

func TestPublicWrappersCannotHideLeaks(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"go.mod":                  "module example.com/boundary\n\ngo 1.24.0\n",
		"internal/state/state.go": "package state\ntype Config struct{}\n",
		"public/public.go": `package public
import "example.com/boundary/internal/state"
type Field struct { Value state.Config }
type Method struct { Next *Method }
func (*Method) Value() *state.Config { return nil }
type Private struct { hidden state.Config; Next *Private }
type Sealed interface { hidden(state.Config) }
`,
		"cli/cli.go": `package cli
import "example.com/boundary/public"
func Field() public.Field { return public.Field{} }
func Method() *public.Method { return nil }
func Private() public.Private { return public.Private{} }
func Sealed() public.Sealed { return nil }
`,
	})
	violations, err := checkDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Field", "Method"} {
		wantLeak(t, violations, name, "example.com/boundary/internal/state")
	}
	if len(violations) != 2 {
		t.Fatalf("want two reachable leaks, without flagging private members: %v", violations)
	}
}

func TestDefinedTypeFallbackUsesFileImports(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"go.mod":                  "module example.com/boundary\n\ngo 1.24.0\n",
		"internal/weird/state.go": "package renamed\ntype Config struct{}\n",
		"public/public.go":        "package public\ntype Config struct{}\n",
		"cli/alias.go": `package cli
import s "example.com/boundary/internal/weird"
type Aliased s.Config
type Pointer *s.Config
`,
		"cli/unaliased.go": `package cli
import "example.com/boundary/internal/weird"
type Unaliased renamed.Config
type PointerDefault *renamed.Config
`,
		"cli/public.go": `package cli
import s "example.com/boundary/public"
type Allowed s.Config
`,
	})
	violations, err := checkDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Aliased", "Pointer", "Unaliased", "PointerDefault"} {
		wantLeak(t, violations, name, "example.com/boundary/internal/weird")
	}
	if len(violations) != 4 {
		t.Fatalf("want one diagnostic per declaration, with file-local aliases: %v", violations)
	}
}

func TestInactiveFilesAreExcluded(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"go.mod":                  "module example.com/boundary\n\ngo 1.24.0\n",
		"internal/state/state.go": "package state\ntype Config struct{}\n",
		"cli/cli.go": `package cli
import "example.com/boundary/internal/state"
func Leak() *state.Config { return nil }
type Public struct{}
`,
		"cli/inactive.go": `//go:build ignore

package cli
import "example.com/boundary/internal/state"
type Public state.Config
type Ignored state.Config
`,
		"cli/inactive_invalid.go": "//go:build ignore\n\nthis is not Go source\n",
	})
	violations, err := checkDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	wantLeak(t, violations, "Leak", "example.com/boundary/internal/state")
	if len(violations) != 1 {
		t.Fatalf("inactive source affected the package's diagnostics: %v", violations)
	}
}

// TestOnlyThisModulesInternalsAreFlagged is the scoping test, and it uses
// one fixture for both halves on purpose: a dependency's internal type and
// our own arrive at the exported API by the identical route, so the only
// thing that can distinguish them is the module scope.
func TestOnlyThisModulesInternalsAreFlagged(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"go.mod": `module example.com/boundary

go 1.24.0

require example.com/dep v0.0.0

replace example.com/dep => ./dep
`,
		"dep/go.mod": "module example.com/dep\n\ngo 1.24.0\n",
		"dep/internal/hidden/hidden.go": `package hidden
type Secret struct{ Path string }
`,
		// Exactly the shape TestPublicWrappersCannotHideLeaks proves is a
		// leak when the wrapper is ours: an exported field of an internal
		// type, reachable from an exported declaration.
		"dep/pub/pub.go": `package pub
import "example.com/dep/internal/hidden"
type Wrapper struct { Value hidden.Secret }
`,
		"internal/state/state.go": "package state\ntype Config struct{}\n",
		"ours/ours.go": `package ours
import "example.com/boundary/internal/state"
type Wrapper struct { Value state.Config }
`,
		"cli/cli.go": `package cli
import (
	"example.com/boundary/ours"
	"example.com/dep/pub"
)

// Reaches example.com/dep/internal/hidden — another module's business.
func FromDependency() pub.Wrapper { return pub.Wrapper{} }

// Reaches example.com/boundary/internal/state — ours, and a real leak.
func FromThisModule() ours.Wrapper { return ours.Wrapper{} }
`,
	})
	violations, err := checkDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	wantLeak(t, violations, "FromThisModule", "example.com/boundary/internal/state")
	if len(violations) != 1 {
		t.Fatalf("want only this module's internal type flagged, got %d: %v", len(violations), violations)
	}
}

// TestNestedModuleInternalsAreNotOurs is the case a path prefix cannot
// decide.
func TestNestedModuleInternalsAreNotOurs(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"go.mod": `module example.com/boundary

go 1.24.0

require example.com/boundary/sub v0.0.0

replace example.com/boundary/sub => ./sub
`,
		"sub/go.mod": "module example.com/boundary/sub\n\ngo 1.24.0\n",
		"sub/internal/hidden/hidden.go": `package hidden
type Secret struct{ Path string }
`,
		"sub/pub/pub.go": `package pub
import "example.com/boundary/sub/internal/hidden"
type Wrapper struct { Value hidden.Secret }
`,
		"internal/state/state.go": "package state\ntype Config struct{}\n",
		"ours/ours.go": `package ours
import "example.com/boundary/internal/state"
type Wrapper struct { Value state.Config }
`,
		"cli/cli.go": `package cli
import (
	"example.com/boundary/ours"
	"example.com/boundary/sub/pub"
)

// Reaches example.com/boundary/sub/internal/hidden. Under our import path,
// but owned by the nested module — not ours.
func FromNestedModule() pub.Wrapper { return pub.Wrapper{} }

// Reaches example.com/boundary/internal/state — ours, and a real leak.
func FromThisModule() ours.Wrapper { return ours.Wrapper{} }
`,
	})
	violations, err := checkDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	wantLeak(t, violations, "FromThisModule", "example.com/boundary/internal/state")
	if len(violations) != 1 {
		t.Fatalf("want only this module's internal type flagged, got %d: %v", len(violations), violations)
	}
}

// TestModulePathMayContainInternal pins both directions for a module whose
// own path carries an internal segment, because the obvious simplification
// here — scan the whole import path instead of the part after the module
// path — gets one of them wrong.
func TestModulePathMayContainInternal(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"go.mod":                  "module example.com/internal/boundary\n\ngo 1.24.0\n",
		"internal/state/state.go": "package state\ntype Config struct{}\n",
		"pub/pub.go":              "package pub\ntype Config struct{}\n",
		"cli/cli.go": `package cli
import (
	"example.com/internal/boundary/internal/state"
	"example.com/internal/boundary/pub"
)

// Still caught: the suffix after the module path has its own internal segment.
func Leak() state.Config { return state.Config{} }

// Must NOT be caught: an ordinary public package that only looks internal
// because the module is called example.com/internal/boundary.
func NotALeak() pub.Config { return pub.Config{} }
`,
	})
	violations, err := checkDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	wantLeak(t, violations, "Leak", "example.com/internal/boundary/internal/state")
	if len(violations) != 1 {
		t.Fatalf("want only the real leak flagged, got %d: %v", len(violations), violations)
	}
}

// A package whose name merely contains "internal" is not an internal package
// — only a full path segment makes one.
func TestInternalMustBeAWholePathSegment(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"go.mod":                     "module example.com/boundary\n\ngo 1.24.0\n",
		"internalish/state/state.go": "package state\ntype Config struct{}\n",
		"svc/internals/internals.go": "package internals\ntype Config struct{}\n",
		"cli/cli.go": `package cli
import (
	"example.com/boundary/internalish/state"
	"example.com/boundary/svc/internals"
)
func A() *state.Config { return nil }
func B() *internals.Config { return nil }
`,
	})
	violations, err := checkDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("flagged a package that only looks internal: %v", violations)
	}
}
