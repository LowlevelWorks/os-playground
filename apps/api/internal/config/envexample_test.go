package config

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/joho/godotenv"
)

// The files live at the module root; tests run in this package's directory.
const (
	envExamplePath = "../../.env.example"
	envLocalPath   = "../../.env"
)

// settingNames returns the value of every Env* constant in config.go, so a
// setting added there cannot be forgotten in .env.example.
func settingNames(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "config.go", nil, 0)
	if err != nil {
		t.Fatalf("parse config.go: %v", err)
	}
	var names []string
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, ident := range vs.Names {
				if !strings.HasPrefix(ident.Name, "Env") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s is not a string literal", ident.Name)
				}
				name, _ := strconv.Unquote(lit.Value)
				names = append(names, name)
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("found no Env* constants in config.go")
	}
	sort.Strings(names)
	return names
}

// keysIn lists a dotenv file's keys in file order, repeats included, which
// godotenv.Read would hide.
func keysIn(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is envExamplePath or envLocalPath, both constants
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			t.Errorf("%s: %q is not KEY=value", path, line)
			continue
		}
		keys = append(keys, strings.TrimSpace(strings.TrimPrefix(key, "export ")))
	}
	return keys
}

// checkKeys reports keys config does not read (a typo is silently ignored at
// run time) and keys given twice. With complete, it also reports settings the
// file leaves out.
func checkKeys(t *testing.T, path string, complete bool) {
	t.Helper()
	known := make(map[string]bool)
	for _, n := range settingNames(t) {
		known[n] = true
	}
	seen := make(map[string]bool)
	for _, k := range keysIn(t, path) {
		if !known[k] {
			t.Errorf("%s sets %s, which config does not read", path, k)
		}
		if seen[k] {
			t.Errorf("%s sets %s twice", path, k)
		}
		seen[k] = true
	}
	if !complete {
		return
	}
	for n := range known {
		if !seen[n] {
			t.Errorf("%s does not mention %s", path, n)
		}
	}
}

// loadFile parses path the way main does and runs Load over it alone.
func loadFile(t *testing.T, path string) Config {
	t.Helper()
	vars, err := godotenv.Read(path)
	if err != nil {
		t.Fatalf("godotenv.Read(%s): %v", path, err)
	}
	cfg, err := Load(func(k string) (string, bool) { v, ok := vars[k]; return v, ok })
	if err != nil {
		t.Fatalf("%s does not load: %v", path, err)
	}
	return cfg
}

func TestEnvExampleListsEverySettingOnce(t *testing.T) {
	checkKeys(t, envExamplePath, true)
}

func TestEnvExampleLoads(t *testing.T) {
	cfg := loadFile(t, envExamplePath)

	// The copy-me file must not open the server to the network by default.
	if !strings.HasPrefix(cfg.ListenAddr, "127.0.0.1:") {
		t.Errorf("ListenAddr = %q, want loopback in the example", cfg.ListenAddr)
	}
	if cfg.TrustedProxyHops != 0 {
		t.Errorf("TrustedProxyHops = %d, want 0 in the example: anything else trusts X-Forwarded-For",
			cfg.TrustedProxyHops)
	}
	if cfg.MaxVMsPerClient < 1 {
		t.Errorf("MaxVMsPerClient = %d, want a limit", cfg.MaxVMsPerClient)
	}
}

// .env is not committed, so this runs only where one exists. It catches a
// misspelt key, which would otherwise be ignored without a word.
func TestLocalEnvUsesOnlyKnownSettingsAndLoads(t *testing.T) {
	if _, err := os.Stat(envLocalPath); errors.Is(err, fs.ErrNotExist) {
		t.Skip("no .env")
	}
	checkKeys(t, envLocalPath, false)
	loadFile(t, envLocalPath)
}
