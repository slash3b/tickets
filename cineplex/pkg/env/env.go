package env

import (
	"os"
	"strings"
)

func Get(name, fallback string) string {
	n := strings.TrimSpace(name)

	v, ok := os.LookupEnv(strings.ToUpper(n))
	if !ok {
		return fallback
	}

	return v
}
