// Command commentcheck refuses every Go comment that is not the doc comment of an exported identifier.
//
// It walks the directories it is given and reports each stray comment as
// "file":line:column, with the reason it is refused. Allowed are the package
// doc, the docs of exported package level identifiers, the known //go:
// directives in their exact form outside function bodies, the Output comment
// that closes a runnable example in a test file, and every comment of a
// generated file that a //go:generate directive of its directory names as its
// output. A //nolint directive is refused wherever it stands, doc block or not.
//
// A symbolic link or other special file is not read. It is reported as a
// finding, so a checked tree cannot hide a file behind a link.
//
// Usage:
//
//	commentcheck <dir>...
//
// Exit codes:
//
//	0   no stray comment
//	1   at least one finding was listed on stdout
//	2   the command line is wrong
//	66  a directory or a file cannot be read or parsed
//	74  the report or the diagnostic cannot be written
package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type exit uint8

const (
	exitClean   exit = 0
	exitStray   exit = 1
	exitUsage   exit = 2
	exitNoInput exit = 66
	exitIOError exit = 74
)

const (
	refusedReason    = "comment refused (decision 1): only the docs of exported identifiers, the known //go: directives and example outputs are allowed"
	nolintReason     = "//nolint is refused (decision 4): fix the code, or exclude the lint by path and text in .golangci.yml"
	unreadableReason = "not checked (decision 1): only regular files and directories are read, so a symbolic link or special file is refused"
)

type finding struct {
	position token.Position
	reason   string
}

type parsedFile struct {
	path string
	file *ast.File
}

var (
	errNoDirectory = errors.New("give at least one directory to check")
	errNotGoSource = errors.New("not valid Go source")
)

func main() {
	os.Exit(int(check(os.Args[1:], os.Stdout, os.Stderr)))
}

func check(dirs []string, stdout, stderr io.Writer) exit {
	if len(dirs) == 0 {
		return report(stderr, errNoDirectory, exitUsage)
	}
	var strays []string
	for _, dir := range dirs {
		found, err := straysUnder(dir)
		if err != nil {
			return report(stderr, err, exitNoInput)
		}
		strays = append(strays, found...)
	}
	if len(strays) == 0 {
		return exitClean
	}
	if _, err := fmt.Fprintln(stdout, strings.Join(strays, "\n")); err != nil {
		return report(stderr, err, exitIOError)
	}
	return exitStray
}

func report(stderr io.Writer, err error, code exit) exit {
	if _, writeErr := fmt.Fprintf(stderr, "commentcheck: %v\n", err); writeErr != nil {
		return exitIOError
	}
	return code
}

func straysUnder(root string) ([]string, error) {
	files := token.NewFileSet()
	byDirectory := make(map[string][]parsedFile)
	var found []finding
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return cannotRead(path, walkErr)
		}
		switch {
		case entry.IsDir():
			return nil
		case !entry.Type().IsRegular():
			found = append(found, finding{position: token.Position{Filename: path}, reason: unreadableReason})
			return nil
		case filepath.Ext(path) != ".go":
			return nil
		}
		file, err := parseGoFile(files, path)
		if err != nil {
			return err
		}
		directory := filepath.Dir(path)
		byDirectory[directory] = append(byDirectory[directory], parsedFile{path: path, file: file})
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, parsed := range byDirectory {
		outputs := generatorOutputs(parsed)
		for _, candidate := range parsed {
			namedByAGenerator := slices.Contains(outputs, filepath.Base(candidate.path))
			found = append(found, strayComments(files, candidate.path, candidate.file, namedByAGenerator)...)
		}
	}
	slices.SortFunc(found, compareFindings)
	lines := make([]string, 0, len(found))
	for _, item := range found {
		lines = append(lines, location(item.position)+": "+item.reason)
	}
	return lines, nil
}

func parseGoFile(files *token.FileSet, path string) (*ast.File, error) {
	file, err := parser.ParseFile(files, path, nil, parser.ParseComments)
	if err == nil {
		return file, nil
	}
	var list scanner.ErrorList
	if errors.As(err, &list) && len(list) > 0 {
		return nil, fmt.Errorf("cannot parse %s: %w", location(list[0].Pos), errNotGoSource)
	}
	return nil, cannotRead(path, err)
}

func cannotRead(path string, err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		err = pathErr.Err
	}
	return fmt.Errorf("cannot read %q: %w", path, err)
}

func location(position token.Position) string {
	if position.Line == 0 {
		return fmt.Sprintf("%q", position.Filename)
	}
	return fmt.Sprintf("%q:%d:%d", position.Filename, position.Line, position.Column)
}

func compareFindings(first, second finding) int {
	if order := strings.Compare(first.position.Filename, second.position.Filename); order != 0 {
		return order
	}
	if first.position.Line != second.position.Line {
		return first.position.Line - second.position.Line
	}
	return first.position.Column - second.position.Column
}
