package radar

import (
	"path"
	"regexp"
	"strings"
)

var (
	tsImportRE = regexp.MustCompile(`(?m)(?:import\s+(?:[^'"]*?\s+from\s+)?|export\s+[^'"]*?\s+from\s+|require\(\s*|import\(\s*)['"]([^'"]+)['"]`)
	pyFromRE   = regexp.MustCompile(`(?m)^\s*from\s+([.\w]+)\s+import\s+([^\n#]+)`)
	pyImportRE = regexp.MustCompile(`(?m)^\s*import\s+([.\w]+(?:\s*,\s*[.\w]+)*)`)
)

// extractImports 用纯文本正则提取 TS/JS 与 Python 的 import 目标。
func extractImports(file, content string) []string {
	switch strings.ToLower(path.Ext(file)) {
	case ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs":
		var specs []string
		for _, match := range tsImportRE.FindAllStringSubmatch(content, -1) {
			if len(match) > 1 && match[1] != "" {
				specs = append(specs, match[1])
			}
		}
		return specs
	case ".py", ".pyi":
		var specs []string
		for _, match := range pyFromRE.FindAllStringSubmatch(content, -1) {
			if len(match) < 2 || match[1] == "" {
				continue
			}
			base := match[1]
			if base != "." {
				specs = append(specs, base)
			}
			if len(match) > 2 {
				for _, name := range parsePythonNames(match[2]) {
					specs = append(specs, joinPythonModule(base, name))
				}
			}
		}
		for _, match := range pyImportRE.FindAllStringSubmatch(content, -1) {
			if len(match) <= 1 {
				continue
			}
			for _, part := range strings.Split(match[1], ",") {
				part = strings.TrimSpace(part)
				if idx := strings.IndexAny(part, " \t"); idx >= 0 {
					part = part[:idx]
				}
				if part != "" {
					specs = append(specs, part)
				}
			}
		}
		return specs
	default:
		return nil
	}
}

// buildReferences 统计每个文件被多少个其他文件 import。
func buildReferences(metrics map[string]*fileMetric, extensions []string) map[string]int {
	refs := make(map[string]map[string]struct{})
	for importer, metric := range metrics {
		for _, spec := range metric.imports {
			var targets []string
			switch strings.ToLower(path.Ext(importer)) {
			case ".py", ".pyi":
				targets = resolvePythonImport(importer, spec, metrics)
			default:
				targets = resolveTSImport(importer, spec, metrics, extensions)
			}
			for _, target := range targets {
				if target == importer {
					continue
				}
				if refs[target] == nil {
					refs[target] = make(map[string]struct{})
				}
				refs[target][importer] = struct{}{}
			}
		}
	}
	out := make(map[string]int, len(refs))
	for target, importers := range refs {
		out[target] = len(importers)
	}
	return out
}

func resolveTSImport(importer, spec string, files map[string]*fileMetric, extensions []string) []string {
	if spec == "" {
		return nil
	}
	var base string
	if strings.HasPrefix(spec, "/") {
		base = path.Clean(strings.TrimPrefix(spec, "/"))
	} else if strings.HasPrefix(spec, ".") {
		base = path.Clean(path.Join(path.Dir(importer), spec))
	} else {
		return nil
	}

	candidates := []string{base}
	if ext := path.Ext(base); ext != "" {
		stem := strings.TrimSuffix(base, ext)
		for _, candidateExt := range extensions {
			candidates = append(candidates, stem+candidateExt)
		}
	}
	for _, candidateExt := range extensions {
		candidates = append(candidates, base+candidateExt)
	}
	for _, candidateExt := range extensions {
		candidates = append(candidates, path.Join(base, "index"+candidateExt))
	}
	return existingFiles(candidates, files)
}

func resolvePythonImport(importer, spec string, files map[string]*fileMetric) []string {
	if spec == "" {
		return nil
	}
	var candidates []string
	if strings.HasPrefix(spec, ".") {
		dots := 0
		for dots < len(spec) && spec[dots] == '.' {
			dots++
		}
		module := spec[dots:]
		dir := path.Dir(importer)
		for i := 1; i < dots; i++ {
			dir = path.Dir(dir)
		}
		if module == "" {
			candidates = append(candidates, path.Join(dir, "__init__.py"))
		} else {
			candidates = append(candidates, pythonCandidates(path.Join(dir, strings.ReplaceAll(module, ".", "/")))...)
		}
	} else {
		module := strings.ReplaceAll(spec, ".", "/")
		candidates = append(candidates, pythonCandidates(module)...)
		candidates = append(candidates, pythonCandidates(path.Join("src", module))...)
	}
	return existingFiles(candidates, files)
}

func pythonCandidates(base string) []string {
	return []string{base + ".py", base + ".pyi", path.Join(base, "__init__.py")}
}

func parsePythonNames(raw string) []string {
	raw = strings.TrimSpace(strings.Trim(raw, "()"))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	names := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if idx := strings.IndexAny(part, " \t"); idx >= 0 {
			part = part[:idx]
		}
		if part == "" || part == "*" {
			continue
		}
		names = append(names, part)
	}
	return names
}

func joinPythonModule(base, name string) string {
	if name == "" {
		return base
	}
	if base == "." {
		return "." + name
	}
	return strings.TrimSuffix(base, ".") + "." + name
}

func existingFiles(candidates []string, files map[string]*fileMetric) []string {
	seen := make(map[string]struct{}, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = path.Clean(candidate)
		if _, ok := files[candidate]; !ok {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
	}
	return out
}
