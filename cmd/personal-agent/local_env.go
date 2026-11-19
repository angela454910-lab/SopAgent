package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// loadLocalEnv reads configuration as data. It never executes shell syntax,
// expands variables, logs values or overrides an already exported variable.
// A missing default file is optional; an explicitly requested file is required.
func loadLocalEnv(path string, required bool) error {
	if path == "" {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !required {
			return nil
		}
		return fmt.Errorf("read environment file: %w", err)
	}
	defer file.Close()
	values, err := parseLocalEnv(file)
	if err != nil {
		return err
	}
	for key, value := range values {
		if _, exported := os.LookupEnv(key); exported {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return errors.New("cannot set local environment variable")
		}
	}
	return nil
}

func parseLocalEnv(reader io.Reader) (map[string]string, error) {
	values := map[string]string{}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 1024), 1<<20)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		if lineNumber == 1 {
			line = strings.TrimPrefix(line, "\ufeff")
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") || strings.HasPrefix(line, "export\t") {
			line = strings.TrimSpace(line[len("export"):])
		}
		key, raw, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found || !validLocalEnvKey(key) {
			return nil, fmt.Errorf("environment file line %d: expected KEY=value", lineNumber)
		}
		value, err := parseLocalEnvValue(strings.TrimSpace(raw))
		if err != nil {
			// Never include the source line or parser input in an error: it may
			// contain a key, URL password or another deployment credential.
			return nil, fmt.Errorf("environment file line %d: %s", lineNumber, err)
		}
		values[key] = value
	}
	if scanner.Err() != nil {
		return nil, errors.New("cannot read environment file or line exceeds 1 MiB")
	}
	return values, nil
}

func validLocalEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i := range len(key) {
		c := key[i]
		if c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}

func parseLocalEnvValue(raw string) (string, error) {
	if strings.ContainsRune(raw, '\x00') {
		return "", errors.New("NUL bytes are not allowed")
	}
	if raw == "" {
		return "", nil
	}
	quote := raw[0]
	if quote != '\'' && quote != '"' {
		for i := range len(raw) {
			if raw[i] == '#' && (i == 0 || raw[i-1] == ' ' || raw[i-1] == '\t') {
				return strings.TrimSpace(raw[:i]), nil
			}
		}
		return raw, nil
	}
	var value strings.Builder
	for i := 1; i < len(raw); i++ {
		c := raw[i]
		if c == quote {
			tail := strings.TrimSpace(raw[i+1:])
			if tail != "" && !strings.HasPrefix(tail, "#") {
				return "", errors.New("unexpected content after quoted value")
			}
			return value.String(), nil
		}
		if quote == '"' && c == '\\' && i+1 < len(raw) {
			i++
			switch escaped := raw[i]; escaped {
			case 'n':
				value.WriteByte('\n')
			case 'r':
				value.WriteByte('\r')
			case 't':
				value.WriteByte('\t')
			case '\\', '"':
				value.WriteByte(escaped)
			default:
				value.WriteByte('\\')
				value.WriteByte(escaped)
			}
			continue
		}
		value.WriteByte(c)
	}
	return "", errors.New("unterminated quoted value")
}
