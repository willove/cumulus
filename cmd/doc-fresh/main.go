// Command doc-fresh 检查文档引用的符号路径存在、ADR 编号连续。
//
// 治的病：文档里的代码路径过期（架构文档说改 keys.go，实际文件早改名
// 了；读者按图索骥扑空）。cumulus 的四个 UI 版本互相作废，一半是代码
// 漂移，一半是文档追着漂移跑。路径引用要么对，要么删——没有第三种。
//
// 检查两条：
//  1. 所有 .md 里反引号包裹的代码路径（internal|cmd|scripts 下的 .go/.sh）
//     必须存在于磁盘；
//  2. docs/adr/ 的编号必须从 001 起连续无重号。
//
// 用法：doc-fresh（仓库根运行）；有违反 exit 2 并逐条列出。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var pathRef = regexp.MustCompile("`((?:internal|cmd|scripts)/[A-Za-z0-9_/.-]+\\.(?:go|sh))`")
var adrName = regexp.MustCompile(`^(\d{3})-[a-z0-9-]+\.md$`)

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	var problems []string

	// 1) 文档路径引用
	mdFiles := findMarkdown(root)
	for _, md := range mdFiles {
		body, err := os.ReadFile(md)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: unreadable: %v", md, err))
			continue
		}
		for _, m := range pathRef.FindAllStringSubmatch(string(body), -1) {
			ref := m[1]
			if _, err := os.Stat(filepath.Join(root, ref)); err != nil {
				problems = append(problems, fmt.Sprintf("%s: stale path reference %q (file does not exist)", md, ref))
			}
		}
	}

	// 2) ADR 编号连续
	adrDir := filepath.Join(root, "docs", "adr")
	entries, err := os.ReadDir(adrDir)
	if err == nil {
		var nums []int
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			m := adrName.FindStringSubmatch(e.Name())
			if m == nil {
				problems = append(problems, fmt.Sprintf("docs/adr/%s: name must be NNN-slug.md", e.Name()))
				continue
			}
			n, _ := strconv.Atoi(m[1])
			nums = append(nums, n)
		}
		sort.Ints(nums)
		for i, n := range nums {
			if n != i+1 {
				problems = append(problems, fmt.Sprintf("docs/adr: numbering must be continuous from 001; got %03d at position %d", n, i+1))
				break
			}
		}
	}

	if len(problems) > 0 {
		fmt.Println("doc-fresh: stale docs")
		for _, p := range problems {
			fmt.Println("  -", p)
		}
		os.Exit(2)
	}
	fmt.Printf("doc-fresh: %d markdown files, path references and ADR numbering clean\n", len(mdFiles))
}

func findMarkdown(root string) []string {
	var out []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "bin", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".md") {
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out
}
