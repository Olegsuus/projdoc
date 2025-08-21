// cmd/app/main.go
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ---------- CLI flags ----------
var (
	srcDir       = flag.String("src", ".", "Go source root")
	migDir       = flag.String("migrations", "./migrations", "SQL migrations dir")
	outFile      = flag.String("out", "docs.md", "Output markdown file")
	emitMermaid  = flag.Bool("mermaid", true, "Emit Mermaid ER diagram")
	sqlExts      = []string{".sql"}
	goSkipVendor = true
)

// ---------- Data models ----------
type DocItem struct {
	Section string // service|repo|endpoint|domain|model
	ID      string
	Summary string
	Details string
	Tags    []string
	Errors  []string
	Example string

	RepoQuery  string // "@doc.repo.query"
	ServiceUC  string // "@doc.service.usecase"
	SourceFile string
	SourceLine int
	GoSymbol   string // func/receiver/package name

	Extra map[string]string // любые @doc.<key>, которые мы не распарсили в фикс-поля
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

	Fields []ModelField
}

type ModelField struct {
	Name     string // Go имя
	Type     string // Go тип (как строка)
	JSON     string
	DB       string
	Validate []string
	DocTags  []string // из doc:"..."
	Comment  string   // комментарий над полем
}

// глобальный сборщик моделей из parseGoDocs
var collectedModels []Model

// ---------- Main ----------
func main() {
	flag.Parse()

	// 1) Парсим миграции (без БД)
	tables := parseMigrations(*migDir)

	// 2) Парсим Go-комментарии с @doc.* (и модели)
	docs := parseGoDocs(*srcDir)

	// 3) Рендерим Markdown
	md := renderMarkdown(tables, docs, collectedModels, *emitMermaid)

	if err := os.WriteFile(*outFile, md, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *outFile, err)
		os.Exit(1)
	}
	fmt.Printf("Generated %s\n", *outFile)
}

// ---------- SQL migrations parsing (primitive but useful) ----------
func parseMigrations(dir string) []Table {
	var files []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		for _, ext := range sqlExts {
			if strings.HasSuffix(strings.ToLower(d.Name()), ext) {
				files = append(files, path)
				break
			}
		}
		return nil
	})

	var schema = make(map[string]*Table)
	for _, f := range files {
		b, _ := os.ReadFile(f)
		stmts := splitSQLStatements(string(b))

		for _, s := range stmts {
			up := strings.ToUpper(s)
			switch {
			case strings.HasPrefix(strings.TrimSpace(up), "CREATE TABLE"):
				t := parseCreateTable(s)
				if t.Name != "" {
					if _, ok := schema[t.Name]; schema[t.Name] == nil || ok {
						// merge primitive (idempotent for repeated migrations)
						schema[t.Name] = mergeTables(schema[t.Name], &t)
					}
				}
			case strings.HasPrefix(strings.TrimSpace(up), "ALTER TABLE"):
				applyAlter(schema, s)
			case strings.HasPrefix(strings.TrimSpace(up), "CREATE INDEX") ||
				strings.HasPrefix(strings.TrimSpace(up), "CREATE UNIQUE INDEX"):
				idx := parseCreateIndex(s)
				if idx.Table != "" && schema[idx.Table] != nil {
					schema[idx.Table].Indexes = upsertIndex(schema[idx.Table].Indexes, idx)
				}
			}
		}
	}

	// to slice + sort
	var out []Table
	for _, t := range schema {
		sort.Slice(t.Columns, func(i, j int) bool { return t.Columns[i].Name < t.Columns[j].Name })
		sort.Strings(t.PrimaryKey)
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// very rough statement splitter — good enough for typical migrations
func splitSQLStatements(sql string) []string {
	var out []string
	var buf strings.Builder
	inStr := false
	var quote rune

	for _, r := range sql {
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
		case ';':
			stmt := strings.TrimSpace(buf.String())
			if stmt != "" {
				out = append(out, stmt)
			}
			buf.Reset()
		default:
			buf.WriteRune(r)
		}
	}
	if s := strings.TrimSpace(buf.String()); s != "" {
		out = append(out, s)
	}
	return out
}

var (
	reCreateTable = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(IF\s+NOT\s+EXISTS\s+)?("?[\w.]+?"?)\s*$begin:math:text$(.+)$end:math:text$`)
	reColLine     = regexp.MustCompile(`(?is)^\s*("?[\w.]+"?)\s+([^\s,]+)(.*)$`)
	rePkInline    = regexp.MustCompile(`(?is)PRIMARY\s+KEY\s*\(([^)]+)\)`)
	reFk          = regexp.MustCompile(`(?is)CONSTRAINT\s+"?[\w.]+"?\s+FOREIGN\s+KEY\s*\(([^)]+)\)\s*REFERENCES\s+("?[\w.]+"?)\s*\(([^)]+)\)([^,]*)`)
	rePkTable     = regexp.MustCompile(`(?is)CONSTRAINT\s+"?[\w.]+"?\s+PRIMARY\s+KEY\s*\(([^)]+)\)`)
	reCreateIdx   = regexp.MustCompile(`(?is)CREATE\s+(UNIQUE\s+)?INDEX\s+("?[\w.]+"?)\s+ON\s+("?[\w.]+"?)\s*\(([^)]+)\)`)
)

func parseCreateTable(s string) Table {
	m := reCreateTable.FindStringSubmatch(s)
	if len(m) < 3 {
		return Table{}
	}
	name := unq(m[1])
	body := m[2]

	t := Table{Name: name}

	// split by commas at top-level (no parens)
	parts := splitTopLevelCommas(body)
	for _, p := range parts {
		p = strings.TrimSpace(p)
		up := strings.ToUpper(p)

		// table-level PK
		if pm := rePkTable.FindStringSubmatch(p); len(pm) == 2 {
			t.PrimaryKey = normList(pm[1])
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

		// column line
		if cm := reColLine.FindStringSubmatch(p); len(cm) >= 4 && !strings.HasPrefix(up, "CONSTRAINT ") {
			col := Column{
				Name: unq(cm[1]),
				Type: strings.TrimSpace(cm[2]),
				Null: !strings.Contains(strings.ToUpper(cm[3]), "NOT NULL"),
			}
			// inline PK
			if rePkInline.MatchString(p) {
				t.PrimaryKey = append(t.PrimaryKey, col.Name)
			}
			t.Columns = append(t.Columns, col)
			continue
		}
	}

	return t
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

func parseCreateIndex(s string) Index {
	m := reCreateIdx.FindStringSubmatch(s)
	if len(m) < 5 {
		return Index{}
	}
	unique := strings.TrimSpace(m[1]) != ""
	name := unq(m[2])
	table := unq(m[3])
	cols := normList(m[4])
	return Index{Name: name, Table: table, Columns: cols, Unique: unique}
}

func applyAlter(schema map[string]*Table, s string) {
	// простейшие случаи: ADD CONSTRAINT PK/FK, ADD COLUMN
	up := strings.ToUpper(s)
	parts := strings.Fields(up)
	if len(parts) < 3 || parts[0] != "ALTER" || parts[1] != "TABLE" {
		return
	}
	// имя таблицы после ALTER TABLE
	tname := unq(strings.Fields(strings.TrimPrefix(strings.TrimSpace(s), parts[0]+" "+parts[1]))[0])
	t := schema[tname]
	if t == nil {
		t = &Table{Name: tname}
		schema[tname] = t
	}

	// ADD COLUMN
	if strings.Contains(up, "ADD COLUMN") {
		raw := takeAfter(s, "ADD COLUMN")
		line := strings.TrimSpace(raw)
		seg := line
		if i := strings.Index(seg, ","); i > 0 {
			seg = seg[:i]
		}
		if i := strings.Index(seg, ";"); i > 0 {
			seg = seg[:i]
		}
		cm := reColLine.FindStringSubmatch(seg)
		if len(cm) >= 4 {
			col := Column{Name: unq(cm[1]), Type: strings.TrimSpace(cm[2]),
				Null: !strings.Contains(strings.ToUpper(cm[3]), "NOT NULL")}
			// upsert
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

	// ADD CONSTRAINT PRIMARY KEY(...)
	if rePkTable.MatchString(s) {
		pm := rePkTable.FindStringSubmatch(s)
		if len(pm) == 2 {
			t.PrimaryKey = upsertCols(t.PrimaryKey, normList(pm[1])...)
		}
	}

	// ADD CONSTRAINT ... FOREIGN KEY (...)
	if reFk.MatchString(s) {
		fm := reFk.FindStringSubmatch(s)
		if len(fm) >= 5 {
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
}

func mergeTables(old, cur *Table) *Table {
	if old == nil {
		return cur
	}
	// upsert columns
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
		p = strings.TrimSpace(p)
		p = strings.Trim(p, `"`)
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

// ---------- Go @doc.* parsing ----------
func parseGoDocs(root string) []DocItem {
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
		file, err := parser.ParseFile(fset, gf, nil, parser.ParseComments)
		if err != nil {
			continue
		}

		// file-level comments (package doc)
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
				if nd.Doc == nil {
					continue
				}
				it := makeDocItemFromCommentBlock(nd.Doc.Text())
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
				out = append(out, *it)

			case *ast.GenDecl:
				// (1) Докблок над самим объявлением (может описывать пакетные константы/типы)
				if nd.Doc != nil {
					if it := makeDocItemFromCommentBlock(nd.Doc.Text()); it != nil {
						pos := fset.Position(nd.Pos())
						it.SourceFile = pos.Filename
						it.SourceLine = pos.Line
						it.GoSymbol = nd.Tok.String()
						out = append(out, *it)
					}
				}
				// (2) Спецификации в объявлении: интересуют struct-типы с @doc.section:model
				for _, spec := range nd.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					// читаем докблок для конкретного type (если есть)
					var it *DocItem
					if nd.Doc != nil {
						it = makeDocItemFromCommentBlock(nd.Doc.Text())
					}
					// если дока висит не на GenDecl, но на самом TypeSpec — тоже подхватим
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

						// поля структуры
						for _, f := range st.Fields.List {
							// имя поля
							var fieldName string
							if len(f.Names) > 0 {
								fieldName = f.Names[0].Name
							} else {
								// анонимные/embedded можно обработать отдельно (TODO)
								continue
							}
							// тип поля
							fType := exprString(f.Type)

							// теги
							jsonTag := getTagValue(f, "json")
							dbTag := getTagValue(f, "db")
							if dbTag == "" {
								dbTag = getTagValue(f, "sqlc") // на случай sqlc
							}
							validate := splitCSV(getTagValue(f, "validate"))
							docTags := splitCSV(getTagValue(f, "doc"))

							// комментарий над полем
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

	// сортировка для детерминизма
	sort.Slice(out, func(i, j int) bool {
		if out[i].Section == out[j].Section {
			return out[i].ID < out[j].ID
		}
		return out[i].Section < out[j].Section
	})
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
				// часть многострочного значения
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

	// положим все ключи, которые не распознали, в Extra
	for k, v := range m {
		switch k {
		case "section", "id", "summary", "details", "tags", "errors", "example", "repo.query", "service.usecase":
			// пропускаем — уже положили в фиксированные поля
		default:
			it.Extra[k] = v
		}
	}

	return it
}

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
	default:
		return fmt.Sprintf("%T", e)
	}
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

// ---------- Markdown render ----------
func renderMarkdown(tables []Table, docs []DocItem, models []Model, withMermaid bool) []byte {
	var md bytes.Buffer
	md.WriteString("# Project Documentation (auto)\n\n")

	// Sections from @doc
	writeDocSection(&md, docs, "endpoint", "## Endpoints")
	writeDocSection(&md, docs, "service", "## Services & Use cases")
	writeDocSection(&md, docs, "repo", "## Repositories & Queries")
	writeDocSection(&md, docs, "domain", "## Domain Notes")

	// Models
	writeModelsSection(&md, models, tables)

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
					// Mermaid типы условные
					fmt.Fprintf(&md, "    %s %s\n", c.Type, c.Name)
				}
				md.WriteString("  }\n")
			}
			for _, t := range tables {
				for _, fk := range t.Foreign {
					// t ||--o{ fk.RefTable
					fmt.Fprintf(&md, "  %s }o--|| %s : FK\n", t.Name, fk.RefTable)
				}
			}
			md.WriteString("```\n\n")
		}
	}

	return md.Bytes()
}

func writeModelsSection(md *bytes.Buffer, models []Model, tables []Table) {
	if len(models) == 0 {
		return
	}
	md.WriteString("## Models\n\n")

	byTable := map[string]*Table{}
	for i := range tables {
		tt := tables[i] // copy for address
		byTable[tt.Name] = &tt
	}

	for _, m := range models {
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

		md.WriteString("| Field | JSON | DB | Type | Constraints | Notes |\n|---|---|---|---|---|---|\n")
		for _, f := range m.Fields {
			cons := strings.Join(f.Validate, ", ")
			if len(f.DocTags) > 0 {
				if cons != "" {
					cons += ", "
				}
				cons += strings.Join(f.DocTags, ", ")
			}
			fmt.Fprintf(md, "| %s | %s | %s | %s | %s | %s |\n",
				f.Name, nz(f.JSON), nz(f.DB), f.Type, nz(cons), nz(f.Comment),
			)
		}
		md.WriteString("\n")

		if m.Example != "" {
			md.WriteString("**Example**\n\n```\n" + m.Example + "\n```\n\n")
		}
		fmt.Fprintf(md, "_src: %s:%d (type %s)_\n\n", shortPath(m.SourceFile), m.SourceLine, m.Name)
	}
}

func nz(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "-"
	}
	return s
}

func writeDocSection(md *bytes.Buffer, docs []DocItem, want, title string) {
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
	for _, d := range items {
		id := d.ID
		if id == "" {
			id = slug(d.Summary)
		}
		fmt.Fprintf(md, "### %s\n\n", safeTitle(id, d.Summary))
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
		if len(d.Errors) > 0 {
			fmt.Fprintf(md, "**Errors:** %s\n\n", strings.Join(d.Errors, ", "))
		}
		if d.Example != "" {
			md.WriteString("**Example**\n\n```\n" + d.Example + "\n```\n\n")
		}
		fmt.Fprintf(md, "_src: %s:%d (%s)_\n\n", shortPath(d.SourceFile), d.SourceLine, d.GoSymbol)
	}
}

func shortPath(p string) string {
	p = filepath.ToSlash(p)
	if i := strings.Index(p, "/"); i >= 0 {
		return p
	}
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

func getTagValue(f *ast.Field, key string) string {
	if f.Tag == nil {
		return ""
	}
	raw := strings.Trim(f.Tag.Value, "`")
	// простая выборка по ключу
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
