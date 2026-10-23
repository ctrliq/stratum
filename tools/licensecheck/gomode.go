// gomode.go loads the packages a Go build links and decides each one's
// license.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	classifier "github.com/google/licenseclassifier/v2"
	"github.com/google/licenseclassifier/v2/assets"
)

// buildTarget is where and for which platforms the build is loaded.
type buildTarget struct {
	dir     string
	goos    string
	goarchs []string
	tags    string
}

// goModule and goPackage are the parts of `go list -json` output the check
// reads.
type goModule struct {
	Path    string
	Version string
	Main    bool
	Dir     string
	Replace *goModule
	Error   *struct{ Err string }
}

type goPackage struct {
	ImportPath string
	Dir        string
	Standard   bool
	Module     *goModule
	GoFiles    []string // with cgo off, cgo files are never compiled
	SFiles     []string
	HFiles     []string // headers, which assembly can #include
	SysoFiles  []string
	EmbedFiles []string
	Error      *goListError
	DepsErrors []*goListError
}

type goListError struct {
	ImportStack []string
	Err         string
}

// linkedPackage is one third-party package, merged across every target.
type linkedPackage struct {
	path   string
	dir    string
	module *goModule
	files  map[string]bool // compiled files, by absolute path
	// headerless says it ships files no SPDX header can cover: prebuilt
	// objects, or files embedded with //go:embed.
	headerless bool
}

// checkGo reports the license of every third-party package that patterns
// link on any of t's platforms.
func checkGo(t buildTarget, patterns []string, rep *reporter, stderr io.Writer) error {
	if info, err := os.Stat(t.dir); err != nil {
		return err
	} else if !info.IsDir() {
		return fmt.Errorf("-C %s is not a directory", t.dir)
	}
	pkgs, err := loadLinked(t, patterns, stderr)
	if err != nil {
		return err
	}
	var paths []string
	for path, p := range pkgs {
		// Only the main module is first-party: a module replaced by a
		// directory, even one inside this repository, is still someone
		// else's code linked into the binary.
		if !p.module.Main {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	c := &fileClassifier{cache: map[string]fileLicenses{}}
	for _, path := range paths {
		v, err := c.packageVerdict(pkgs[path])
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		rep.report(path, v)
	}
	var platforms []string
	for _, arch := range t.goarchs {
		platforms = append(platforms, t.goos+"/"+arch)
	}
	rep.summary(stderr, fmt.Sprintf("%d third-party packages linked for %s", len(paths), strings.Join(platforms, ", ")))
	return nil
}

// goCommand runs the go command in t.dir for GOARCH arch with cgo and
// workspaces off. The target environment is set on this subprocess only,
// overriding whatever the caller's environment says.
func goCommand(t buildTarget, arch string, args ...string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = t.dir
	// The release build uses no workspace. In one, every module the go.work
	// uses is a main module, which checkGo would skip unchecked, so
	// GOWORK=off also ignores a go.work found in a parent directory.
	cmd.Env = append(cmd.Environ(), "CGO_ENABLED=0", "GOOS="+t.goos, "GOARCH="+arch, "GOWORK=off")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		err = fmt.Errorf("go %s in %s for %s/%s: %w", strings.Join(args, " "), t.dir, t.goos, arch, err)
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w\n%s", err, msg)
		}
		return nil, err
	}
	return out, nil
}

// loadLinked lists every non-standard package the patterns link, for each
// target architecture. Any package that fails to load is an error: it would
// otherwise drop out of the check and pass silently.
func loadLinked(t buildTarget, patterns []string, stderr io.Writer) (map[string]*linkedPackage, error) {
	// -buildvcs=false: the check reads packages and license files, never VCS
	// metadata, and a stray VCS directory above t.dir (a junk .git in TMPDIR,
	// say) would otherwise fail every load.
	args := []string{"list", "-e", "-deps", "-json", "-buildvcs=false"}
	if t.tags != "" {
		args = append(args, "-tags="+t.tags)
	}
	args = append(args, patterns...)

	linked := map[string]*linkedPackage{}
	loadErrs := loadErrors{}
	for _, arch := range t.goarchs {
		platform := t.goos + "/" + arch
		out, err := goCommand(t, arch, args...)
		if err != nil {
			return nil, err
		}
		dec := json.NewDecoder(bytes.NewReader(out))
		listed := 0
		for {
			var p goPackage
			if err := dec.Decode(&p); err == io.EOF {
				break
			} else if err != nil {
				return nil, fmt.Errorf("reading go list output for %s: %w", platform, err)
			}
			listed++
			if p.Error != nil {
				loadErrs.add(p.ImportPath, p.Error.Err, platform, true)
			}
			for _, e := range p.DepsErrors {
				subject := p.ImportPath
				if n := len(e.ImportStack); n > 0 {
					subject = e.ImportStack[n-1]
				}
				loadErrs.add(subject, e.Err, platform, false)
			}
			if p.Module != nil && p.Module.Error != nil {
				loadErrs.add("module "+p.Module.Path, p.Module.Error.Err, platform, true)
			}
			if p.Standard || p.Error != nil {
				continue
			}
			if p.Module == nil || p.Module.Dir == "" {
				return nil, fmt.Errorf("%s has no module directory for %s, so its license files cannot be found (vendored builds are not supported)", p.ImportPath, platform)
			}
			lp := linked[p.ImportPath]
			if lp == nil {
				lp = &linkedPackage{path: p.ImportPath, dir: p.Dir, module: p.Module, files: map[string]bool{}}
				linked[p.ImportPath] = lp
			}
			for _, name := range slices.Concat(p.GoFiles, p.SFiles, p.HFiles) {
				lp.files[filepath.Join(p.Dir, name)] = true
			}
			lp.headerless = lp.headerless || len(p.SysoFiles) > 0 || len(p.EmbedFiles) > 0
		}
		if listed == 0 {
			return nil, fmt.Errorf("%s matched no packages for %s", strings.Join(patterns, " "), platform)
		}
	}
	if len(loadErrs) > 0 {
		var msgs []string
		for text, e := range loadErrs {
			msgs = append(msgs, fmt.Sprintf("licensecheck: cannot load %s (%s): %s", e.subject, strings.Join(e.platforms, ", "), text))
		}
		sort.Strings(msgs)
		for _, msg := range msgs {
			fmt.Fprintln(stderr, msg)
		}
		return nil, fmt.Errorf("%d package load error(s), so the build cannot be checked", len(msgs))
	}
	return linked, nil
}

// loadErrors holds each distinct load error go list reports, keyed by its
// text: a package's own error also reaches every importer's DepsErrors.
type loadErrors map[string]*loadError

type loadError struct {
	subject   string   // the package or module the error is about
	platforms []string // the GOOS/GOARCH pairs it occurs on
}

// add records the error text about subject on platform. own says the error
// is subject's own, which names it better than an importer's DepsErrors
// entry does.
func (errs loadErrors) add(subject, text, platform string, own bool) {
	e := errs[text]
	if e == nil {
		e = &loadError{subject: subject}
		errs[text] = e
	} else if own {
		e.subject = subject
	}
	if !slices.Contains(e.platforms, platform) {
		e.platforms = append(e.platforms, platform)
	}
}

// within reports whether path is root or inside it.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// fileClassifier identifies license texts, loading the classifier's corpus
// on first use and caching each file's result, since many packages share
// their module's license files.
type fileClassifier struct {
	c     *classifier.Classifier
	cache map[string]fileLicenses
}

// fileLicenses is what the classifier finds in one license file.
type fileLicenses struct {
	ids       []string // the licenses it identifies
	resembles []string // each match too weak to identify a license, and any text no match covers, described
}

// packageVerdict decides p's license. When every compiled file has an SPDX
// header, the headers alone decide. Otherwise every valid header is still
// judged, and fails the package if its license fails, and the nearest license
// files decide for the files without one; an unreadable header warns.
func (fc *fileClassifier) packageVerdict(p *linkedPackage) (verdict, error) {
	e, text, all, bad, err := headerLicense(p.files)
	if err != nil {
		return verdict{}, err
	}
	// A prebuilt object or an embedded file carries no header, so it counts
	// as an unheaded file.
	if all && !p.headerless {
		return judge(p.path, e, "SPDX headers "+text), nil
	}
	// The license files cover only the files without a header; a header's
	// license still applies to its own file.
	var header verdict
	if e != nil {
		if header = judge(p.path, e, "SPDX headers on some files "+text); header.level == levelFail {
			return header, nil
		}
	}
	v, err := fc.fileVerdict(p)
	if err != nil || v.level == levelFail {
		return v, err
	}
	// A header that declares no license, or one the check cannot read, is an
	// unknown license for its file, whatever the license files say.
	if len(bad) > 0 {
		return verdict{levelWarn, fmt.Sprintf("unknown license (SPDX header %q on some files; %s)", bad, v.reason)}, nil
	}
	// A header that passes only by an exception still reports it.
	if header.level == levelNote && v.level == levelPass {
		return header, nil
	}
	return v, nil
}

// fileVerdict decides p's license from its nearest license files.
func (fc *fileClassifier) fileVerdict(p *linkedPackage) (verdict, error) {
	files, err := licenseFiles(p.dir, p.module.Dir)
	if err != nil {
		return verdict{}, err
	}
	if len(files) == 0 {
		return verdict{levelWarn, "no license found (no SPDX header on every compiled file, and no LICENSE, LICENCE or COPYING file up to the module root)"}, nil
	}
	var ids, parts []string
	unknown := false
	for _, file := range files {
		found, err := fc.classify(file)
		if err != nil {
			return verdict{}, err
		}
		rel, err := filepath.Rel(p.module.Dir, file)
		if err != nil {
			return verdict{}, err
		}
		rel = filepath.ToSlash(rel)
		if len(found.ids) == 0 && len(found.resembles) == 0 {
			unknown = true
			parts = append(parts, rel+" (unknown text)")
			continue
		}
		// Text that only resembles a license is unknown, whatever else the
		// file identifies.
		unknown = unknown || len(found.resembles) > 0
		parts = append(parts, fmt.Sprintf("%s (%s)", rel, strings.Join(slices.Concat(found.ids, found.resembles), ", ")))
		ids = appendNew(ids, found.ids...)
	}
	source := "license files " + strings.Join(parts, ", ")
	if len(ids) > 0 {
		args := make([]*expr, len(ids))
		for i, id := range ids {
			args[i] = &expr{id: id}
		}
		// Every license found applies: several license files are AND.
		if v := judge(p.path, and(args...), source); v.level == levelFail || !unknown {
			return v, nil
		}
	}
	return verdict{levelWarn, "unknown license (" + source + ")"}, nil
}

// classifierNames maps the classifier's license names that differ from the
// SPDX id to the SPDX id.
var classifierNames = map[string]string{"BSD-0-Clause": "0BSD"}

// minConfidence is the lowest classifier confidence at which a match
// identifies a license, by match type; other types, such as copyright lines,
// identify none. The classifier reports matches down to 0.8, which admits a
// different license built on a known one: a BSD-3-Clause text with a
// no-nuclear-use condition added scores 0.88. A weaker match only says what
// the text resembles, so its file is an unknown license, which warns. A full
// license text needs 0.95 (the lowest stratum links scores 0.958). A notice
// needs 0.9: the classifier's Apache-2.0 notice opens with a placeholder
// copyright line that no real notice carries, so every real one scores 0.93.
var minConfidence = map[string]float64{"License": 0.95, "Header": 0.9}

// classify returns the licenses whose full text appears in file, and each
// standard license notice outside those texts, such as Apache-2.0's
// "Licensed under the Apache License" header after an MIT text. A match below
// its minConfidence identifies no license; classify describes it instead, as
// in "resembles BSD-3-Clause (0.88)".
func (fc *fileClassifier) classify(file string) (fileLicenses, error) {
	if found, ok := fc.cache[file]; ok {
		return found, nil
	}
	if fc.c == nil {
		c, err := assets.DefaultClassifier()
		if err != nil {
			return fileLicenses{}, fmt.Errorf("loading the license classifier: %w", err)
		}
		fc.c = c
	}
	text, err := os.ReadFile(file)
	if err != nil {
		return fileLicenses{}, err
	}
	matches := fc.c.Match(text).Matches
	var found fileLicenses
	for _, m := range matches {
		floor, ok := minConfidence[m.MatchType]
		if !ok || m.MatchType == "Header" && inLicenseText(m, matches) {
			continue
		}
		id := m.Name
		if spdxID, ok := classifierNames[id]; ok {
			id = spdxID
		}
		if m.Confidence < floor {
			found.resembles = appendNew(found.resembles, fmt.Sprintf("resembles %s (%.2f)", id, m.Confidence))
			continue
		}
		found.ids = appendNew(found.ids, id)
	}
	// Text beyond the licenses found could add terms to them, such as a
	// no-commercial-use paragraph after an MIT text.
	if n := unmatchedLines(text, matches); n > 0 {
		found.resembles = append(found.resembles, fmt.Sprintf("%d line(s) match no license", n))
	}
	sort.Strings(found.ids)
	sort.Strings(found.resembles)
	fc.cache[file] = found
	return found, nil
}

// unmatchedLines counts the lines of text that lie outside every match, of
// any type or confidence, and say something beyond a license title or a
// copyright line: even a terse added term such as "No commercial use." counts.
func unmatchedLines(text []byte, matches classifier.Matches) int {
	n := 0
	for i, line := range strings.Split(string(text), "\n") {
		if !strings.ContainsFunc(line, unicode.IsLetter) || licenseTitle(line) || copyrightNotice(line) {
			continue
		}
		if !slices.ContainsFunc(matches, func(m *classifier.Match) bool {
			return m.StartLine <= i+1 && i+1 <= m.EndLine
		}) {
			n++
		}
	}
	return n
}

// copyrightLine matches a copyright notice that the classifier does not
// always cover: one sentence, optionally followed by "All rights reserved.",
// or that phrase alone. A second sentence, as in "Copyright (c) X. Commercial
// use prohibited.", does not match, since it could add terms.
var copyrightLine = regexp.MustCompile(`(?i)^\W*(?:(?:copyright\b|\(c\)|©)(?:[^.;]|[.;]\S)*[.;]?\s*)?(?:all rights reserved\.?)?\s*$`)

// noticeWords are the lowercase words a bare copyright notice may hold;
// holders' names and the rest are capitalised, numbers, or addresses.
var noticeWords = map[string]bool{
	"copyright": true, "c": true, "by": true, "and": true, "the": true, "of": true,
	"its": true, "authors": true, "contributors": true, "all": true, "rights": true, "reserved": true,
}

// copyrightNotice reports whether line is a bare copyright notice: it
// matches copyrightLine, and every all-lowercase word in it is in
// noticeWords, so "Copyright (c) X, for non-commercial use only" is not one.
// shortcut: a Title Case term ("For Non-Commercial Use Only") in the notice's
// one sentence still passes; telling it from a holder's name needs more than
// word shapes.
func copyrightNotice(line string) bool {
	if !copyrightLine.MatchString(line) {
		return false
	}
	for _, w := range strings.Fields(line) {
		w = strings.TrimFunc(w, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		if strings.ContainsAny(w, "@./") || w != strings.ToLower(w) || !strings.ContainsFunc(w, unicode.IsLetter) {
			continue // an address, a name, or a number
		}
		if !noticeWords[w] {
			return false
		}
	}
	return true
}

// titleWords are the words a license title, such as "The MIT License (MIT)" or
// "BSD 3-Clause License", is made of. The classifier matches no title.
var titleWords = map[string]bool{
	"the": true, "license": true, "licence": true, "mit": true, "expat": true,
	"bsd": true, "clause": true, "simplified": true, "new": true, "revised": true,
	"isc": true, "apache": true, "version": true,
}

// licenseTitle reports whether line holds only titleWords and numbers.
func licenseTitle(line string) bool {
	words := strings.FieldsFunc(strings.ToLower(line), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	return !slices.ContainsFunc(words, func(w string) bool {
		return !titleWords[w] && strings.ContainsFunc(w, unicode.IsLetter)
	})
}

// inLicenseText reports whether one of matches identifies a license text
// that spans every line of the notice n, which is then part of that text (as
// at the end of Apache-2.0's) rather than a license of its own. A text too
// weak to identify a license accounts for no notice in it.
func inLicenseText(n *classifier.Match, matches classifier.Matches) bool {
	for _, m := range matches {
		if m.MatchType == "License" && m.Confidence >= minConfidence["License"] &&
			m.StartLine <= n.StartLine && n.EndLine <= m.EndLine {
			return true
		}
	}
	return false
}

// licenseFiles returns the license files (see isLicenseFile) in the
// directory nearest to dir that has any, walking up to the module root and
// never above it.
func licenseFiles(dir, root string) ([]string, error) {
	if !within(root, dir) {
		return nil, fmt.Errorf("package directory %s is outside its module root %s", dir, root)
	}
	for {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		var files []string
		for _, e := range entries {
			if !isLicenseFile(e.Name()) {
				continue
			}
			path := filepath.Join(dir, e.Name())
			info, err := os.Stat(path)
			if err != nil {
				return nil, err
			}
			if info.Mode().IsRegular() {
				files = append(files, path)
			}
		}
		if len(files) > 0 {
			return files, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir || !within(root, parent) {
			return nil, nil
		}
		dir = parent
	}
}

// isLicenseFile reports whether name, in any case, is LICENSE, LICENCE or
// COPYING, alone or followed by ".", "-" or "_" and more (LICENSE.md,
// LICENSE-MIT, COPYING_v2). A Go source file such as copying.go is code, not
// a license.
func isLicenseFile(name string) bool {
	if strings.EqualFold(filepath.Ext(name), ".go") {
		return false
	}
	upper := strings.ToUpper(name)
	for _, stem := range []string{"LICENSE", "LICENCE", "COPYING"} {
		if rest, ok := strings.CutPrefix(upper, stem); ok && (rest == "" || strings.ContainsRune(".-_", rune(rest[0]))) {
			return true
		}
	}
	return false
}

// headerLicense returns the license that the SPDX headers of files declare,
// with its text, or nil when no file has a header that parses. all reports
// whether every file has one; otherwise the license files decide for the
// rest. bad lists each header that does not parse or declares no license
// (NONE, NOASSERTION). Different headers all apply, so they combine with AND.
func headerLicense(files map[string]bool) (e *expr, text string, all bool, bad []string, err error) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var exprs []*expr
	var texts []string
	all = len(files) > 0
	for _, name := range names {
		tags, err := spdxHeader(name)
		if err != nil {
			return nil, "", false, nil, err
		}
		all = all && len(tags) > 0
		// Each tag is parsed alone, so a bad one cannot hide a good one.
		for _, tag := range tags {
			if slices.Contains(texts, tag) {
				continue
			}
			e, err := parseExpr(tag)
			if err != nil || noLicense(tag) {
				all = false
				bad = appendNew(bad, tag)
				continue
			}
			exprs = append(exprs, e)
			texts = append(texts, tag)
		}
	}
	if len(exprs) == 0 {
		return nil, "", false, bad, nil
	}
	if len(texts) > 1 {
		for i, text := range texts {
			if exprs[i].op != "" {
				texts[i] = "(" + text + ")"
			}
		}
	}
	return and(exprs...), strings.Join(texts, " AND "), all, bad, nil
}

const spdxTag = "SPDX-License-Identifier:"

// spdxHeader returns the expression of each SPDX-License-Identifier line in
// the comments that open a source file, before its first code. Several tags
// all apply.
func spdxHeader(name string) ([]string, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// A long opening line must not hide a header, so a line longer than
	// this is an error rather than the end of the scan.
	sc.Buffer(nil, 16<<20)
	var tags []string
	inBlock := false
	for first := true; sc.Scan(); first = false {
		line := strings.TrimSpace(sc.Text())
		if first {
			line = strings.TrimPrefix(line, "\uFEFF") // a UTF-8 byte order mark
		}
		// Code after a block comment's "*/" ends the header on that line.
		for line != "" {
			var comment string
			switch {
			case inBlock:
				comment, line, inBlock = blockComment(line)
			case strings.HasPrefix(line, "//"):
				comment, line = line[2:], ""
			case strings.HasPrefix(line, "/*"):
				comment, line, inBlock = blockComment(line[2:])
			default:
				return tags, nil // code: the header has ended
			}
			if i := strings.Index(comment, spdxTag); i >= 0 {
				tags = appendNew(tags, strings.TrimSpace(comment[i+len(spdxTag):]))
			}
			line = strings.TrimSpace(line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	return tags, nil
}

// blockComment splits a line inside a /* */ comment into the comment text,
// up to its end, and what follows the end, and reports whether the comment
// continues on the next line.
func blockComment(line string) (comment, rest string, open bool) {
	if i := strings.Index(line, "*/"); i >= 0 {
		return line[:i], line[i+2:], false
	}
	return line, "", true
}
