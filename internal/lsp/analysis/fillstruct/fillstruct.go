// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package fillstruct defines an Analyzer that automatically
// fills in a struct declaration with zero value elements for each field.
package fillstruct

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/internal/analysisinternal"
)

const Doc = `suggested input for incomplete struct initializations

This analyzer provides the appropriate zero values for all
uninitialized fields of an empty struct. For example, given the following struct:
	type Foo struct {
		ID   int64
		Name string
	}
the initialization
	var _ = Foo{}
will turn into
	var _ = Foo{
		ID: 0,
		Name: "",
	}
`

var Analyzer = &analysis.Analyzer{
	Name:             "fillstruct",
	Doc:              Doc,
	Requires:         []*analysis.Analyzer{inspect.Analyzer},
	Run:              run,
	RunDespiteErrors: true,
}

func run(pass *analysis.Pass) (interface{}, error) {
	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	nodeFilter := []ast.Node{(*ast.CompositeLit)(nil)}
	inspect.Preorder(nodeFilter, func(n ast.Node) {
		info := pass.TypesInfo
		if info == nil {
			return
		}
		expr := n.(*ast.CompositeLit)

		// TODO: Handle partially-filled structs as well.
		if len(expr.Elts) != 0 {
			return
		}

		var file *ast.File
		for _, f := range pass.Files {
			if f.Pos() <= expr.Pos() && expr.Pos() <= f.End() {
				file = f
				break
			}
		}
		if file == nil {
			return
		}

		typ := info.TypeOf(expr)
		if typ == nil {
			return
		}

		// Find reference to the type declaration of the struct being initialized.
		for {
			p, ok := typ.Underlying().(*types.Pointer)
			if !ok {
				break
			}
			typ = p.Elem()
		}
		typ = typ.Underlying()

		obj, ok := typ.(*types.Struct)
		if !ok {
			return
		}
		fieldCount := obj.NumFields()
		// Skip any struct that is already populated or that has no fields.
		if fieldCount == 0 || fieldCount == len(expr.Elts) {
			return
		}

		// Don't mutate the existing token.File. Instead, create a copy that we can use to modify
		// position information.
		original := pass.Fset.File(expr.Lbrace)
		fset := token.NewFileSet()
		tok := fset.AddFile(original.Name(), -1, original.Size())

		pos := token.Pos(1)
		var elts []ast.Expr
		for i := 0; i < fieldCount; i++ {
			field := obj.Field(i)
			// Ignore fields that are not accessible in the current package.
			if field.Pkg() != nil && field.Pkg() != pass.Pkg && !field.Exported() {
				continue
			}

			value := populateValue(pass.Fset, file, pass.Pkg, field.Type())
			if value == nil {
				continue
			}
			pos = nextLinePos(tok, pos)
			kv := &ast.KeyValueExpr{
				Key: &ast.Ident{
					NamePos: pos,
					Name:    field.Name(),
				},
				Colon: pos,
				Value: value, // 'value' has no position. fomat.Node corrects for AST nodes with no position.
			}
			elts = append(elts, kv)
		}

		// If all of the struct's fields are unexported, we have nothing to do.
		if len(elts) == 0 {
			return
		}

		cl := ast.CompositeLit{
			Type:   expr.Type, // Don't adjust the expr.Type's position.
			Lbrace: token.Pos(1),
			Elts:   elts,
			Rbrace: nextLinePos(tok, elts[len(elts)-1].Pos()),
		}

		var buf bytes.Buffer
		if err := format.Node(&buf, fset, &cl); err != nil {
			return
		}

		msg := "Fill struct"
		if name, ok := expr.Type.(*ast.Ident); ok {
			msg = fmt.Sprintf("Fill %s", name)
		}

		pass.Report(analysis.Diagnostic{
			Pos: expr.Lbrace,
			End: expr.Rbrace,
			SuggestedFixes: []analysis.SuggestedFix{{
				Message: msg,
				TextEdits: []analysis.TextEdit{{
					Pos:     expr.Pos(),
					End:     expr.End(),
					NewText: buf.Bytes(),
				}},
			}},
		})
	})
	return nil, nil
}

func nextLinePos(tok *token.File, pos token.Pos) token.Pos {
	line := tok.Line(pos)
	if line+1 > tok.LineCount() {
		tok.AddLine(tok.Offset(pos) + 1)
	}
	return tok.LineStart(line + 1)
}

// populateValue constructs an expression to fill the value of a struct field.
//
// When the type of a struct field is a basic literal or interface, we return
// default values. For other types, such as maps, slices, and channels, we create
// expressions rather than using default values.
//
// The reasoning here is that users will call fillstruct with the intention of
// initializing the struct, in which case setting these fields to nil has no effect.
func populateValue(fset *token.FileSet, f *ast.File, pkg *types.Package, typ types.Type) ast.Expr {
	under := typ
	if n, ok := typ.(*types.Named); ok {
		under = n.Underlying()
	}
	switch u := under.(type) {
	case *types.Basic:
		switch {
		case u.Info()&types.IsNumeric != 0:
			return &ast.BasicLit{Kind: token.INT, Value: "0"}
		case u.Info()&types.IsBoolean != 0:
			return &ast.Ident{Name: "false"}
		case u.Info()&types.IsString != 0:
			return &ast.BasicLit{Kind: token.STRING, Value: `""`}
		default:
			panic("unknown basic type")
		}
	case *types.Map:
		k := analysisinternal.TypeExpr(fset, f, pkg, u.Key())
		v := analysisinternal.TypeExpr(fset, f, pkg, u.Elem())
		if k == nil || v == nil {
			return nil
		}
		return &ast.CompositeLit{
			Type: &ast.MapType{
				Key:   k,
				Value: v,
			},
		}
	case *types.Slice:
		s := analysisinternal.TypeExpr(fset, f, pkg, u.Elem())
		if s == nil {
			return nil
		}
		return &ast.CompositeLit{
			Type: &ast.ArrayType{
				Elt: s,
			},
		}
	case *types.Array:
		a := analysisinternal.TypeExpr(fset, f, pkg, u.Elem())
		if a == nil {
			return nil
		}
		return &ast.CompositeLit{
			Type: &ast.ArrayType{
				Elt: a,
				Len: &ast.BasicLit{
					Kind: token.INT, Value: fmt.Sprintf("%v", u.Len())},
			},
		}
	case *types.Chan:
		v := analysisinternal.TypeExpr(fset, f, pkg, u.Elem())
		if v == nil {
			return nil
		}
		dir := ast.ChanDir(u.Dir())
		if u.Dir() == types.SendRecv {
			dir = ast.SEND | ast.RECV
		}
		return &ast.CallExpr{
			Fun: ast.NewIdent("make"),
			Args: []ast.Expr{
				&ast.ChanType{
					Dir:   dir,
					Value: v,
				},
			},
		}
	case *types.Struct:
		s := analysisinternal.TypeExpr(fset, f, pkg, typ)
		if s == nil {
			return nil
		}
		return &ast.CompositeLit{
			Type: s,
		}
	case *types.Signature:
		var params []*ast.Field
		for i := 0; i < u.Params().Len(); i++ {
			p := analysisinternal.TypeExpr(fset, f, pkg, u.Params().At(i).Type())
			if p == nil {
				return nil
			}
			params = append(params, &ast.Field{
				Type: p,
				Names: []*ast.Ident{
					{
						Name: u.Params().At(i).Name(),
					},
				},
			})
		}
		var returns []*ast.Field
		for i := 0; i < u.Results().Len(); i++ {
			r := analysisinternal.TypeExpr(fset, f, pkg, u.Results().At(i).Type())
			if r == nil {
				return nil
			}
			returns = append(returns, &ast.Field{
				Type: r,
			})
		}
		return &ast.FuncLit{
			Type: &ast.FuncType{
				Params: &ast.FieldList{
					List: params,
				},
				Results: &ast.FieldList{
					List: returns,
				},
			},
			Body: &ast.BlockStmt{},
		}
	case *types.Pointer:
		return &ast.UnaryExpr{
			Op: token.AND,
			X:  populateValue(fset, f, pkg, u.Elem()),
		}
	case *types.Interface:
		return ast.NewIdent("nil")
	}
	return nil
}