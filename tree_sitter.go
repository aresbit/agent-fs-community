package agentfs

import (
	"context"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/c"
	"github.com/smacker/go-tree-sitter/cpp"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/rust"
)

// tsLanguageForExtension 把扩展名映射到 tree-sitter grammar。
// 返回 nil 表示该扩展名没有 tree-sitter 支持，回退到窗口 chunk。
func tsLanguageForExtension(extension string) *sitter.Language {
	switch extension {
	case ".c", ".h":
		return c.GetLanguage()
	case ".cc", ".cpp", ".hpp", ".cxx":
		return cpp.GetLanguage()
	case ".py", ".pyw":
		return python.GetLanguage()
	case ".js", ".jsx", ".mjs", ".cjs":
		return javascript.GetLanguage()
	case ".go":
		return golang.GetLanguage()
	case ".rs":
		return rust.GetLanguage()
	case ".java":
		return java.GetLanguage()
	default:
		return nil
	}
}

// isFunctionNode 判断一个 tree-sitter 节点是不是「函数/方法」级声明，
// 这是检索的最小语义单元。namespace / class / impl 是容器，递归进入而非成 chunk。
func isFunctionNode(nodeType string) bool {
	for _, marker := range []string{
		"function_definition", "function_declaration", "method_declaration",
		"function_item", "constructor_declaration",
	} {
		if strings.Contains(nodeType, marker) {
			return true
		}
	}
	return false
}

// nodeSymbol 提取声明节点的符号名：优先 name field，其次 declarator field（C/C++ 函数名
// 藏在 function_declarator → declarator → identifier 里），最后回退到第一个 identifier。
func nodeSymbol(node *sitter.Node, source []byte) string {
	if name := node.ChildByFieldName("name"); name != nil {
		return name.Content(source)
	}
	if declarator := node.ChildByFieldName("declarator"); declarator != nil {
		if symbol := identifierContent(declarator, source); symbol != "" {
			return symbol
		}
	}
	for index := 0; index < int(node.NamedChildCount()); index++ {
		child := node.NamedChild(index)
		switch child.Type() {
		case "identifier", "type_identifier", "field_identifier", "name",
			"namespace_identifier", "property_identifier":
			return child.Content(source)
		}
	}
	return ""
}

// identifierContent 递归下沉 declarator/function_declarator/pointer 等包装节点，
// 找到最内层的 identifier 并返回其内容。C 里函数名是 function_definition.declarator
// → function_declarator.declarator → identifier 三层嵌套。
func identifierContent(node *sitter.Node, source []byte) string {
	if node == nil {
		return ""
	}
	switch node.Type() {
	case "identifier", "field_identifier", "type_identifier", "function_identifier",
		"namespace_identifier", "property_identifier", "name":
		return node.Content(source)
	}
	if name := node.ChildByFieldName("declarator"); name != nil {
		if symbol := identifierContent(name, source); symbol != "" {
			return symbol
		}
	}
	if name := node.ChildByFieldName("name"); name != nil {
		if symbol := identifierContent(name, source); symbol != "" {
			return symbol
		}
	}
	for index := 0; index < int(node.NamedChildCount()); index++ {
		if symbol := identifierContent(node.NamedChild(index), source); symbol != "" {
			return symbol
		}
	}
	return ""
}

// collectFunctionChunks 递归遍历 AST，收集函数/方法级声明为 chunk，
// 容器（namespace/class/impl）递归进入，本身不单独成 chunk。
func collectFunctionChunks(node *sitter.Node, source []byte, language string, chunks *[]parsedChunk) {
	if node == nil {
		return
	}
	if isFunctionNode(node.Type()) {
		symbol := nodeSymbol(node, source)
		content := strings.TrimSpace(node.Content(source))
		if content == "" {
			return
		}
		startLine := int(node.StartPoint().Row) + 1
		endLine := int(node.EndPoint().Row) + 1
		parts := windowChunks(content, language, symbol)
		for index := range parts {
			parts[index].start = startLine
			parts[index].end = endLine
		}
		*chunks = append(*chunks, parts...)
		return
	}
	for index := 0; index < int(node.NamedChildCount()); index++ {
		collectFunctionChunks(node.NamedChild(index), source, language, chunks)
	}
}

// isCallNode 判断节点是否是「函数调用」。
func isCallNode(nodeType string) bool {
	for _, marker := range []string{
		"call_expression", "method_invocation", "function_call",
		"call", "invocation_expression",
	} {
		if strings.Contains(nodeType, marker) {
			return true
		}
	}
	return false
}

// callCallee 提取调用节点的被调函数名（function field 或第一个 identifier）。
func callCallee(node *sitter.Node, source []byte) string {
	if fn := node.ChildByFieldName("function"); fn != nil {
		if name := fn.ChildByFieldName("field"); name != nil { // 方法调用 obj.method()
			return name.Content(source)
		}
		if fn.Type() == "identifier" || fn.Type() == "field_identifier" ||
			fn.Type() == "function_identifier" {
			return fn.Content(source)
		}
		// 成员访问 a.b() → 取最后一段
		if fn.Type() == "member_expression" || fn.Type() == "field_expression" ||
			fn.Type() == "attribute" {
			if field := fn.ChildByFieldName("field"); field != nil {
				return field.Content(source)
			}
			if fn.NamedChildCount() > 0 {
				return fn.NamedChild(int(fn.NamedChildCount()) - 1).Content(source)
			}
		}
	}
	return ""
}

type symbolDef struct {
	symbol    string
	kind      string
	startLine int
	endLine   int
}

type symbolRef struct {
	caller string
	callee string
	line   int
}

// treeSitterSymbols 提取函数定义与调用引用，构成符号图。
// 返回 defs（函数/符号定义）与 refs（caller → callee 引用）。
func treeSitterSymbols(source string, extension string) (defs []symbolDef, refs []symbolRef) {
	lang := tsLanguageForExtension(extension)
	if lang == nil {
		return nil, nil
	}
	parser := sitter.NewParser()
	defer parser.Close()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, []byte(source))
	if err != nil || tree == nil {
		return nil, nil
	}
	defer tree.Close()

	var walk func(node *sitter.Node, caller string)
	walk = func(node *sitter.Node, caller string) {
		if node == nil {
			return
		}
		if isFunctionNode(node.Type()) {
			symbol := nodeSymbol(node, []byte(source))
			if symbol != "" {
				defs = append(defs, symbolDef{
					symbol:    symbol,
					kind:      "function",
					startLine: int(node.StartPoint().Row) + 1,
					endLine:   int(node.EndPoint().Row) + 1,
				})
			}
			for index := 0; index < int(node.NamedChildCount()); index++ {
				walk(node.NamedChild(index), symbol)
			}
			return
		}
		if isCallNode(node.Type()) {
			if callee := callCallee(node, []byte(source)); callee != "" {
				refs = append(refs, symbolRef{
					caller: caller,
					callee: callee,
					line:   int(node.StartPoint().Row) + 1,
				})
			}
		}
		for index := 0; index < int(node.NamedChildCount()); index++ {
			walk(node.NamedChild(index), caller)
		}
	}
	walk(tree.RootNode(), "")
	return defs, refs
}
func treeSitterChunks(source string, extension string) []parsedChunk {
	lang := tsLanguageForExtension(extension)
	if lang == nil {
		return nil
	}
	language := languageForExtension(extension)
	parser := sitter.NewParser()
	defer parser.Close()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, []byte(source))
	if err != nil || tree == nil {
		return nil
	}
	defer tree.Close()

	chunks := make([]parsedChunk, 0, 64)
	collectFunctionChunks(tree.RootNode(), []byte(source), language, &chunks)
	if len(chunks) == 0 {
		// 没有函数级声明的文件（如纯数据头文件）回退到窗口 chunk。
		return windowChunks(source, language, "")
	}
	return chunks
}
