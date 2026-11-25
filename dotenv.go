package main

import (
	"bufio"
	"os"
	"strings"
)

// loadDotEnv reads KEY=VALUE lines from path into the process environment.
// Variables already set in the real environment win over the file, and a missing
// file is not an error. It returns how many variables were taken from the file.
func loadDotEnv(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := parseEnvLine(sc.Text())
		if !ok {
			continue
		}
		if _, set := os.LookupEnv(key); set {
			continue
		}
		if err := os.Setenv(key, val); err != nil {
			return n, err
		}
		n++
	}
	return n, sc.Err()
}

// parseEnvLine understands:
//
//	KEY=value
//	export KEY=value
//	KEY="quoted value"   KEY='quoted value'
//	KEY=value # trailing comment
//	# full line comment
func parseEnvLine(line string) (key, val string, ok bool) {
	line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "export "))

	k, v, found := strings.Cut(line, "=")
	k = strings.TrimSpace(k)
	if !found || k == "" {
		return "", "", false
	}

	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		v = v[1 : len(v)-1]
	} else if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return k, v, true
}
