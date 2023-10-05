package protogetter

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/token"
	"go/types"
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

const msgFormat = "avoid direct access to proto field %s, use %s instead"

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
	// Skip generated files.
	var files []*ast.File
	for _, f := range pass.Files {
		if !isGeneratedFile(f) {
			files = append(files, f)
		}
	}
	insp := inspector.New(files)

	var issues []Issue

	nodeTypes := []ast.Node{
		(*ast.AssignStmt)(nil),
		(*ast.IncDecStmt)(nil),
		(*ast.UnaryExpr)(nil),
		(*ast.SelectorExpr)(nil),
	}
	insp.Nodes(nodeTypes, func(node ast.Node, push bool) (dontStop bool) {
		if !push {
			return false
		}

		switch n := node.(type) {
		case *ast.AssignStmt:
			for _, l := range n.Lhs {
				if _, ok := l.(*ast.SelectorExpr); ok {
					return false // t.Embedded.Embedded.S = "1"
				}
			}
			return true // _ = t.Embedded.Embedded

		case *ast.IncDecStmt:
			return false // t.I32++

		case *ast.UnaryExpr:
			return n.Op != token.AND // &t.S
		}

		report := analyzeSelectorExpr(pass, node.(*ast.SelectorExpr))
		if report == nil {
			return true
		}

		switch mode {
		case StandaloneMode:
			pass.Report(report.ToAnalysisDiagnostic())
		case GolangciLintMode:
			issues = append(issues, report.ToIssue(pass.Fset))
		}
		return true
	})

	return issues
}

func analyzeSelectorExpr(pass *analysis.Pass, se *ast.SelectorExpr) *Report {
	if !isProtoMessage(pass.TypesInfo, se.X) {
		return nil
	}

	if se.Sel == nil || strings.HasPrefix(se.Sel.Name, "Get") {
		return nil
	}
	if methodExists(pass.TypesInfo, se.X, "Get"+se.Sel.Name) {
		return &Report{
			Range: se,
			From:  formatNode(pass.Fset, se),
			To:    formatNode(pass.Fset, se.X) + ".Get" + se.Sel.Name + "()",
			SelectorEdit: Edit{
				Range: se.Sel,
				From:  se.Sel.Name,
				To:    "Get" + se.Sel.Name + "()",
			},
		}
	}

	return nil
}

func isGeneratedFile(f *ast.File) bool {
	for _, c := range f.Comments {
		if strings.HasPrefix(c.Text(), "Code generated") {
			return true
		}
	}

	return false
}

func isProtoMessage(info *types.Info, expr ast.Expr) bool {
	// First, we are checking for the presence of the ProtoReflect method which is currently being generated
	// and corresponds to v2 version.
	// https://pkg.go.dev/google.golang.org/protobuf@v1.31.0/proto#Message
	const protoV2Method = "ProtoReflect"
	ok := methodExists(info, expr, protoV2Method)
	if ok {
		return true
	}

	// Afterwards, we are checking the ProtoMessage method. All the structures that implement the proto.Message interface
	// have a ProtoMessage method and are proto-structures. This interface has been generated since version 1.0.0 and
	// continues to exist for compatibility.
	// https://pkg.go.dev/github.com/golang/protobuf/proto?utm_source=godoc#Message
	const protoV1Method = "ProtoMessage"
	ok = methodExists(info, expr, protoV1Method)
	if ok {
		// Since there is a protoc-gen-gogo generator that implements the proto.Message interface, but may not generate
		// getters or generate from without checking for nil, so even if getters exist, we skip them.
		const protocGenGoGoMethod = "MarshalToSizedBuffer"
		return !methodExists(info, expr, protocGenGoGoMethod)
	}

	return false
}

func methodExists(info *types.Info, x ast.Expr, name string) bool {
	if info == nil {
		return false
	}

	t := info.TypeOf(x)
	if t == nil {
		return false
	}

	ptr, ok := t.Underlying().(*types.Pointer)
	if ok {
		t = ptr.Elem()
	}

	named, ok := t.(*types.Named)
	if !ok {
		return false
	}

	for i := 0; i < named.NumMethods(); i++ {
		if named.Method(i).Name() == name {
			return true
		}
	}

	return false
}

func formatNode(fset *token.FileSet, node ast.Node) string {
	buf := new(bytes.Buffer)
	if err := format.Node(buf, fset, node); err != nil {
		log.Printf("Error formatting expression: %v\n", err)
		return ""
	}

	return buf.String()
}
