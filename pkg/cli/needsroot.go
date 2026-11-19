package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// needsRoot turns a permission failure on the node's own files into its
// cause, which is that the command was not run as root, and ends with the
// command that works: sudo and the binary's full path, since sudo's PATH
// usually leaves out /usr/local/bin. The original error stays on the last
// line for whoever needs it.
func needsRoot(err error) error {
	if err == nil || !errors.Is(err, fs.ErrPermission) || os.Geteuid() == 0 {
		return err
	}
	return fmt.Errorf("this command needs root. Run it again with sudo and the full path:\n  %s\n(%w)", sudoCommand(os.Args[1:]), err)
}

// sudoCommand is the given arguments as a command line that runs this
// binary as root, by its own path.
func sudoCommand(args []string) string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = "stratum"
	}
	parts := []string{"sudo", quoteIfNeeded(exe)}
	for _, a := range args {
		parts = append(parts, quoteIfNeeded(a))
	}
	return strings.Join(parts, " ")
}

// quoteIfNeeded leaves a word that a shell reads as itself unquoted, so the
// printed command reads naturally, and quotes anything else.
func quoteIfNeeded(word string) string {
	if word != "" && strings.Trim(word, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=@,+%") == "" {
		return word
	}
	return shellQuote(word)
}
