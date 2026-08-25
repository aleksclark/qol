package config

import (
	"fmt"
	"os"
	"strings"
)

// ReadSecret returns a trimmed secret from file or inline value.
// A non-empty file path wins so containers can inject secrets without
// putting the value in the process environment.
func ReadSecret(value, file string) (string, error) {
	if strings.TrimSpace(file) != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read secret file: %w", err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return strings.TrimSpace(value), nil
}
