// processorboundary checks active backend syntax as well as imports. This is
// an architecture regression guard, not a sandbox against obfuscated Go code.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const module = "github.com/spencernorton/DH-Waypoint"

var blocked = []string{"/llmprovider", "/llmmatch", "/llmstage", "/llmreadiness", "/llmeval", "/llmcapture", "/llmwork", "/onnx", "/tensorflow", "/torch", "/adapters/", "openai-go", "go-openai", "anthropic-sdk"}

type pkg struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	CgoFiles   []string
}

func dependencies(pattern string) ([]pkg, error) {
	cmd := exec.Command("go", "list", "-deps", "-json", pattern)
	cmd.Stderr = os.Stderr
	b, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	var out []pkg
	for {
		var p pkg
		if err := d.Decode(&p); err == io.EOF {
			return out, nil
		} else if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
}

func sourceViolations(path string, source any) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
	if err != nil {
		return nil, err
	}
	var out []string
	values := map[string]bool{}
	path = filepath.ToSlash(path)
	processor := strings.Contains(path, "internal/processors/")
	for _, imp := range f.Imports {
		name, _ := strconv.Unquote(imp.Path.Value)
		if processor && (name == "net/http" || name == "net") &&
			!strings.HasSuffix(path, "/transport.go") && !strings.HasSuffix(path, "/http.go") {
			out = append(out, "processor network I/O belongs in transport.go: "+name)
		}
	}
	ast.Inspect(f, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.BasicLit:
			if n.Kind != token.STRING {
				break
			}
			v, _ := strconv.Unquote(n.Value)
			v = strings.ToLower(v)
			values[v] = true
			for _, marker := range []string{"chat/completions", "/api/generate", "/api/chat", "api.openai.com", "api.anthropic.com", "max_completion_tokens", "system_prompt", "response_format", "openai_api_key", "rule_adjustments"} {
				if strings.Contains(v, marker) {
					out = append(out, "embedded inference/learning literal: "+marker)
				}
			}
		case *ast.FuncDecl:
			if strings.Contains(strings.ToLower(n.Name.Name), "builtinllm") || strings.Contains(strings.ToLower(n.Name.Name), "calibratebuiltin") {
				out = append(out, "embedded inference/learning function: "+n.Name.Name)
			}
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && processor && !strings.HasSuffix(path, "/transport.go") {
				switch sel.Sel.Name {
				case "Do", "Post", "PostForm", "NewRequest", "NewRequestWithContext", "RoundTrip", "Dial", "DialContext":
					out = append(out, "processor outbound call outside transport.go: "+sel.Sel.Name)
				}
			}
		}
		return true
	})
	if values["model"] && (values["messages"] || values["temperature"]) {
		out = append(out, "embedded provider request fields")
	}
	return out, nil
}

func check() error {
	deps, err := dependencies(".")
	if err != nil {
		return err
	}
	var violations []string
	for _, p := range deps {
		for _, name := range blocked {
			if strings.Contains(p.ImportPath, name) {
				violations = append(violations, "embedded runtime dependency: "+p.ImportPath)
			}
		}
		if !strings.HasPrefix(p.ImportPath, module+"/") && p.ImportPath != module {
			continue
		}
		for _, name := range append(p.GoFiles, p.CgoFiles...) {
			found, err := sourceViolations(filepath.Join(p.Dir, name), nil)
			if err != nil {
				return err
			}
			for _, v := range found {
				violations = append(violations, p.ImportPath+"/"+name+": "+v)
			}
		}
	}
	for _, adapter := range []string{"./adapters/llm/cmd/dh-waypoint-llm", "./adapters/armiger/cmd/dh-waypoint-armiger"} {
		deps, err = dependencies(adapter)
		if err != nil {
			return err
		}
		for _, p := range deps {
			if strings.HasPrefix(p.ImportPath, module+"/internal/") || p.ImportPath == module {
				violations = append(violations, "adapter imports backend internals: "+p.ImportPath)
			}
		}
	}
	if len(violations) > 0 {
		return fmt.Errorf("processor boundary violations:\n%s", strings.Join(violations, "\n"))
	}
	return nil
}

func main() {
	if err := check(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("Backend graph and source contain no embedded inference/learning; adapters import only the public contract.")
}
