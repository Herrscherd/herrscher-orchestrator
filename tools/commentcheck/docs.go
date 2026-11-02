package main

import (
	"go/ast"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

func allowedDocs(file *ast.File) map[*ast.CommentGroup]bool {
	allowed := map[*ast.CommentGroup]bool{file.Doc: true}
	for _, declared := range file.Decls {
		switch top := declared.(type) {
		case *ast.FuncDecl:
			allowed[top.Doc] = top.Name.IsExported() && hasAnExportedReceiver(top)
		case *ast.GenDecl:
			allowed[top.Doc] = slices.ContainsFunc(top.Specs, isExportedSpec)
			for _, spec := range top.Specs {
				allowSpecDocs(spec, allowed)
			}
		}
	}
	delete(allowed, nil)
	return allowed
}

func hasAnExportedReceiver(function *ast.FuncDecl) bool {
	if function.Recv == nil || len(function.Recv.List) == 0 {
		return true
	}
	return isExportedTypeName(function.Recv.List[0].Type)
}

func allowSpecDocs(spec ast.Spec, allowed map[*ast.CommentGroup]bool) {
	switch declared := spec.(type) {
	case *ast.TypeSpec:
		allowed[declared.Doc] = declared.Name.IsExported()
		if declared.Name.IsExported() {
			allowFieldDocs(declared.Type, allowed)
		}
	case *ast.ValueSpec:
		exported := slices.ContainsFunc(declared.Names, (*ast.Ident).IsExported)
		allowed[declared.Doc] = exported
		if exported && declared.Type != nil {
			allowFieldDocs(declared.Type, allowed)
		}
	}
}

func allowFieldDocs(typeExpression ast.Expr, allowed map[*ast.CommentGroup]bool) {
	switch declared := typeExpression.(type) {
	case *ast.StructType:
		allowAccessibleFieldDocs(declared.Fields, allowed)
	case *ast.InterfaceType:
		allowAccessibleFieldDocs(declared.Methods, allowed)
	case *ast.StarExpr:
		allowFieldDocs(declared.X, allowed)
	case *ast.ArrayType:
		allowFieldDocs(declared.Elt, allowed)
	case *ast.MapType:
		allowFieldDocs(declared.Key, allowed)
		allowFieldDocs(declared.Value, allowed)
	case *ast.ChanType:
		allowFieldDocs(declared.Value, allowed)
	}
}

func allowAccessibleFieldDocs(fields *ast.FieldList, allowed map[*ast.CommentGroup]bool) {
	for _, field := range fields.List {
		accessible := isAccessibleField(field)
		allowed[field.Doc] = accessible
		if accessible {
			allowFieldDocs(field.Type, allowed)
		}
	}
}

func isAccessibleField(field *ast.Field) bool {
	if len(field.Names) > 0 {
		return slices.ContainsFunc(field.Names, (*ast.Ident).IsExported)
	}
	return isExportedTypeName(field.Type)
}

func isExportedTypeName(typeExpression ast.Expr) bool {
	for {
		switch unwrapped := typeExpression.(type) {
		case *ast.StarExpr:
			typeExpression = unwrapped.X
		case *ast.IndexExpr:
			typeExpression = unwrapped.X
		case *ast.IndexListExpr:
			typeExpression = unwrapped.X
		case *ast.SelectorExpr:
			return unwrapped.Sel.IsExported()
		case *ast.Ident:
			return unwrapped.IsExported()
		default:
			return false
		}
	}
}

func isExportedSpec(spec ast.Spec) bool {
	switch declared := spec.(type) {
	case *ast.TypeSpec:
		return declared.Name.IsExported()
	case *ast.ValueSpec:
		return slices.ContainsFunc(declared.Names, (*ast.Ident).IsExported)
	}
	return false
}

func exampleDocs(file *ast.File) map[*ast.CommentGroup]bool {
	allowed := make(map[*ast.CommentGroup]bool)
	for _, declared := range file.Decls {
		if example, isFunc := declared.(*ast.FuncDecl); isFunc && isRunnableExample(example) && example.Doc != nil {
			allowed[example.Doc] = true
		}
	}
	return allowed
}

func allowExampleOutputs(file *ast.File, allowed map[*ast.CommentGroup]bool) {
	for _, declared := range file.Decls {
		example, isFunc := declared.(*ast.FuncDecl)
		if !isFunc || !isRunnableExample(example) {
			continue
		}
		if last := lastCommentIn(file, example.Body); last != nil && isExampleOutput(last) {
			allowed[last] = true
		}
	}
}

func isRunnableExample(example *ast.FuncDecl) bool {
	suffix, isExample := strings.CutPrefix(example.Name.Name, "Example")
	if !isExample || example.Recv != nil || example.Body == nil {
		return false
	}
	if example.Type.Params.NumFields() != 0 || example.Type.Results.NumFields() != 0 {
		return false
	}
	first, _ := utf8.DecodeRuneInString(suffix)
	return suffix == "" || first == '_' || unicode.IsUpper(first)
}

func lastCommentIn(file *ast.File, body *ast.BlockStmt) *ast.CommentGroup {
	var last *ast.CommentGroup
	for _, group := range file.Comments {
		if group.Pos() > body.Lbrace && group.End() < body.Rbrace {
			last = group
		}
	}
	return last
}

func isExampleOutput(group *ast.CommentGroup) bool {
	first := strings.TrimSpace(strings.TrimPrefix(group.List[0].Text, "//"))
	return strings.HasPrefix(first, "Output:") || strings.HasPrefix(first, "Unordered output:")
}
