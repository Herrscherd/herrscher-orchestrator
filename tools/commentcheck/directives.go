package main

import (
	"go/ast"
	"go/build/constraint"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

const toolDirective = "tool"

type directive struct {
	name      string
	arguments []string
}

func generatorOutputs(parsed []parsedFile) []string {
	var outputs []string
	for _, candidate := range parsed {
		if ast.IsGenerated(candidate.file) {
			continue
		}
		directory := filepath.Dir(candidate.path)
		for _, group := range candidate.file.Comments {
			for _, line := range group.List {
				if found := splitDirective(line.Text); found.name == "generate" && isKnownDirective(directory, line.Text) {
					outputs = append(outputs, outputFlagValues(found.arguments)...)
				}
			}
		}
	}
	return outputs
}

func outputFlagValues(arguments []string) []string {
	var outputs []string
	for index, argument := range arguments {
		flagName, value, hasValue := strings.Cut(argument, "=")
		if flagName != "-output" && flagName != "-o" {
			continue
		}
		if hasValue {
			outputs = append(outputs, value)
		} else if index+1 < len(arguments) {
			outputs = append(outputs, arguments[index+1])
		}
	}
	return outputs
}

func splitDirective(text string) directive {
	line, isGoDirective := strings.CutPrefix(text, "//go:")
	nameEnd := strings.IndexFunc(line, unicode.IsSpace)
	if nameEnd < 0 {
		nameEnd = len(line)
	}
	arguments, parsed := directiveWords(line[nameEnd:])
	if !isGoDirective || nameEnd == 0 || !parsed {
		return directive{}
	}
	return directive{name: line[:nameEnd], arguments: arguments}
}

func isKnownDirective(directory, text string) bool {
	found := splitDirective(text)
	arguments := found.arguments
	switch found.name {
	case "noinline", "nosplit", "noescape", "norace", "uintptrescapes":
		return len(arguments) == 0
	case "build":
		_, err := constraint.Parse(text)
		return err == nil
	case "generate":
		return isPinnedGenerator(directory, arguments)
	case "embed":
		return len(arguments) > 0 && !slices.ContainsFunc(arguments, func(pattern string) bool {
			return !matchesAFile(directory, pattern)
		})
	case "fix":
		return slices.Equal(arguments, []string{"inline"})
	case "debug":
		return len(arguments) == 1 && isSetting(arguments[0])
	}
	return false
}

func directiveWords(arguments string) ([]string, bool) {
	var patterns []string
	for rest := strings.TrimSpace(arguments); rest != ""; rest = strings.TrimSpace(rest) {
		end := strings.IndexFunc(rest, unicode.IsSpace)
		if quote := rest[0]; quote == '"' || quote == '`' {
			end = closingQuote(rest) + 1
		}
		if end <= 0 {
			end = len(rest)
		}
		pattern, err := unquotePattern(rest[:end])
		if err != nil {
			return nil, false
		}
		patterns = append(patterns, pattern)
		rest = rest[end:]
	}
	return patterns, true
}

func closingQuote(quoted string) int {
	for index := 1; index < len(quoted); index++ {
		switch {
		case quoted[index] == '\\' && quoted[0] == '"':
			index++
		case quoted[index] == quoted[0]:
			return index
		}
	}
	return -1
}

func unquotePattern(word string) (string, error) {
	if word[0] != '"' && word[0] != '`' {
		return word, nil
	}
	return strconv.Unquote(word)
}

func isPinnedGenerator(directory string, arguments []string) bool {
	if len(arguments) < 3 || arguments[0] != "go" || !isFlagList(arguments[3:]) {
		return false
	}
	switch command := arguments[2]; arguments[1] {
	case "run":
		return isVersionedModule(command) || isLocalPackage(directory, command)
	case toolDirective:
		return slices.Contains(declaredTools(directory), command)
	}
	return false
}

func isVersionedModule(command string) bool {
	module, version, versioned := strings.Cut(command, "@")
	return versioned && strings.Contains(module, ".") && !strings.HasPrefix(module, ".") && strings.HasPrefix(version, "v")
}

func isLocalPackage(directory, command string) bool {
	if command != "." && !strings.HasPrefix(command, "./") && !strings.HasPrefix(command, "../") {
		return false
	}
	info, err := os.Stat(filepath.Join(directory, command))
	return err == nil && info.IsDir()
}

func isFlagList(arguments []string) bool {
	for index := 0; index < len(arguments); index++ {
		if !strings.HasPrefix(arguments[index], "-") {
			return false
		}
		takesNextValue := !strings.Contains(arguments[index], "=")
		if takesNextValue && index+1 < len(arguments) && !strings.HasPrefix(arguments[index+1], "-") {
			index++
		}
	}
	return true
}

func declaredTools(directory string) []string {
	root, found := moduleRoot(directory)
	if !found {
		return nil
	}
	manifest, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil
	}
	var tools []string
	inToolBlock := false
	for line := range strings.Lines(string(manifest)) {
		words := strings.Fields(line)
		switch {
		case len(words) == 0:
		case inToolBlock && words[0] == ")":
			inToolBlock = false
		case inToolBlock:
			tools = append(tools, words[0], filepath.Base(words[0]))
		case words[0] == toolDirective && len(words) == 2 && words[1] == "(":
			inToolBlock = true
		case words[0] == toolDirective && len(words) == 2:
			tools = append(tools, words[1], filepath.Base(words[1]))
		}
	}
	return tools
}

func moduleRoot(directory string) (string, bool) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return "", false
	}
	for current := absolute; ; current = filepath.Dir(current) {
		if _, err := os.Stat(filepath.Join(current, "go.mod")); err == nil {
			return current, true
		}
		if filepath.Dir(current) == current {
			return "", false
		}
	}
}

func matchesAFile(directory, pattern string) bool {
	matches, err := filepath.Glob(filepath.Join(directory, strings.TrimPrefix(pattern, "all:")))
	return err == nil && len(matches) > 0
}

func isSetting(argument string) bool {
	key, value, hasValue := strings.Cut(argument, "=")
	return hasValue && key != "" && value != "" && !strings.ContainsFunc(key, func(letter rune) bool {
		return !unicode.IsLower(letter) && !unicode.IsDigit(letter)
	})
}
