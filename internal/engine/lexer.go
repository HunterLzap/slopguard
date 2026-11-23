package engine

import (
	"path/filepath"
	"strings"
)

// LexedLine 保存源码单行经词法剥离后的代码片段、注释片段与原始文本。
type LexedLine struct {
	Line     int
	Code     string
	Comments string
	Raw      string
}

type commentStyle int

const (
	commentStyleUnknown commentStyle = iota
	commentStyleC
	commentStyleHash
)

type lexLanguage struct {
	style             commentStyle
	templateInterp    bool // JS/TS 模板字符串支持 ${...}
	backtickRawString bool // Go 等反引号原生字符串
	tripleQuoteString bool // Python ''' / """ 多行字符串
}

func languageForFile(filename string) lexLanguage {
	base := strings.ToLower(filepath.Base(filename))
	switch base {
	case "makefile", "dockerfile", "containerfile":
		return lexLanguage{style: commentStyleHash}
	}
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs":
		return lexLanguage{
			style:          commentStyleC,
			templateInterp: true,
		}
	case ".go":
		return lexLanguage{
			style:             commentStyleC,
			backtickRawString: true,
		}
	case ".rs", ".java", ".kt", ".kts", ".swift", ".c", ".cc", ".cpp", ".cxx", ".h", ".hpp", ".cs", ".php", ".scala", ".css", ".scss", ".less":
		return lexLanguage{style: commentStyleC}
	case ".py", ".pyi":
		return lexLanguage{
			style:             commentStyleHash,
			tripleQuoteString: true,
		}
	case ".sh", ".bash", ".zsh", ".rb", ".pl", ".r", ".yaml", ".yml", ".toml", ".tf", ".hcl", ".conf", ".ini":
		return lexLanguage{style: commentStyleHash}
	default:
		return lexLanguage{style: commentStyleUnknown}
	}
}

// SupportsCommentLexing 判断文件是否具备可识别的代码注释语法（用于注释探针等）。
func SupportsCommentLexing(filename string) bool {
	return languageForFile(filename).style != commentStyleUnknown
}

type lexState int

const (
	stateCode lexState = iota
	stateSingleQuote
	stateDoubleQuote
	stateBacktick
	stateTripleSingle
	stateTripleDouble
	stateLineComment
	stateBlockComment
)

// LexLines 使用轻量状态机对源码逐行剥离字符串与注释，返回每行的 Code/Comments/Raw。
// 多行模板字符串、Python 三引号字符串与 /* */ 块注释跨行保持连续状态。
func LexLines(filename, src string) []LexedLine {
	return lexLinesWithStart(filename, src, 1)
}

func lexLinesWithStart(filename, src string, startLine int) []LexedLine {
	lang := languageForFile(filename)
	src = strings.ReplaceAll(src, "\r\n", "\n")
	if src == "" {
		return nil
	}

	if lang.style == commentStyleUnknown {
		rawLines := splitRawLines(src)
		out := make([]LexedLine, len(rawLines))
		for i, raw := range rawLines {
			out[i] = LexedLine{
				Line: startLine + i,
				Code: raw,
				Raw:  raw,
			}
		}
		return out
	}

	var (
		out         []LexedLine
		state       = stateCode
		interpStack []int
		codeBuf     strings.Builder
		commentBuf  strings.Builder
		rawBuf      strings.Builder
		lineNo      = startLine
	)

	flushLine := func() {
		out = append(out, LexedLine{
			Line:     lineNo,
			Code:     codeBuf.String(),
			Comments: commentBuf.String(),
			Raw:      rawBuf.String(),
		})
		codeBuf.Reset()
		commentBuf.Reset()
		rawBuf.Reset()
		lineNo++
	}

	n := len(src)
	for i := 0; i < n; {
		ch := src[i]
		if ch == '\n' {
			flushLine()
			if state == stateLineComment || state == stateSingleQuote || state == stateDoubleQuote {
				// 单行注释和单行引号在换行处闭合，避免未闭合引号污染后续行。
				state = stateCode
			}
			i++
			continue
		}
		rawBuf.WriteByte(ch)

		switch state {
		case stateCode:
			if lang.style == commentStyleC {
				if ch == '/' && i+1 < n && src[i+1] == '/' {
					codeBuf.WriteByte(' ')
					if commentBuf.Len() > 0 {
						commentBuf.WriteByte(' ')
					}
					commentBuf.WriteString("//")
					rawBuf.WriteByte(src[i+1])
					state = stateLineComment
					i += 2
					continue
				}
				if ch == '/' && i+1 < n && src[i+1] == '*' {
					codeBuf.WriteByte(' ')
					if commentBuf.Len() > 0 {
						commentBuf.WriteByte(' ')
					}
					commentBuf.WriteString("/*")
					rawBuf.WriteByte(src[i+1])
					state = stateBlockComment
					i += 2
					continue
				}
			} else if lang.style == commentStyleHash {
				if ch == '#' {
					codeBuf.WriteByte(' ')
					if commentBuf.Len() > 0 {
						commentBuf.WriteByte(' ')
					}
					commentBuf.WriteByte('#')
					state = stateLineComment
					i++
					continue
				}
				if lang.tripleQuoteString && i+2 < n {
					if ch == '\'' && src[i+1] == '\'' && src[i+2] == '\'' {
						codeBuf.WriteByte(' ')
						rawBuf.WriteString("''")
						state = stateTripleSingle
						i += 3
						continue
					}
					if ch == '"' && src[i+1] == '"' && src[i+2] == '"' {
						codeBuf.WriteByte(' ')
						rawBuf.WriteString(`""`)
						state = stateTripleDouble
						i += 3
						continue
					}
				}
			}

			switch ch {
			case '\'':
				codeBuf.WriteByte(' ')
				state = stateSingleQuote
				i++
			case '"':
				codeBuf.WriteByte(' ')
				state = stateDoubleQuote
				i++
			case '`':
				if lang.templateInterp || lang.backtickRawString {
					codeBuf.WriteByte(' ')
					state = stateBacktick
					i++
				} else {
					codeBuf.WriteByte(ch)
					i++
				}
			case '{':
				if len(interpStack) > 0 {
					interpStack[len(interpStack)-1]++
				}
				codeBuf.WriteByte(ch)
				i++
			case '}':
				if len(interpStack) > 0 {
					interpStack[len(interpStack)-1]--
					if interpStack[len(interpStack)-1] == 0 {
						interpStack = interpStack[:len(interpStack)-1]
						state = stateBacktick
					}
				}
				codeBuf.WriteByte(ch)
				i++
			default:
				codeBuf.WriteByte(ch)
				i++
			}

		case stateSingleQuote:
			if ch == '\\' && i+1 < n && src[i+1] != '\n' {
				rawBuf.WriteByte(src[i+1])
				i += 2
				continue
			}
			if ch == '\'' {
				state = stateCode
			}
			i++

		case stateDoubleQuote:
			if ch == '\\' && i+1 < n && src[i+1] != '\n' {
				rawBuf.WriteByte(src[i+1])
				i += 2
				continue
			}
			if ch == '"' {
				state = stateCode
			}
			i++

		case stateBacktick:
			if lang.templateInterp && ch == '\\' && i+1 < n && src[i+1] != '\n' {
				rawBuf.WriteByte(src[i+1])
				i += 2
				continue
			}
			if lang.templateInterp && ch == '$' && i+1 < n && src[i+1] == '{' {
				rawBuf.WriteByte('{')
				codeBuf.WriteString("${")
				interpStack = append(interpStack, 1)
				state = stateCode
				i += 2
				continue
			}
			if ch == '`' {
				state = stateCode
			}
			i++

		case stateTripleSingle:
			if ch == '\\' && i+1 < n && src[i+1] != '\n' {
				rawBuf.WriteByte(src[i+1])
				i += 2
				continue
			}
			if ch == '\'' && i+2 < n && src[i+1] == '\'' && src[i+2] == '\'' {
				rawBuf.WriteString("''")
				state = stateCode
				i += 3
				continue
			}
			i++

		case stateTripleDouble:
			if ch == '\\' && i+1 < n && src[i+1] != '\n' {
				rawBuf.WriteByte(src[i+1])
				i += 2
				continue
			}
			if ch == '"' && i+2 < n && src[i+1] == '"' && src[i+2] == '"' {
				rawBuf.WriteString(`""`)
				state = stateCode
				i += 3
				continue
			}
			i++

		case stateLineComment:
			commentBuf.WriteByte(ch)
			i++

		case stateBlockComment:
			if ch == '*' && i+1 < n && src[i+1] == '/' {
				commentBuf.WriteString("*/")
				rawBuf.WriteByte('/')
				codeBuf.WriteByte(' ')
				state = stateCode
				i += 2
				continue
			}
			commentBuf.WriteByte(ch)
			i++
		}
	}

	if rawBuf.Len() > 0 || codeBuf.Len() > 0 || commentBuf.Len() > 0 {
		flushLine()
	}
	return out
}

func splitRawLines(src string) []string {
	src = strings.TrimSuffix(src, "\n")
	if src == "" {
		return nil
	}
	return strings.Split(src, "\n")
}
