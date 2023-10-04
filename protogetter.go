package protogetter

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"log"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/ast/inspector"
)

type Mode int

const (
	StandaloneMode Mode = iota
	GolangciLintMode
)

const msgFormat = "avoid direct access to proto field %q use %q"

func NewAnalyzer() *analysis.Analyzer {
	return &analysis.Analyzer{
		Name: "protogetter",
		Doc:  "Reports direct reads from proto message fields when getters should be used",
		Run: func(pass *analysis.Pass) (any, error) {
			Run(pass, StandaloneMode)
			return nil, nil
		},
	}
}

func Run(pass *analysis.Pass, mode Mode) []Issue {
	nodeTypes := []ast.Node{
		(*ast.AssignStmt)(nil),
		(*ast.CallExpr)(nil),
		(*ast.SelectorExpr)(nil),
		(*ast.IncDecStmt)(nil),
		(*ast.UnaryExpr)(nil),
	}

	// Skip generated files.
	var files []*ast.File
	for _, f := range pass.Files {
		if !isGeneratedFile(f) {
			files = append(files, f)

			// ast.Print(pass.Fset, f)
		}
	}

	ins := inspector.New(files)

	var issues []Issue

	filter := NewPosFilter()
	ins.Preorder(nodeTypes, func(node ast.Node) {
		report := analyse(pass, filter, node)
		if report == nil {
			return
		}

		switch mode {
		case StandaloneMode:
			pass.Report(report.ToDiagReport())
		case GolangciLintMode:
			issues = append(issues, report.ToIssue(pass.Fset))
		}
	})

	return issues
}

func analyse(pass *analysis.Pass, filter *PosFilter, n ast.Node) *Report {
	// fmt.Printf("\n>>> check: %s\n", formatNode(n))
	// ast.Print(pass.Fset, n)
	if filter.IsFiltered(n.Pos()) {
		// fmt.Printf(">>> filtered\n")
		return nil
	}

	result, err := Process(pass.TypesInfo, filter, n)
	if err != nil {
		pass.Report(analysis.Diagnostic{
			Pos:     n.Pos(),
			End:     n.End(),
			Message: fmt.Sprintf("error: %v", err),
		})

		return nil
	}

	// If existing in filter, skip it.
	if filter.IsFiltered(n.Pos()) {
		return nil
	}

	if result.Skipped() {
		return nil
	}

	// If the expression has already been replaced, skip it.
	if filter.IsAlreadyReplaced(pass.Fset, n.Pos(), n.End()) {
		return nil
	}
	// Add the expression to the filter.
	filter.AddAlreadyReplaced(pass.Fset, n.Pos(), n.End())

	return &Report{
		node:   n,
		result: result,
	}
}

// Issue is used to integrate with golangci-lint's inline auto fix.
type Issue struct {
	Pos       token.Position
	Message   string
	InlineFix InlineFix
}

type InlineFix struct {
	StartCol  int // zero-based
	Length    int
	NewString string
}

type Report struct {
	node   ast.Node
	result *Result
}

func (r *Report) ToDiagReport() analysis.Diagnostic {
	msg := fmt.Sprintf(msgFormat, r.result.From, r.result.To)

	return analysis.Diagnostic{
		Pos:     r.node.Pos(),
		End:     r.node.End(),
		Message: msg,
		SuggestedFixes: []analysis.SuggestedFix{
			{
				Message: msg,
				TextEdits: []analysis.TextEdit{
					{
						Pos:     r.node.Pos(),
						End:     r.node.End(),
						NewText: []byte(r.result.To),
					},
				},
			},
		},
	}
}

func (r *Report) ToIssue(fset *token.FileSet) Issue {
	msg := fmt.Sprintf(msgFormat, r.result.From, r.result.To)
	return Issue{
		Pos:     fset.Position(r.node.Pos()),
		Message: msg,
		InlineFix: InlineFix{
			StartCol:  fset.Position(r.node.Pos()).Column - 1,
			Length:    len(r.result.From),
			NewString: r.result.To,
		},
	}
}

func isGeneratedFile(f *ast.File) bool {
	for _, c := range f.Comments {
		if strings.HasPrefix(c.Text(), "Code generated") {
			return true
		}
	}

	return false
}

func formatNode(node ast.Node) string {
	buf := new(bytes.Buffer)
	if err := format.Node(buf, token.NewFileSet(), node); err != nil {
		log.Printf("Error formatting expression: %v", err)
		return ""
	}

	return buf.String()
}
