// Package gatelist holds the pin guards that keep the repository's two gate
// lists from drifting apart. A gate is a check that runs on every pull request;
// the two lists that spell the gates out are the steps of the CI lint job
// (.github/workflows/ci.yml) and the recipes of the Makefile's lint target.
// Neither list is derived from the other and nothing else in the tree compares
// them, so a gate dropped from one, or a self-test registered in neither, would
// pass every check.
//
// The package is test-only on purpose. What it pins is not code but the shape
// of files that change with the very decisions under guard, so the guards live
// beside their readers (ciLintJobSteps, which parses the workflow, and
// makefileCommands, which reads a named Makefile recipe) in gatelist_test.go,
// unexported, and run where `go test ./...` runs: the Makefile's test target,
// on every pull request. That placement also keeps the readers single-copy — an
// exported duplicate here, for other guards over the same files, would itself
// be a second hand-maintained list, the thing this package exists to catch.
package gatelist
