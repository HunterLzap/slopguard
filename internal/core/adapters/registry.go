package adapters

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type Input uint8

const (
	Project Input = iota
	Files
	TypeScript
	Kotlin
)

type Tool struct {
	Name     string
	Install  string
	Args     []string
	Input    Input
	LocalBin bool
	Optional bool
	Parse    Parser
}

type Language struct {
	ID              string
	Extensions      []string
	Markers         []string
	Groups          [][]Tool
	SampleExtension string
}

var languages = []Language{
	{
		ID: "typescript", Extensions: []string{".ts", ".tsx", ".js", ".jsx"}, Markers: []string{"package.json", "tsconfig.json"}, SampleExtension: ".ts",
		Groups: [][]Tool{
			{{Name: "tsc", Install: "npm install --save-dev typescript", Args: []string{"--noEmit", "--pretty", "false"}, Input: TypeScript, LocalBin: true, Parse: parseTypeScript}},
			{
				{Name: "eslint", Install: "npm install --save-dev eslint", Args: []string{"--format", "json"}, Input: Files, LocalBin: true, Parse: parseESLint},
				{Name: "biome", Install: "npm install --save-dev @biomejs/biome", Args: []string{"lint", "--reporter=json"}, Input: Files, LocalBin: true, Parse: parseBiome},
			},
		},
	},
	{
		ID: "python", Extensions: []string{".py", ".pyi"}, Markers: []string{"pyproject.toml", "requirements.txt"}, SampleExtension: ".py",
		Groups: [][]Tool{
			{{Name: "ruff", Install: "pip install ruff", Args: []string{"check", "--output-format=json", "--no-fix", "--no-cache"}, Input: Files, Parse: parseRuff}},
			{{Name: "pyright", Install: "npm install -g pyright", Args: []string{"--outputjson"}, Input: Files, LocalBin: true, Parse: parsePyright}},
		},
	},
	{
		ID: "go", Extensions: []string{".go"}, Markers: []string{"go.mod"}, SampleExtension: ".go",
		Groups: [][]Tool{
			{{Name: "go", Install: "https://go.dev/dl/", Args: []string{"vet", "./..."}, Parse: parseNative}},
			{{Name: "golangci-lint", Install: "https://golangci-lint.run/welcome/install/", Args: []string{"run"}, Optional: true, Parse: parseNative}},
		},
	},
	{
		ID: "rust", Extensions: []string{".rs"}, Markers: []string{"Cargo.toml"}, SampleExtension: ".rs",
		Groups: [][]Tool{
			{{Name: "cargo", Install: "rustup component add clippy", Args: []string{"clippy", "--message-format=json"}, Parse: parseCargo}},
		},
	},
	{
		ID: "jvm", Extensions: []string{".java", ".kt", ".kts"}, Markers: []string{"pom.xml", "build.gradle", "build.gradle.kts"}, SampleExtension: ".kt",
		Groups: [][]Tool{
			{
				{Name: "ktlint", Install: "brew install ktlint", Args: []string{"--reporter=plain"}, Input: Kotlin, Parse: parseNative},
				{Name: "gradlew", Install: "gradle wrapper", Args: []string{"--console=plain", "check"}, Parse: parseNative},
				{Name: "mvn", Install: "https://maven.apache.org/install.html", Args: []string{"-B", "verify"}, Parse: parseNative},
			},
		},
	},
	{
		ID: "dart", Extensions: []string{".dart"}, Markers: []string{"pubspec.yaml"}, SampleExtension: ".dart",
		Groups: [][]Tool{
			{
				{Name: "dart", Install: "https://dart.dev/get-dart", Args: []string{"analyze", "--format=machine", "--no-fatal-warnings"}, Parse: parseDart},
				{Name: "flutter", Install: "https://docs.flutter.dev/install", Args: []string{"analyze", "--no-pub", "--no-fatal-infos", "--no-fatal-warnings"}, Parse: parseDart},
			},
		},
	},
	{
		ID: "cpp", Extensions: []string{".c", ".cc", ".cpp", ".cxx", ".h", ".hh", ".hpp", ".hxx"}, Markers: []string{"CMakeLists.txt", "compile_commands.json"}, SampleExtension: ".cpp",
		Groups: [][]Tool{
			{
				{Name: "cppcheck", Install: "https://cppcheck.sourceforge.io/", Args: []string{"--enable=warning,style,performance,portability", "--error-exitcode=1", "--template={file}:{line}:{column}: {severity}: {message} [{id}]"}, Input: Files, Parse: parseNative},
				{Name: "clang-tidy", Install: "https://clang.llvm.org/extra/clang-tidy/", Input: Files, Parse: parseNative},
			},
		},
	},
}

func Match(path string) (Language, bool) {
	for _, language := range languages {
		if slices.Contains(language.Markers, filepath.Base(path)) || language.Source(path) {
			return language, true
		}
	}
	return Language{}, false
}

func (language Language) Source(path string) bool {
	return slices.Contains(language.Extensions, strings.ToLower(filepath.Ext(path)))
}

func (language Language) Root(project, file string) string {
	for dir := filepath.Dir(filepath.Join(project, file)); dir != project; dir = filepath.Dir(dir) {
		for _, marker := range language.Markers {
			if Regular(filepath.Join(dir, marker)) {
				return dir
			}
		}
	}
	return project
}

func SampleExtension(language string) string {
	for _, adapter := range languages {
		if adapter.ID == language {
			return adapter.SampleExtension
		}
	}
	return ".txt"
}

func Regular(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().IsRegular()
}
