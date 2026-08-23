package agentfs

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

type parsedDocument struct {
	text     string
	mime     string
	language string
	hash     string
	chunks   []parsedChunk
}

type parsedChunk struct {
	ordinal  int
	language string
	symbol   string
	start    int
	end      int
	content  string
	hash     string
	vector   []float32
}

func extractDocument(ctx context.Context, path string, limit int) (parsedDocument, error) {
	extension := strings.ToLower(filepath.Ext(path))
	var text, mimeType string
	var err error
	switch extension {
	case ".pdf":
		text, err = extractPDF(ctx, path, limit)
		mimeType = "application/pdf"
	case ".docx", ".pptx", ".xlsx":
		text, err = extractOffice(path, extension, limit)
		mimeType = officeMIME(extension)
	default:
		text, mimeType, err = extractPlain(path, limit)
	}
	if err != nil {
		return parsedDocument{}, err
	}
	language := languageForExtension(extension)
	document := parsedDocument{text: text, mime: mimeType, language: language, hash: hashText(text)}
	if strings.TrimSpace(text) == "" {
		return document, nil
	}
	if extension == ".go" {
		document.chunks = goASTChunks(path, text)
	} else if tsLanguageForExtension(extension) != nil {
		document.chunks = treeSitterChunks(text, extension)
	}
	if len(document.chunks) == 0 {
		document.chunks = windowChunks(text, language, "")
	}
	for index := range document.chunks {
		document.chunks[index].ordinal = index
		document.chunks[index].hash = hashText(document.chunks[index].content)
	}
	return document, nil
}

func extractPlain(path string, limit int) (string, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return "", "", errors.Join(readErr, closeErr)
	}
	if len(raw) > limit {
		raw = raw[:limit]
	}
	mimeType := http.DetectContentType(raw)
	if bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(raw) {
		return "", mimeType, nil
	}
	return string(raw), mimeType, nil
}

func extractPDF(ctx context.Context, path string, limit int) (string, error) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		return "", fmt.Errorf("extract PDF %s: pdftotext is required: %w", path, err)
	}
	commandCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(commandCtx, "pdftotext", "-layout", "-nopgbrk", path, "-")
	pipe, err := command.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("open pdftotext output: %w", err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return "", fmt.Errorf("start pdftotext: %w", err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(pipe, int64(limit)+1))
	_, drainErr := io.Copy(io.Discard, pipe)
	waitErr := command.Wait()
	if commandCtx.Err() != nil {
		return "", fmt.Errorf("extract PDF %s: %w", path, commandCtx.Err())
	}
	if readErr != nil || drainErr != nil || waitErr != nil {
		return "", fmt.Errorf("extract PDF %s: %w: %s", path,
			errors.Join(readErr, drainErr, waitErr), strings.TrimSpace(stderr.String()))
	}
	if len(raw) > limit {
		raw = raw[:limit]
	}
	return strings.TrimSpace(string(raw)), nil
}

func extractOffice(path, extension string, limit int) (string, error) {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return "", fmt.Errorf("open Office archive %s: %w", path, err)
	}
	defer archive.Close()
	entries := make([]*zip.File, 0, len(archive.File))
	for _, entry := range archive.File {
		if officeTextEntry(extension, entry.Name) {
			entries = append(entries, entry)
		}
	}
	slices.SortFunc(entries, func(left, right *zip.File) int { return strings.Compare(left.Name, right.Name) })
	var output strings.Builder
	for _, entry := range entries {
		if output.Len() >= limit {
			break
		}
		if entry.UncompressedSize64 > uint64(limit)*8 {
			return "", fmt.Errorf("Office XML entry %s exceeds extraction bound", entry.Name)
		}
		reader, err := entry.Open()
		if err != nil {
			return "", fmt.Errorf("open Office XML %s: %w", entry.Name, err)
		}
		text, parseErr := extractXMLText(io.LimitReader(reader, int64(limit-output.Len())+1), limit-output.Len())
		closeErr := reader.Close()
		if parseErr != nil || closeErr != nil {
			return "", fmt.Errorf("parse Office XML %s: %w", entry.Name, errors.Join(parseErr, closeErr))
		}
		if output.Len() > 0 && text != "" {
			output.WriteByte('\n')
		}
		output.WriteString(text)
	}
	return strings.TrimSpace(output.String()), nil
}

func extractXMLText(reader io.Reader, limit int) (string, error) {
	decoder := xml.NewDecoder(reader)
	var output strings.Builder
	capture := false
	for {
		tokenValue, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		switch value := tokenValue.(type) {
		case xml.StartElement:
			capture = value.Name.Local == "t" || value.Name.Local == "v"
		case xml.EndElement:
			if value.Name.Local == "p" || value.Name.Local == "row" || value.Name.Local == "si" {
				output.WriteByte('\n')
			}
			capture = false
		case xml.CharData:
			if capture {
				if output.Len()+len(value) > limit {
					remaining := max(0, limit-output.Len())
					output.Write(value[:remaining])
					return output.String(), nil
				}
				output.Write(value)
				output.WriteByte(' ')
			}
		}
	}
	return output.String(), nil
}

func officeTextEntry(extension, name string) bool {
	switch extension {
	case ".docx":
		return name == "word/document.xml" || strings.HasPrefix(name, "word/header") || strings.HasPrefix(name, "word/footer")
	case ".pptx":
		return strings.HasPrefix(name, "ppt/slides/slide") && strings.HasSuffix(name, ".xml")
	case ".xlsx":
		return name == "xl/sharedStrings.xml" || strings.HasPrefix(name, "xl/worksheets/sheet")
	default:
		return false
	}
}

func officeMIME(extension string) string {
	switch extension {
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	default:
		return "application/zip"
	}
}

func goASTChunks(filename, source string) []parsedChunk {
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, filename, source, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	tokenFile := fileSet.File(parsed.Pos())
	if tokenFile == nil {
		return nil
	}
	chunks := make([]parsedChunk, 0, len(parsed.Decls)+1)
	for _, declaration := range parsed.Decls {
		start := tokenFile.Offset(declaration.Pos())
		end := tokenFile.Offset(declaration.End())
		if start < 0 || end <= start || end > len(source) {
			continue
		}
		symbol := goDeclarationName(declaration)
		startLine := fileSet.Position(declaration.Pos()).Line
		endLine := fileSet.Position(declaration.End()).Line
		content := source[start:end]
		parts := windowChunks(content, "go", symbol)
		for index := range parts {
			parts[index].start = startLine
			parts[index].end = endLine
		}
		chunks = append(chunks, parts...)
	}
	return chunks
}

func goDeclarationName(declaration ast.Decl) string {
	switch value := declaration.(type) {
	case *ast.FuncDecl:
		if value.Recv != nil && len(value.Recv.List) > 0 {
			return receiverName(value.Recv.List[0].Type) + "." + value.Name.Name
		}
		return value.Name.Name
	case *ast.GenDecl:
		names := make([]string, 0, len(value.Specs))
		for _, specification := range value.Specs {
			switch item := specification.(type) {
			case *ast.TypeSpec:
				names = append(names, item.Name.Name)
			case *ast.ValueSpec:
				for _, name := range item.Names {
					names = append(names, name.Name)
				}
			case *ast.ImportSpec:
				names = append(names, strings.Trim(item.Path.Value, `"`))
			}
		}
		return strings.Join(names, ",")
	default:
		return ""
	}
}

func receiverName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.StarExpr:
		return receiverName(value.X)
	case *ast.IndexExpr:
		return receiverName(value.X)
	case *ast.IndexListExpr:
		return receiverName(value.X)
	default:
		return "receiver"
	}
}

func windowChunks(text, language, symbol string) []parsedChunk {
	const size = 1200
	const overlap = 200
	runes := []rune(text)
	chunks := make([]parsedChunk, 0, max(1, len(runes)/size))
	for start := 0; start < len(runes); start += size - overlap {
		end := min(len(runes), start+size)
		content := strings.TrimSpace(string(runes[start:end]))
		if content != "" {
			chunks = append(chunks, parsedChunk{language: language, symbol: symbol, content: content})
		}
		if end == len(runes) {
			break
		}
	}
	return chunks
}

func languageForExtension(extension string) string {
	return map[string]string{
		".go": "go", ".py": "python", ".js": "javascript", ".jsx": "javascript",
		".ts": "typescript", ".tsx": "typescript", ".rs": "rust", ".c": "c", ".h": "c",
		".cc": "cpp", ".cpp": "cpp", ".hpp": "cpp", ".java": "java", ".kt": "kotlin",
		".swift": "swift", ".md": "markdown", ".sql": "sql", ".sh": "shell",
	}[extension]
}

func hashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
