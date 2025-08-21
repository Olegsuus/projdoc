// cmd/app/main.go
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"html"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	srcDir      = flag.String("src", ".", "Go source root")
	migDir      = flag.String("migrations", "./migrations", "SQL migrations dir")
	outFile     = flag.String("out", "docs.md", "Output markdown file")
	emitMermaid = flag.Bool("mermaid", true, "Emit Mermaid ER diagram")
	openapiOut  = flag.String("openapi", "", "Write OpenAPI 3.0 yaml to this file if set")

	goSkipVendor = true

	reIsTableLevelConstraint = regexp.MustCompile(`(?is)^\s*(PRIMARY\s+KEY|FOREIGN\s+KEY|UNIQUE|CHECK|CONSTRAINT|EXCLUDE|INDEX)\b`)
	reTrimQuotes             = regexp.MustCompile(`^"(.*)"$`)
)

// ========================= Data models =========================

type DocItem struct {
	Section string // endpoint|service|repo|domain|model
	ID      string
	Summary string
	Details string
	Tags    []string
	Errors  []string
	Example string

	RepoQuery  string
	ServiceUC  string
	SourceFile string
	SourceLine int
	GoSymbol   string

	Extra map[string]string // любые @doc.<key>

	// Автодополненные поля из анализа
	RequestType  string
	ResponseType string
	Statuses     []int
	Route        *Route
	Calls        []string // человекочитаемые call targets
}

type Table struct {
	Name       string
	Columns    []Column
	PrimaryKey []string
	Foreign    []ForeignKey
	Indexes    []Index
}

type Column struct {
	Name string
	Type string
	Null bool
}

type ForeignKey struct {
	Cols     []string
	RefTable string
	RefCols  []string
	OnDelete string
	OnUpdate string
}

type Index struct {
	Name    string
	Table   string
	Columns []string
	Unique  bool
}

// Models (Go structs)
type Model struct {
	ID          string
	Summary     string
	Details     string
	Tags        []string
	Table       string
	Lifecycle   string
	Invariants  string
	Permissions string
	Example     string
	SourceFile  string
	SourceLine  int
	Name        string // Go type name
	Fields      []ModelField
}

type ModelField struct {
	Name     string
	Type     string
	JSON     string
	DB       string
	Validate []string
	DocTags  []string
	Comment  string
}

var collectedModels []Model

// Routes
type Route struct {
	Method     string
	Path       string
	Auth       bool
	File       string
	Line       int
	HandlerSym string
}

// Call graph
type FuncKey struct {
	Pkg string // файл->пакет
	Rec string // получатель (напр. *clientService)
	Nom string // имя (напр. SelectManyClients)
}

func (k FuncKey) String() string {
	if k.Rec != "" {
		return fmt.Sprintf("(%s).%s", k.Rec, k.Nom)
	}
	if k.Pkg != "" {
		return k.Pkg + "." + k.Nom
	}
	return k.Nom
}

type FuncDeclInfo struct {
	Key     FuncKey
	File    string
	Line    int
	Calls   []string // собранные имена вызовов (сырьё)
	Comment string
}

// ========================= Main =========================

func main() {
	flag.Parse()

	// 1) SQL
	tables, _ := parseMigrations(*migDir)

	// 2) Сбор функций (для call graph)
	funcs := collectAllFunctions(*srcDir)

	// 3) Go @doc.* (включая внутренние комментарии) и модели
	docs := parseGoDocs(*srcDir, funcs)

	// 4) Роуты Fiber
	routes := parseFiberRoutes(*srcDir)

	// 5) Сопоставим роуты к endpoint-докам по имени хендлера
	attachRoutes(docs, routes)

	// 6) Обогатим call graph: сопоставим «сырьё» к DocItem’ам (плоский список)
	resolveCallGraph(docs, funcs)

	// 7) Построим рекурсивные потоки для ручек
	graphs := buildCallGraphs(docs, funcs)

	// 8) Markdown
	md := renderMarkdown(tables, docs, collectedModels, *emitMermaid, graphs)
	if err := os.WriteFile(*outFile, md, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *outFile, err)
		os.Exit(1)
	}
	fmt.Printf("Generated %s\n", *outFile)

	// 9) HTML-диаграммы
	if err := writeFlowHTML("docs_flow.html", graphs); err == nil {
		fmt.Println("Generated docs_flow.html")
	}

	// 10) OpenAPI (минимальный)
	if strings.TrimSpace(*openapiOut) != "" {
		if err := writeOpenAPI(*openapiOut, docs); err != nil {
			fmt.Fprintf(os.Stderr, "openapi: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Generated %s\n", *openapiOut)
	}
}

// ========================= SQL parsing =========================

var (
	reCreateTableStmt = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(IF\s+NOT\s+EXISTS\s+)?("?[\w\.]+"?)\s*$begin:math:text$(.*?)$end:math:text$\s*;`)
	reCreateIndexStmt = regexp.MustCompile(`(?is)CREATE\s+(UNIQUE\s+)?INDEX\s+("?[\w\.]+"?)\s+ON\s+("?[\w\.]+"?)\s*$begin:math:text$([^)]+)$end:math:text$\s*;`)
	reAlterTableStmt  = regexp.MustCompile(`(?is)ALTER\s+TABLE\s+("?[\w\.]+"?)\s+(ADD\s+CONSTRAINT\s+[^;]+|ADD\s+COLUMN\s+[^;]+);`)
	rePkTable         = regexp.MustCompile(`(?is)PRIMARY\s+KEY\s*$begin:math:text$([^)]+)$end:math:text$`)
	reFk              = regexp.MustCompile(`(?is)FOREIGN\s+KEY\s*$begin:math:text$([^)]+)$end:math:text$\s*REFERENCES\s+("?[\w\.]+"?)\s*$begin:math:text$([^)]+)$end:math:text$([^,)]*)`)
	reColLine         = regexp.MustCompile(`(?is)^\s*("?[\w\.]+"?)\s+([^\s,]+)(.*)$`)
	rePkInline        = regexp.MustCompile(`(?is)\bPRIMARY\s+KEY\b`)
)

func parseMigrations(dir string) ([]Table, error) {
	if dir == "" {
		return nil, errors.New("parseMigrations: empty dir")
	}
	schema := map[string]*Table{}

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".sql") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		content := sanitizeSQL(string(b))

		// CREATE TABLE
		for _, m := range reCreateTableStmt.FindAllStringSubmatch(content, -1) {
			name := unq(m[2])
			body := strings.TrimSpace(m[3])
			t := parseCreateTableBody(name, body)
			upsertTable(schema, &t)
		}
		// CREATE INDEX
		for _, m := range reCreateIndexStmt.FindAllStringSubmatch(content, -1) {
			ix := Index{
				Unique:  strings.TrimSpace(m[1]) != "",
				Name:    unq(m[2]),
				Table:   unq(m[3]),
				Columns: normList(m[4]),
			}
			if t := schema[ix.Table]; t != nil {
				t.Indexes = upsertIndex(t.Indexes, ix)
			}
		}
		// ALTER TABLE
		for _, m := range reAlterTableStmt.FindAllStringSubmatch(content, -1) {
			tbl := unq(m[1])
			stmt := strings.TrimSpace(m[2])
			applyAlter(schema, tbl, stmt)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// to slice
	var out []Table
	for _, t := range schema {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func upsertTable(schema map[string]*Table, t *Table) {
	if old, ok := schema[t.Name]; ok {
		schema[t.Name] = mergeTables(old, t)
	} else {
		cp := *t
		schema[t.Name] = &cp
	}
}

func parseCreateTableBody(name, body string) Table {
	t := Table{Name: name}
	parts := splitTopLevelCommas(body)
	for _, raw := range parts {
		p := strings.TrimSpace(raw)
		up := strings.ToUpper(p)

		// table-level PK
		if pm := rePkTable.FindStringSubmatch(p); len(pm) == 2 {
			t.PrimaryKey = upsertCols(t.PrimaryKey, normList(pm[1])...)
			continue
		}
		// table-level FK
		if fm := reFk.FindStringSubmatch(p); len(fm) >= 5 {
			fk := ForeignKey{
				Cols:     normList(fm[1]),
				RefTable: unq(fm[2]),
				RefCols:  normList(fm[3]),
			}
			opts := strings.ToUpper(fm[4])
			if strings.Contains(opts, "ON DELETE") {
				fk.OnDelete = strings.TrimSpace(takeAfter(opts, "ON DELETE"))
			}
			if strings.Contains(opts, "ON UPDATE") {
				fk.OnUpdate = strings.TrimSpace(takeAfter(opts, "ON UPDATE"))
			}
			t.Foreign = append(t.Foreign, fk)
			continue
		}
		// column
		if cm := reColLine.FindStringSubmatch(p); len(cm) >= 4 && !strings.HasPrefix(up, "CONSTRAINT ") {
			col := Column{
				Name: unq(cm[1]),
				Type: strings.TrimSpace(cm[2]),
				Null: !strings.Contains(strings.ToUpper(cm[3]), "NOT NULL"),
			}
			if rePkInline.MatchString(p) { // inline PK
				t.PrimaryKey = upsertCols(t.PrimaryKey, col.Name)
			}
			t.Columns = append(t.Columns, col)
			continue
		}
	}
	return t
}

func applyAlter(schema map[string]*Table, tableName, stmt string) {
	up := strings.ToUpper(stmt)
	t := schema[tableName]
	if t == nil {
		t = &Table{Name: tableName}
		schema[tableName] = t
	}
	if strings.HasPrefix(up, "ADD COLUMN") {
		raw := strings.TrimSpace(stmt[len("ADD COLUMN"):])
		if cm := reColLine.FindStringSubmatch(raw); len(cm) >= 4 {
			col := Column{
				Name: unq(cm[1]),
				Type: strings.TrimSpace(cm[2]),
				Null: !strings.Contains(strings.ToUpper(cm[3]), "NOT NULL"),
			}
			found := false
			for i := range t.Columns {
				if t.Columns[i].Name == col.Name {
					t.Columns[i] = col
					found = true
					break
				}
			}
			if !found {
				t.Columns = append(t.Columns, col)
			}
		}
	}
	if strings.Contains(up, "FOREIGN KEY") {
		if fm := reFk.FindStringSubmatch(stmt); len(fm) >= 5 {
			fk := ForeignKey{
				Cols:     normList(fm[1]),
				RefTable: unq(fm[2]),
				RefCols:  normList(fm[3]),
			}
			opts := strings.ToUpper(fm[4])
			if strings.Contains(opts, "ON DELETE") {
				fk.OnDelete = strings.TrimSpace(takeAfter(opts, "ON DELETE"))
			}
			if strings.Contains(opts, "ON UPDATE") {
				fk.OnUpdate = strings.TrimSpace(takeAfter(opts, "ON UPDATE"))
			}
			t.Foreign = append(t.Foreign, fk)
		}
	}
	if strings.Contains(up, "PRIMARY KEY") {
		if pm := rePkTable.FindStringSubmatch(stmt); len(pm) == 2 {
			t.PrimaryKey = upsertCols(t.PrimaryKey, normList(pm[1])...)
		}
	}
}

func mergeTables(old, cur *Table) *Table {
	for _, c := range cur.Columns {
		found := false
		for i := range old.Columns {
			if old.Columns[i].Name == c.Name {
				old.Columns[i] = c
				found = true
				break
			}
		}
		if !found {
			old.Columns = append(old.Columns, c)
		}
	}
	old.PrimaryKey = upsertCols(old.PrimaryKey, cur.PrimaryKey...)
	old.Foreign = append(old.Foreign, cur.Foreign...)
	old.Indexes = append(old.Indexes, cur.Indexes...)
	return old
}

func upsertCols(dst []string, add ...string) []string {
	m := map[string]bool{}
	for _, d := range dst {
		m[d] = true
	}
	for _, a := range add {
		if !m[a] {
			dst = append(dst, a)
		}
	}
	return dst
}

func upsertIndex(dst []Index, in Index) []Index {
	for i := range dst {
		if dst[i].Name == in.Name {
			dst[i] = in
			return dst
		}
	}
	return append(dst, in)
}

func normList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(strings.Trim(p, `"`))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func unq(s string) string { return strings.Trim(strings.TrimSpace(s), `"`) }

func takeAfter(s, token string) string {
	i := strings.Index(strings.ToUpper(s), strings.ToUpper(token))
	if i < 0 {
		return ""
	}
	return s[i+len(token):]
}

func sanitizeSQL(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "-- +goose") {
			continue
		}
		if idx := strings.Index(ln, "--"); idx >= 0 {
			ln = ln[:idx]
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

func splitTopLevelCommas(s string) []string {
	var out []string
	var lvl int
	var buf strings.Builder
	inStr := false
	var quote rune

	for _, r := range s {
		if inStr {
			buf.WriteRune(r)
			if r == quote {
				inStr = false
			}
			continue
		}
		switch r {
		case '\'', '"', '`':
			inStr = true
			quote = r
			buf.WriteRune(r)
		case '(':
			lvl++
			buf.WriteRune(r)
		case ')':
			lvl--
			buf.WriteRune(r)
		case ',':
			if lvl == 0 {
				out = append(out, strings.TrimSpace(buf.String()))
				buf.Reset()
			} else {
				buf.WriteRune(r)
			}
		default:
			buf.WriteRune(r)
		}
	}
	if s := strings.TrimSpace(buf.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// ========================= AST helpers =========================

func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.StarExpr:
		return "*" + exprString(v.X)
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	case *ast.ArrayType:
		return "[]" + exprString(v.Elt)
	case *ast.MapType:
		return "map[" + exprString(v.Key) + "]" + exprString(v.Value)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.FuncType:
		return "func"
	default:
		return fmt.Sprintf("%T", e)
	}
}

func getTagValue(f *ast.Field, key string) string {
	if f.Tag == nil {
		return ""
	}
	raw := strings.Trim(f.Tag.Value, "`")
	for _, part := range strings.Split(raw, " ") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, key+":") {
			val := strings.TrimPrefix(part, key+":")
			val = strings.Trim(val, `"`)
			return val
		}
	}
	return ""
}

func cleanJSONName(tag, fallback string) string {
	if tag == "" {
		return fallback
	}
	name := strings.Split(tag, ",")[0]
	if name == "" || name == "-" {
		return "-"
	}
	return name
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.Join(strings.Fields(s), " ")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ========================= Collect functions (for call graph) =========================

func collectAllFunctions(root string) map[string]FuncDeclInfo {
	fset := token.NewFileSet()
	funcs := map[string]FuncDeclInfo{}

	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if goSkipVendor && (strings.Contains(path, "/vendor/") || strings.Contains(path, "/.git/")) {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.AllErrors)
		if err != nil {
			return nil
		}
		pkg := file.Name.Name

		ast.Inspect(file, func(n ast.Node) bool {
			fd, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}
			var rec string
			if fd.Recv != nil && len(fd.Recv.List) > 0 {
				rec = exprString(fd.Recv.List[0].Type)
			}
			key := FuncKey{Pkg: pkg, Rec: rec, Nom: fd.Name.Name}
			pos := fset.Position(fd.Pos())

			// собрать "сырьё" вызовов
			var calls []string
			if fd.Body != nil {
				ast.Inspect(fd.Body, func(nn ast.Node) bool {
					ce, ok := nn.(*ast.CallExpr)
					if !ok {
						return true
					}
					switch fn := ce.Fun.(type) {
					case *ast.SelectorExpr:
						calls = append(calls, exprString(fn))
					case *ast.Ident:
						calls = append(calls, fn.Name)
					}
					return true
				})
			}

			funcs[key.String()] = FuncDeclInfo{
				Key:   key,
				File:  pos.Filename,
				Line:  pos.Line,
				Calls: calls,
			}
			return true
		})
		return nil
	})

	return funcs
}

// ========================= Go @doc + Models + Handler meta + inner @doc =========================

func parseGoDocs(root string, funcs map[string]FuncDeclInfo) []DocItem {
	var goFiles []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if goSkipVendor && (d.Name() == "vendor" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			goFiles = append(goFiles, path)
		}
		return nil
	})

	var out []DocItem
	fset := token.NewFileSet()

	for _, gf := range goFiles {
		file, err := parser.ParseFile(fset, gf, nil, parser.ParseComments|parser.AllErrors)
		if err != nil {
			continue
		}

		// file-level comments
		if file.Doc != nil {
			it := makeDocItemFromCommentBlock(file.Doc.Text())
			if it != nil {
				pos := fset.Position(file.Pos())
				it.SourceFile = pos.Filename
				it.SourceLine = pos.Line
				it.GoSymbol = "package " + file.Name.Name
				out = append(out, *it)
			}
		}

		// decls
		for _, d := range file.Decls {
			switch nd := d.(type) {
			case *ast.FuncDecl:
				if nd.Doc == nil && nd.Body == nil {
					continue
				}
				// внешний докблок
				var it *DocItem
				if nd.Doc != nil {
					it = makeDocItemFromCommentBlock(nd.Doc.Text())
				}
				// внутренние @doc.* (в теле)
				if nd.Body != nil && file.Comments != nil {
					for _, cg := range file.Comments {
						if cg.Pos() >= nd.Body.Pos() && cg.End() <= nd.Body.End() {
							if inner := makeDocItemFromCommentBlock(cg.Text()); inner != nil {
								if it == nil {
									// позволяем докам жить только внутри тела
									it = inner
								} else {
									mergeDocItems(it, inner)
								}
							}
						}
					}
				}
				if it == nil {
					continue
				}

				pos := fset.Position(nd.Pos())
				it.SourceFile = pos.Filename
				it.SourceLine = pos.Line
				if nd.Recv != nil && len(nd.Recv.List) > 0 {
					it.GoSymbol = fmt.Sprintf("(%s).%s", exprString(nd.Recv.List[0].Type), nd.Name.Name)
				} else {
					it.GoSymbol = nd.Name.Name
				}

				// Автовытягивание HandlerMeta (BodyParser/Status/JSON)
				meta := inferHandlerMeta(nd)
				if it.RequestType == "" && meta.RequestType != "" {
					it.RequestType = meta.RequestType
				}
				if it.ResponseType == "" && meta.ResponseType != "" {
					it.ResponseType = meta.ResponseType
				}
				if len(it.Statuses) == 0 && len(meta.Statuses) > 0 {
					it.Statuses = meta.Statuses
				}

				// Для endpoint/service/repo — сохраним «сырьё» вызовов
				if strings.EqualFold(it.Section, "endpoint") ||
					strings.EqualFold(it.Section, "service") ||
					strings.EqualFold(it.Section, "repo") {
					if info, ok := funcs[it.GoSymbol]; ok {
						it.Extra["raw.calls"] = strings.Join(info.Calls, ", ")
					}
				}

				out = append(out, *it)

			case *ast.GenDecl:
				// модели (@doc.section:model)
				for _, spec := range nd.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					var it *DocItem
					if nd.Doc != nil {
						it = makeDocItemFromCommentBlock(nd.Doc.Text())
					}
					if it == nil && ts.Doc != nil {
						it = makeDocItemFromCommentBlock(ts.Doc.Text())
					}
					if it != nil && strings.EqualFold(it.Section, "model") {
						pos := fset.Position(nd.Pos())
						m := Model{
							ID:          firstNonEmpty(it.ID, slug(it.Summary), ts.Name.Name),
							Summary:     it.Summary,
							Details:     it.Details,
							Tags:        it.Tags,
							Table:       it.Extra["table"],
							Lifecycle:   it.Extra["lifecycle"],
							Invariants:  it.Extra["invariants"],
							Permissions: it.Extra["permissions"],
							Example:     it.Example,
							SourceFile:  pos.Filename,
							SourceLine:  pos.Line,
							Name:        ts.Name.Name,
						}

						for _, f := range st.Fields.List {
							var fieldName string
							if len(f.Names) > 0 {
								fieldName = f.Names[0].Name
							} else {
								continue
							}
							fType := exprString(f.Type)
							jsonTag := getTagValue(f, "json")
							dbTag := getTagValue(f, "db")
							if dbTag == "" {
								dbTag = getTagValue(f, "sqlc")
							}
							validate := splitCSV(getTagValue(f, "validate"))
							docTags := splitCSV(getTagValue(f, "doc"))
							var comment string
							if f.Doc != nil {
								comment = strings.TrimSpace(f.Doc.Text())
							} else if f.Comment != nil {
								comment = strings.TrimSpace(f.Comment.Text())
							}

							m.Fields = append(m.Fields, ModelField{
								Name:     fieldName,
								Type:     fType,
								JSON:     cleanJSONName(jsonTag, fieldName),
								DB:       dbTag,
								Validate: validate,
								DocTags:  docTags,
								Comment:  oneLine(comment),
							})
						}
						collectedModels = append(collectedModels, m)
					}
				}
			}
		}
	}

	// сортировка
	sort.Slice(out, func(i, j int) bool {
		if out[i].Section == out[j].Section {
			return out[i].ID < out[j].ID
		}
		return out[i].Section < out[j].Section
	})
	return out
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func makeDocItemFromCommentBlock(text string) *DocItem {
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Split(bufio.ScanLines)

	m := map[string]string{}
	var lastKey string
	var buf bytes.Buffer

	flush := func() {
		if lastKey != "" {
			m[lastKey] = strings.TrimSpace(buf.String())
			buf.Reset()
			lastKey = ""
		}
	}

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "@doc.") {
			flush()
			kv := strings.SplitN(strings.TrimPrefix(line, "@doc."), ":", 2)
			key := strings.TrimSpace(kv[0])
			val := ""
			if len(kv) == 2 {
				val = strings.TrimSpace(kv[1])
			}
			lastKey = key
			buf.WriteString(val)
			buf.WriteString("\n")
		} else {
			if lastKey != "" {
				if line == "" {
					flush()
				} else {
					buf.WriteString(line)
					buf.WriteString("\n")
				}
			}
		}
	}
	flush()

	if len(m) == 0 {
		return nil
	}

	it := &DocItem{
		Section:   m["section"],
		ID:        m["id"],
		Summary:   m["summary"],
		Details:   m["details"],
		Example:   m["example"],
		RepoQuery: m["repo.query"],
		ServiceUC: m["service.usecase"],
		Extra:     map[string]string{},
	}
	if t := strings.TrimSpace(m["tags"]); t != "" {
		it.Tags = splitCSV(t)
	}
	if e := strings.TrimSpace(m["errors"]); e != "" {
		it.Errors = splitCSV(e)
	}

	for k, v := range m {
		switch k {
		case "section", "id", "summary", "details", "tags", "errors", "example", "repo.query", "service.usecase":
		default:
			it.Extra[k] = v
		}
	}
	return it
}

func mergeDocItems(dst, src *DocItem) {
	if src.Summary != "" {
		dst.Summary = src.Summary
	}
	if src.Details != "" {
		dst.Details = src.Details
	}
	if src.Example != "" {
		dst.Example = src.Example
	}
	if src.RepoQuery != "" {
		dst.RepoQuery = src.RepoQuery
	}
	if src.ServiceUC != "" {
		dst.ServiceUC = src.ServiceUC
	}
	dst.Tags = append(dst.Tags, src.Tags...)
	dst.Errors = append(dst.Errors, src.Errors...)
	if dst.Extra == nil {
		dst.Extra = map[string]string{}
	}
	for k, v := range src.Extra {
		if strings.TrimSpace(v) == "" {
			continue
		}
		dst.Extra[k] = v
	}
}

// ========================= HandlerMeta =========================

type HandlerMeta struct {
	RequestType  string
	ResponseType string
	Statuses     []int
}

func inferHandlerMeta(f *ast.FuncDecl) HandlerMeta {
	var meta HandlerMeta
	locals := map[string]string{}

	ast.Inspect(f, func(n ast.Node) bool {
		switch nn := n.(type) {
		case *ast.DeclStmt:
			if gd, ok := nn.Decl.(*ast.GenDecl); ok && gd.Tok == token.VAR {
				for _, s := range gd.Specs {
					if vs, ok := s.(*ast.ValueSpec); ok {
						typ := exprString(vs.Type)
						for _, name := range vs.Names {
							locals[name.Name] = typ
						}
					}
				}
			}
		case *ast.AssignStmt:
			if len(nn.Lhs) == 1 && len(nn.Rhs) == 1 {
				lid, ok1 := nn.Lhs[0].(*ast.Ident)
				cl, ok2 := nn.Rhs[0].(*ast.CompositeLit)
				if ok1 && ok2 {
					locals[lid.Name] = exprString(cl.Type)
				}
			}
		case *ast.CallExpr:
			sel, ok := nn.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			// BodyParser
			if sel.Sel.Name == "BodyParser" && len(nn.Args) > 0 {
				if u, ok := nn.Args[0].(*ast.UnaryExpr); ok && u.Op == token.AND {
					if id, ok := u.X.(*ast.Ident); ok {
						if t := locals[id.Name]; t != "" {
							meta.RequestType = t
						}
					}
				}
			}
			// Status
			if sel.Sel.Name == "Status" && len(nn.Args) > 0 {
				if bl, ok := nn.Args[0].(*ast.SelectorExpr); ok {
					if id, ok2 := bl.X.(*ast.Ident); ok2 && id.Name == "fiber" {
						if code := statusNameToCode(bl.Sel.Name); code > 0 {
							meta.Statuses = append(meta.Statuses, code)
						}
					}
				} else if bl2, ok := nn.Args[0].(*ast.BasicLit); ok && bl2.Kind == token.INT {
					if v, err := strconv.Atoi(bl2.Value); err == nil {
						meta.Statuses = append(meta.Statuses, v)
					}
				}
			}
			// JSON
			if sel.Sel.Name == "JSON" && len(nn.Args) > 0 {
				switch a := nn.Args[0].(type) {
				case *ast.Ident:
					if t := locals[a.Name]; t != "" {
						meta.ResponseType = t
					}
				case *ast.CompositeLit:
					meta.ResponseType = exprString(a.Type)
				}
			}
		}
		return true
	})

	// uniq statuses
	uniq := map[int]bool{}
	var out []int
	for _, s := range meta.Statuses {
		if s <= 0 {
			continue
		}
		if !uniq[s] {
			out = append(out, s)
			uniq[s] = true
		}
	}
	meta.Statuses = out
	return meta
}

func statusNameToCode(name string) int {
	switch name {
	case "StatusOK":
		return 200
	case "StatusCreated":
		return 201
	case "StatusNoContent":
		return 204
	case "StatusBadRequest":
		return 400
	case "StatusUnauthorized":
		return 401
	case "StatusForbidden":
		return 403
	case "StatusNotFound":
		return 404
	case "StatusConflict":
		return 409
	case "StatusInternalServerError":
		return 500
	default:
		return 0
	}
}

// ========================= Fiber routes =========================

func parseFiberRoutes(root string) []Route {
	var routes []Route
	fset := token.NewFileSet()

	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		if !strings.Contains(path, "router") && !strings.Contains(path, "route") {
			// грубый фильтр — можно расширить
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.AllErrors)
		if err != nil {
			return nil
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			mName := sel.Sel.Name
			if !map[string]bool{"Get": true, "Post": true, "Put": true, "Delete": true, "Patch": true}[mName] {
				return true
			}
			if len(call.Args) < 2 {
				return true
			}
			// path
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			pathVal := strings.Trim(lit.Value, `"`)

			auth := false
			handlerSym := ""

			switch a := call.Args[1].(type) {
			case *ast.CallExpr:
				if sel2, ok := a.Fun.(*ast.SelectorExpr); ok &&
					strings.Contains(strings.ToLower(exprString(sel2.X)), "middleware") &&
					strings.Contains(strings.ToLower(sel2.Sel.Name), "jwt") {
					auth = true
					if len(a.Args) > 0 {
						handlerSym = exprString(a.Args[0])
					}
				}
			default:
				handlerSym = exprString(a)
			}

			pos := fset.Position(call.Pos())
			routes = append(routes, Route{
				Method:     strings.ToUpper(mName),
				Path:       pathVal,
				Auth:       auth,
				File:       pos.Filename,
				Line:       pos.Line,
				HandlerSym: handlerSym,
			})
			return true
		})
		return nil
	})

	return routes
}

func attachRoutes(docs []DocItem, routes []Route) {
	for i := range docs {
		if !strings.EqualFold(docs[i].Section, "endpoint") {
			continue
		}
		gs := docs[i].GoSymbol // например: (*userHandler).Authenticate
		// match by method name suffix
		name := gs
		if idx := strings.LastIndex(gs, "."); idx >= 0 {
			name = gs[idx+1:]
			name = strings.TrimSuffix(name, ")")
		}
		for _, r := range routes {
			// handlerSym может быть client.GenerateOneLinkHandler — содержится ли имя?
			if strings.Contains(r.HandlerSym, name) {
				docs[i].Route = &r
				// auth из @doc.auth имеет приоритет
				if docs[i].Extra["auth"] == "" && r.Auth {
					docs[i].Extra["auth"] = "required"
				}
				break
			}
		}
	}
}

// ========================= Resolve call graph (flat) =========================

func resolveCallGraph(docs []DocItem, funcs map[string]FuncDeclInfo) {
	// построим быстрый индекс DocItem по GoSymbol и по короткому имени
	bySym := map[string]*DocItem{}
	byShort := map[string]*DocItem{}
	for i := range docs {
		di := &docs[i]
		if di.GoSymbol != "" {
			bySym[di.GoSymbol] = di
			short := di.GoSymbol
			if idx := strings.LastIndex(short, "."); idx >= 0 {
				short = short[idx+1:]
				short = strings.TrimSuffix(short, ")")
			}
			if short != "" {
				byShort[short] = di
			}
		}
	}

	for i := range docs {
		di := &docs[i]
		if !(strings.EqualFold(di.Section, "endpoint") ||
			strings.EqualFold(di.Section, "service") ||
			strings.EqualFold(di.Section, "repo")) {
			continue
		}
		info, ok := funcs[di.GoSymbol]
		if !ok {
			continue
		}
		seen := map[string]bool{}
		for _, raw := range info.Calls {
			name := raw
			if idx := strings.LastIndex(name, "."); idx >= 0 {
				name = name[idx+1:]
			}
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			var target *DocItem
			if t, ok := byShort[name]; ok {
				target = t
			} else if t, ok := bySym[name]; ok {
				target = t
			}
			human := raw
			if target != nil {
				human = fmt.Sprintf("%s → %s", raw, safeTitle(target.ID, target.Summary))
			}
			if !seen[human] {
				di.Calls = append(di.Calls, human)
				seen[human] = true
			}
		}
	}
}

// ========================= Build recursive call graphs =========================

type CallGraph struct {
	Title string   // "auth-authenticate — Аутентификация..."
	ASCII []string // готовые строки ASCII-стека
	Nodes []CGNode // для Mermaid
	Edges []CGEdge
}

type CGNode struct{ ID, Label string }
type CGEdge struct{ From, To, Label string }

func buildCallGraphs(docs []DocItem, funcs map[string]FuncDeclInfo) []CallGraph {
	// индексы
	bySym := map[string]*DocItem{}
	byShort := map[string]*DocItem{}
	for i := range docs {
		di := &docs[i]
		if di.GoSymbol == "" {
			continue
		}
		bySym[di.GoSymbol] = di
		short := di.GoSymbol
		if idx := strings.LastIndex(short, "."); idx >= 0 {
			short = short[idx+1:]
			short = strings.TrimSuffix(short, ")")
		}
		byShort[short] = di
	}

	// вспомогательные
	nextDocByRawCall := func(raw string) *DocItem {
		// raw может быть "uh.userService.SelectUser" -> берём правую часть
		name := raw
		if idx := strings.LastIndex(name, "."); idx >= 0 {
			name = name[idx+1:]
		}
		if d := byShort[name]; d != nil {
			return d
		}
		if d := bySym[name]; d != nil {
			return d
		}
		return nil
	}

	var graphs []CallGraph

	for i := range docs {
		di := &docs[i]
		if !strings.EqualFold(di.Section, "endpoint") {
			continue
		}
		start := di.GoSymbol
		info, ok := funcs[start]
		if !ok {
			continue
		}
		_ = info

		// подготавливаем граф
		var g CallGraph
		title := safeTitle(di.ID, di.Summary)
		if di.Route != nil {
			title = fmt.Sprintf("%s — %s %s", title, di.Route.Method, di.Route.Path)
		}
		g.Title = title

		// узлы/рёбра с автогенерацией id
		nodeID := map[string]string{} // label -> id
		idSeq := 0
		getID := func(label string) string {
			if id, ok := nodeID[label]; ok {
				return id
			}
			idSeq++
			id := fmt.Sprintf("N%d", idSeq)
			nodeID[label] = id
			g.Nodes = append(g.Nodes, CGNode{ID: id, Label: label})
			return id
		}
		addEdge := func(fromLabel, toLabel, lbl string) {
			a := getID(fromLabel)
			b := getID(toLabel)
			g.Edges = append(g.Edges, CGEdge{From: a, To: b, Label: lbl})
		}

		// стартовая подпись
		startLabel := start
		if di.Route != nil {
			startLabel = fmt.Sprintf("%s %s", di.Route.Method, di.Route.Path)
		}
		getID(startLabel)

		// DFS с ограничением глубины и фильтром шума
		type frame struct {
			Sym   string
			Label string
			Depth int
		}
		stack := []frame{{Sym: start, Label: startLabel, Depth: 0}}
		visited := map[string]bool{}

		var ascii []string
		ascii = append(ascii, fmt.Sprintf("[Endpoint] %s", startLabel))

		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]

			if visited[cur.Sym] && cur.Depth > 0 {
				continue
			}
			visited[cur.Sym] = true
			fi, ok := funcs[cur.Sym]
			if !ok {
				continue
			}

			// соберём отсортированные "дети" вызовы
			var children []string
			seenRaw := map[string]bool{}
			for _, raw := range fi.Calls {
				if seenRaw[raw] || isNoiseCall(raw) {
					continue
				}
				seenRaw[raw] = true
				children = append(children, raw)
			}
			sort.Strings(children)

			for j := len(children) - 1; j >= 0; j-- { // в стек в обратном, чтобы идти сверху вниз
				raw := children[j]
				if d := nextDocByRawCall(raw); d != nil {
					lbl := shortFuncName(d.GoSymbol)
					addEdge(cur.Label, lbl, "")
					if cur.Depth < 6 { // ограничение глубины
						stack = append(stack, frame{Sym: d.GoSymbol, Label: lbl, Depth: cur.Depth + 1})
					}
					ascii = append(ascii, fmt.Sprintf("%s→ %s", strings.Repeat("  ", cur.Depth+1), lbl))

					// если repo — добавим SQL как лист
					if strings.EqualFold(d.Section, "repo") && strings.TrimSpace(d.RepoQuery) != "" {
						sqlLabel := "SQL: " + trimSQL(d.RepoQuery, 80)
						addEdge(lbl, sqlLabel, "")
						ascii = append(ascii, fmt.Sprintf("%s→ %s", strings.Repeat("  ", cur.Depth+2), sqlLabel))
					}
				} else {
					// библиотеки (whitelist)
					if isLibInteresting(raw) {
						lbl := raw
						addEdge(cur.Label, lbl, "")
						ascii = append(ascii, fmt.Sprintf("%s→ %s", strings.Repeat("  ", cur.Depth+1), lbl))
					}
				}
			}
		}

		g.ASCII = ascii
		graphs = append(graphs, g)
	}

	return graphs
}

func shortFuncName(goSym string) string {
	if goSym == "" {
		return ""
	}
	s := goSym
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

func trimSQL(q string, n int) string {
	qq := strings.Join(strings.Fields(q), " ")
	if len(qq) <= n {
		return qq
	}
	return qq[:n-1] + "…"
}

func isNoiseCall(raw string) bool {
	r := raw
	// отсекаем шум
	noisePrefixes := []string{
		"log.", "ctx.", "context.", "errors.", "fmt.", "time.", "os.",
		"strconv.", "strings.", "bytes.", "json.", "sql.", "reflect.",
		"rows.", "append", "len", "cap", "make", "copy", "panic", "recover",
	}
	for _, p := range noisePrefixes {
		if strings.HasPrefix(r, p) {
			return true
		}
	}
	// часто в проекте: logger.With(...).Str(...).Msg(...), ctx.Status(...), ctx.JSON(...)
	if strings.Contains(r, ".With") || strings.Contains(r, ".Msg") ||
		strings.Contains(r, ".Status") || strings.Contains(r, ".JSON") || strings.Contains(r, ".BodyParser") {
		return true
	}
	return false
}

func isLibInteresting(raw string) bool {
	// подсветим крипто/джвт/хеш и т.п.
	if strings.HasPrefix(raw, "bcrypt.") ||
		strings.HasPrefix(raw, "jwt.") {
		return true
	}
	return false
}

// ========================= Markdown =========================

func renderMarkdown(tables []Table, docs []DocItem, models []Model, withMermaid bool, graphs []CallGraph) []byte {
	var md bytes.Buffer
	md.WriteString("# Project Documentation (auto)\n\n")

	// TOC
	md.WriteString("- [Endpoints](#endpoints)\n")
	md.WriteString("- [Services & Use cases](#services--use-cases)\n")
	md.WriteString("- [Repositories & Queries](#repositories--queries)\n")
	md.WriteString("- [Models](#models)\n")
	md.WriteString("- [Call Graph](#call-graph)\n")
	md.WriteString("- [Database Schema (from migrations)](#database-schema-from-migrations)\n\n")

	writeDocSection(&md, docs, "endpoint", "## Endpoints", true)
	writeDocSection(&md, docs, "service", "## Services & Use cases", false)
	writeDocSection(&md, docs, "repo", "## Repositories & Queries", false)
	writeDocSection(&md, docs, "domain", "## Domain Notes", false)

	writeModelsSection(&md, models, tables)
	writeCallGraph(&md, graphs)

	// DB schema
	md.WriteString("\n## Database Schema (from migrations)\n\n")
	if len(tables) == 0 {
		md.WriteString("_No tables detected_\n")
	} else {
		for _, t := range tables {
			fmt.Fprintf(&md, "### %s\n\n", t.Name)
			md.WriteString("| Column | Type | Null |\n|---|---|---|\n")
			for _, c := range t.Columns {
				fmt.Fprintf(&md, "| %s | %s | %t |\n", c.Name, c.Type, c.Null)
			}
			if len(t.PrimaryKey) > 0 {
				fmt.Fprintf(&md, "\n**PK:** %s\n", strings.Join(t.PrimaryKey, ", "))
			}
			if len(t.Indexes) > 0 {
				md.WriteString("\n\n**Indexes**\n\n")
				md.WriteString("| Name | Columns | Unique |\n|---|---|---|\n")
				for _, ix := range t.Indexes {
					fmt.Fprintf(&md, "| %s | %s | %t |\n", ix.Name, strings.Join(ix.Columns, ", "), ix.Unique)
				}
			}
			if len(t.Foreign) > 0 {
				md.WriteString("\n\n**Foreign Keys**\n\n")
				md.WriteString("| Columns | Ref | OnDelete | OnUpdate |\n|---|---|---|---|\n")
				for _, fk := range t.Foreign {
					fmt.Fprintf(&md, "| %s | %s(%s) | %s | %s |\n",
						strings.Join(fk.Cols, ", "),
						fk.RefTable, strings.Join(fk.RefCols, ", "),
						nz(fk.OnDelete), nz(fk.OnUpdate),
					)
				}
			}
			md.WriteString("\n\n")
		}
		if withMermaid {
			md.WriteString("#### ER diagram (Mermaid)\n\n```mermaid\nerDiagram\n")
			for _, t := range tables {
				fmt.Fprintf(&md, "  %s {\n", t.Name)
				for _, c := range t.Columns {
					fmt.Fprintf(&md, "    %s %s\n", c.Type, c.Name)
				}
				md.WriteString("  }\n")
			}
			for _, t := range tables {
				for _, fk := range t.Foreign {
					fmt.Fprintf(&md, "  %s }o--|| %s : FK\n", t.Name, fk.RefTable)
				}
			}
			md.WriteString("```\n\n")
		}
	}

	return md.Bytes()
}

func nz(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "-"
	}
	return s
}

func statusBadge(codes []int) string {
	if len(codes) == 0 {
		return "-"
	}
	var parts []string
	for _, c := range codes {
		icon := "✅"
		switch {
		case c >= 500:
			icon = "🟥"
		case c >= 400:
			icon = "🟧"
		case c >= 300:
			icon = "🟨"
		case c >= 200:
			icon = "✅"
		default:
			icon = "⬜️"
		}
		parts = append(parts, fmt.Sprintf("%s %d", icon, c))
	}
	return strings.Join(parts, ", ")
}

func writeDocSection(md *bytes.Buffer, docs []DocItem, want, title string, showRoutes bool) {
	var items []DocItem
	for _, d := range docs {
		if strings.EqualFold(d.Section, want) {
			items = append(items, d)
		}
	}
	if len(items) == 0 {
		return
	}
	md.WriteString(title + "\n\n")
	for idx, d := range items {
		id := d.ID
		if id == "" {
			id = slug(d.Summary)
		}
		fmt.Fprintf(md, "### %s\n\n", safeTitle(id, d.Summary))

		if showRoutes && d.Route != nil {
			auth := ""
			if strings.EqualFold(d.Extra["auth"], "required") || d.Route.Auth {
				auth = " 🔒 _auth required_"
			}
			fmt.Fprintf(md, "`%s %s`%s\n\n", d.Route.Method, d.Route.Path, auth)
		}

		if len(d.Tags) > 0 {
			fmt.Fprintf(md, "_Tags:_ %s\n\n", strings.Join(d.Tags, ", "))
		}
		if d.Summary != "" {
			fmt.Fprintf(md, "**Summary:** %s\n\n", d.Summary)
		}
		if d.Details != "" {
			md.WriteString(d.Details + "\n\n")
		}
		if d.RepoQuery != "" {
			fmt.Fprintf(md, "**Repo query:** `%s`\n\n", d.RepoQuery)
		}
		if d.ServiceUC != "" {
			fmt.Fprintf(md, "**Use case:** `%s`\n\n", d.ServiceUC)
		}

		// Request/Response/Statuses
		if d.RequestType != "" || d.ResponseType != "" || len(d.Statuses) > 0 {
			md.WriteString("| Request | Response | Statuses |\n|---|---|---|\n")
			fmt.Fprintf(md, "| %s | %s | %s |\n\n",
				nz(d.RequestType), nz(d.ResponseType), statusBadge(d.Statuses))
		}

		// curl
		if showRoutes && d.Route != nil {
			body := ""
			if d.RequestType != "" {
				body = ` -H 'Content-Type: application/json' -d '{...}'`
			}
			auth := ""
			if strings.EqualFold(d.Extra["auth"], "required") || d.Route.Auth {
				auth = " -H 'Authorization: Bearer <token>'"
			}
			ex := fmt.Sprintf("curl -X %s http://localhost:8080%s%s%s", d.Route.Method, d.Route.Path, auth, body)
			md.WriteString("<details><summary>Example</summary>\n\n```\n" + ex + "\n```\n\n</details>\n\n")
		} else if d.Example != "" {
			md.WriteString("**Example**\n\n```\n" + d.Example + "\n```\n\n")
		}

		if len(d.Errors) > 0 {
			fmt.Fprintf(md, "**Errors:** %s\n\n", strings.Join(d.Errors, ", "))
		}
		fmt.Fprintf(md, "_src: %s:%d (%s)_\n\n", shortPath(d.SourceFile), d.SourceLine, d.GoSymbol)

		// визуальный разделитель между айтемами
		if idx != len(items)-1 {
			md.WriteString("---\n\n")
		}
	}
}

func writeModelsSection(md *bytes.Buffer, models []Model, tables []Table) {
	if len(models) == 0 {
		return
	}
	md.WriteString("## Models\n\n")

	byTable := map[string]*Table{}
	for i := range tables {
		tt := tables[i]
		byTable[tt.Name] = &tt
	}

	for idx, m := range models {
		title := m.ID
		if m.Summary != "" {
			title += " — " + m.Summary
		}
		fmt.Fprintf(md, "### %s\n\n", title)
		if len(m.Tags) > 0 {
			fmt.Fprintf(md, "_Tags:_ %s\n\n", strings.Join(m.Tags, ", "))
		}
		if m.Details != "" {
			md.WriteString(m.Details + "\n\n")
		}
		if m.Table != "" {
			fmt.Fprintf(md, "**Table:** %s", m.Table)
			if _, ok := byTable[m.Table]; ok {
				md.WriteString("\n\n")
			} else {
				md.WriteString(" _(not found in migrations)_\n\n")
			}
		}
		if m.Lifecycle != "" {
			fmt.Fprintf(md, "**Lifecycle:** %s\n\n", m.Lifecycle)
		}
		if m.Invariants != "" {
			fmt.Fprintf(md, "**Invariants:** %s\n\n", m.Invariants)
		}
		if m.Permissions != "" {
			fmt.Fprintf(md, "**Permissions:** %s\n\n", m.Permissions)
		}

		writeModelTable(md, m)

		if m.Example != "" {
			md.WriteString("**Example**\n\n```\n" + m.Example + "\n```\n\n")
		}
		fmt.Fprintf(md, "_src: %s:%d (type %s)_\n\n", shortPath(m.SourceFile), m.SourceLine, m.Name)

		if idx != len(models)-1 {
			md.WriteString("---\n\n")
		}
	}
}

// динамическая таблица полей — без пустых колонок Constraints/Notes
func writeModelTable(md *bytes.Buffer, m Model) {
	hasDB, hasCons, hasNotes := false, false, false
	for _, f := range m.Fields {
		if nz(f.DB) != "-" {
			hasDB = true
		}
		if strings.TrimSpace(strings.Join(f.Validate, ", ")) != "" || len(f.DocTags) > 0 {
			hasCons = true
		}
		if nz(f.Comment) != "-" {
			hasNotes = true
		}
	}
	cols := []string{"Field", "JSON"}
	if hasDB {
		cols = append(cols, "DB")
	}
	cols = append(cols, "Type")
	if hasCons {
		cols = append(cols, "Constraints")
	}
	if hasNotes {
		cols = append(cols, "Notes")
	}
	md.WriteString("| " + strings.Join(cols, " | ") + " |\n|" + strings.Repeat("---|", len(cols)) + "\n")

	for _, f := range m.Fields {
		row := []string{f.Name, nz(f.JSON)}
		if hasDB {
			row = append(row, nz(f.DB))
		}
		row = append(row, f.Type)
		if hasCons {
			cons := strings.Join(f.Validate, ", ")
			if len(f.DocTags) > 0 {
				if cons != "" {
					cons += ", "
				}
				cons += strings.Join(f.DocTags, ", ")
			}
			row = append(row, nz(cons))
		}
		if hasNotes {
			row = append(row, nz(f.Comment))
		}
		md.WriteString("| " + strings.Join(row, " | ") + " |\n")
	}
	md.WriteString("\n")
}

func writeCallGraph(md *bytes.Buffer, graphs []CallGraph) {
	md.WriteString("## Call Graph\n\n")
	if len(graphs) == 0 {
		md.WriteString("_No calls collected_\n\n")
		return
	}
	for _, g := range graphs {
		renderCallGraphMarkdown(md, g)
	}
}

func shortPath(p string) string {
	p = filepath.ToSlash(p)
	return p
}

func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "-")
	s = regexp.MustCompile(`[^a-z0-9\-]+`).ReplaceAllString(s, "")
	if s == "" {
		s = "item"
	}
	return s
}

func safeTitle(id, summary string) string {
	if summary != "" {
		return fmt.Sprintf("%s — %s", id, summary)
	}
	return id
}

// ========================= OpenAPI (minimal) =========================

func writeOpenAPI(path string, docs []DocItem) error {
	// сгруппируем по пути
	type op struct {
		Method string
		Item   *DocItem
	}
	paths := map[string][]op{}

	for i := range docs {
		di := &docs[i]
		if !strings.EqualFold(di.Section, "endpoint") || di.Route == nil {
			continue
		}
		paths[di.Route.Path] = append(paths[di.Route.Path], op{Method: strings.ToLower(di.Route.Method), Item: di})
	}

	var b strings.Builder
	b.WriteString("openapi: 3.0.3\ninfo:\n  title: Project API\n  version: 1.0.0\npaths:\n")
	for p, ops := range paths {
		fmt.Fprintf(&b, "  %s:\n", p)
		for _, o := range ops {
			sum := o.Item.Summary
			if sum == "" {
				sum = o.Item.ID
			}
			fmt.Fprintf(&b, "    %s:\n      summary: %q\n", o.Method, sum)
			// security
			if strings.EqualFold(o.Item.Extra["auth"], "required") || (o.Item.Route != nil && o.Item.Route.Auth) {
				b.WriteString("      security:\n        - bearerAuth: []\n")
			}
			// responses
			b.WriteString("      responses:\n")
			ok := 200
			for _, s := range o.Item.Statuses {
				if s >= 200 && s < 300 {
					ok = s
					break
				}
			}
			fmt.Fprintf(&b, "        \"%d\": { description: \"OK\" }\n", ok)
			for _, e := range o.Item.Errors {
				ee := strings.TrimSpace(e)
				if ee == "" {
					continue
				}
				fmt.Fprintf(&b, "        \"%s\": { description: \"error\" }\n", ee)
			}
		}
	}
	b.WriteString("components:\n  securitySchemes:\n    bearerAuth:\n      type: http\n      scheme: bearer\n      bearerFormat: JWT\n")

	return os.WriteFile(path, []byte(b.String()), 0644)
}

// ========================= Mermaid render + HTML =========================

func renderCallGraphMarkdown(md *bytes.Buffer, g CallGraph) {
	// ASCII стек
	md.WriteString("### " + g.Title + "\n\n")
	for _, line := range g.ASCII {
		md.WriteString(line + "\n")
	}
	md.WriteString("\n")

	// Mermaid flowchart (рендерится в GitHub/VS Code/Obsidian)
	md.WriteString("```mermaid\n")
	md.WriteString("flowchart TD\n")
	for _, n := range g.Nodes {
		// n.ID — уникальный ID узла (без пробелов), n.Label — подпись
		fmt.Fprintf(md, "  %s[%s]\n", n.ID, escapeMermaid(n.Label))
	}
	for _, e := range g.Edges {
		// e.From, e.To — ID узлов; e.Label — подпись на ребре (можно пусто)
		if e.Label != "" {
			fmt.Fprintf(md, "  %s -->|%s| %s\n", e.From, escapeMermaid(e.Label), e.To)
		} else {
			fmt.Fprintf(md, "  %s --> %s\n", e.From, e.To)
		}
	}
	md.WriteString("```\n\n---\n\n")
}

func escapeMermaid(s string) string {
	// мермейд не любит обратные кавычки и «|»
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "`", "'")
	return s
}

func writeFlowHTML(path string, all []CallGraph) error {
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8">
<title>Project Call Flows</title>
<style>
body{font-family:ui-sans-serif,system-ui,Segoe UI,Roboto,Arial; margin:24px; line-height:1.4;}
h2{margin:24px 0 12px}
hr{margin:24px 0}
.mermaid{margin:16px 0;border:1px solid #eee;border-radius:12px;padding:16px;}
.details{color:#666;font-size:0.9em}
</style>
<script type="module">
import mermaid from "https://cdn.jsdelivr.net/npm/mermaid@10/dist/mermaid.esm.min.mjs";
mermaid.initialize({ startOnLoad: true, securityLevel:"loose", theme: "default" });
</script>
</head><body>
<h1>Project Call Flows</h1>
<p class="details">Автосгенерированные графы от ручек к сервисам/репозиториям/SQL. Откройте этот файл локально в браузере.</p>
`)

	for _, g := range all {
		b.WriteString("<h2>")
		b.WriteString(html.EscapeString(g.Title))
		b.WriteString("</h2>\n<div class=\"mermaid\">\nflowchart TD\n")
		for _, n := range g.Nodes {
			fmt.Fprintf(&b, "  %s[%s]\n", n.ID, escapeMermaid(n.Label))
		}
		for _, e := range g.Edges {
			if e.Label != "" {
				fmt.Fprintf(&b, "  %s -->|%s| %s\n", e.From, escapeMermaid(e.Label), e.To)
			} else {
				fmt.Fprintf(&b, "  %s --> %s\n", e.From, e.To)
			}
		}
		b.WriteString("\n</div>\n<hr/>\n")
	}
	b.WriteString("</body></html>")
	return os.WriteFile(path, []byte(b.String()), 0644)
}
