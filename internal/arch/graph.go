package arch

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ModuleRoot 从本包向上找 go.mod（测试与门禁的 CWD 不同，所以不能写死）。
func ModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		dir = filepath.Dir(dir)
	}
	return "", fmt.Errorf("arch: 找不到 go.mod")
}

// RealGraph 读**真实**的内部导入图（`go list`）。
func RealGraph() ([]Dependency, error) {
	root, err := ModuleRoot()
	if err != nil {
		return nil, err
	}
	// 模板输出：pkg<TAB>dep,dep,...（只保留 module 内部的）
	tmpl := "{{.ImportPath}}\t{{join .Imports \" \"}}"
	cmd := exec.Command("go", "list", "-f", tmpl, "./...")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("arch: go list: %w: %s", err, strings.TrimSpace(errBuf.String()))
	}
	const prefix = "github.com/willove/cumulus/"
	var deps []Dependency
	for _, ln := range strings.Split(out.String(), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		pkg, rest, ok := strings.Cut(ln, "\t")
		if !ok || !strings.HasPrefix(pkg, prefix) {
			continue
		}
		from := normalize(strings.TrimPrefix(pkg, prefix))
		for _, dep := range strings.Fields(rest) {
			if !strings.HasPrefix(dep, prefix) {
				continue
			}
			deps = append(deps, Dependency{From: from, To: normalize(strings.TrimPrefix(dep, prefix))})
		}
	}
	return deps, nil
}

// InternalPackages 列出模块内所有包（相对路径），用于"新包必须登记"的检查。
func InternalPackages() ([]string, error) {
	root, err := ModuleRoot()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("go", "list", "./...")
	cmd.Dir = root
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("arch: go list: %w", err)
	}
	const prefix = "github.com/willove/cumulus/"
	var pkgs []string
	for _, ln := range strings.Split(out.String(), "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, prefix) {
			pkgs = append(pkgs, normalize(strings.TrimPrefix(ln, prefix)))
		}
	}
	return pkgs, nil
}

// normalize 把包路径归一成分层表里的形状：internal/x → x。
// cmd/** 保持原样（应用面记全名，报表更清楚）。
func normalize(p string) string {
	return strings.TrimPrefix(p, "internal/")
}
