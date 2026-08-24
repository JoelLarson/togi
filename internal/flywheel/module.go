package flywheel

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const validationModuleLimit = 1 << 20

type validationModulePolicy struct {
	hasIgnore bool
}

// ValidateModuleConfinement rejects local module replacements that could make
// validation execute source outside the immutable validation root.
func ValidateModuleConfinement(root string) error {
	_, err := readValidationModulePolicy(root)
	return err
}

func readValidationModulePolicy(root string) (validationModulePolicy, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return validationModulePolicy{}, errors.New("validation module root is unsafe")
	}
	modulePath := filepath.Join(root, "go.mod")
	moduleInfo, err := os.Lstat(modulePath)
	if errors.Is(err, os.ErrNotExist) {
		return validationModulePolicy{}, nil
	}
	if err != nil || !moduleInfo.Mode().IsRegular() || moduleInfo.Mode()&os.ModeSymlink != 0 {
		return validationModulePolicy{}, errors.New("open validation module metadata")
	}
	file, err := os.Open(modulePath)
	if err != nil {
		return validationModulePolicy{}, errors.New("open validation module metadata")
	}
	defer file.Close()
	limited := io.LimitReader(file, validationModuleLimit+1)
	contents, err := io.ReadAll(limited)
	if err != nil || len(contents) > validationModuleLimit {
		return validationModulePolicy{}, errors.New("validation module metadata exceeds limits")
	}
	policy := validationModulePolicy{}
	block := ""
	lines := bufio.NewScanner(strings.NewReader(string(contents)))
	for lines.Scan() {
		tokens, err := moduleLineTokens(lines.Text())
		if err != nil {
			return validationModulePolicy{}, errors.New("validation module metadata is malformed")
		}
		if len(tokens) == 0 {
			continue
		}
		if block != "" {
			if len(tokens) == 1 && tokens[0] == ")" {
				block = ""
				continue
			}
			if err := applyValidationModuleDirective(root, block, tokens, &policy); err != nil {
				return validationModulePolicy{}, err
			}
			continue
		}
		if len(tokens) == 2 && tokens[1] == "(" && (tokens[0] == "replace" || tokens[0] == "ignore") {
			block = tokens[0]
			continue
		}
		if tokens[0] == "replace" || tokens[0] == "ignore" {
			if err := applyValidationModuleDirective(root, tokens[0], tokens[1:], &policy); err != nil {
				return validationModulePolicy{}, err
			}
		}
	}
	if lines.Err() != nil || block != "" {
		return validationModulePolicy{}, errors.New("validation module metadata is malformed")
	}
	return policy, nil
}

func moduleLineTokens(line string) ([]string, error) {
	var tokens []string
	for index := 0; index < len(line); {
		for index < len(line) && strings.ContainsRune(" \t\r", rune(line[index])) {
			index++
		}
		if index == len(line) || strings.HasPrefix(line[index:], "//") {
			break
		}
		start := index
		if line[index] == '`' || line[index] == '"' {
			quote := line[index]
			index++
			closed := false
			for index < len(line) {
				if quote == '"' && line[index] == '\\' {
					index += 2
					continue
				}
				if index < len(line) && line[index] == quote {
					index++
					closed = true
					break
				}
				index++
			}
			if !closed {
				return nil, errors.New("unterminated quoted module token")
			}
			value, err := strconv.Unquote(line[start:index])
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, value)
			if index < len(line) && !strings.ContainsRune(" \t\r", rune(line[index])) {
				return nil, errors.New("invalid quoted module token")
			}
			continue
		}
		for index < len(line) && !strings.ContainsRune(" \t\r", rune(line[index])) && !strings.HasPrefix(line[index:], "//") {
			index++
		}
		if start != index {
			tokens = append(tokens, line[start:index])
		}
		if strings.HasPrefix(line[index:], "//") {
			break
		}
	}
	return tokens, nil
}

func applyValidationModuleDirective(root, directive string, tokens []string, policy *validationModulePolicy) error {
	if directive == "ignore" {
		if len(tokens) != 1 || tokens[0] == "" {
			return errors.New("validation module ignore directive is malformed")
		}
		policy.hasIgnore = true
		return nil
	}
	arrow := -1
	for index, token := range tokens {
		if token == "=>" {
			arrow = index
		}
	}
	if arrow < 1 || arrow+2 > len(tokens) || len(tokens)-arrow-1 > 2 {
		return errors.New("validation module replacement is malformed")
	}
	if len(tokens)-arrow-1 == 2 {
		return nil
	}
	target := tokens[arrow+1]
	if filepath.IsAbs(target) || windowsChangedPath(target) || strings.Contains(target, `\`) {
		return errors.New("validation module replacement escapes the snapshot")
	}
	if !filepath.IsAbs(target) && !strings.HasPrefix(target, ".") {
		return nil
	}
	resolved := filepath.Clean(filepath.Join(root, filepath.FromSlash(target)))
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("validation module replacement escapes the snapshot")
	}
	linked, err := changedPathHasSymlink(root, filepath.ToSlash(relative))
	if err != nil || linked {
		return errors.New("validation module replacement is not confined")
	}
	return nil
}
