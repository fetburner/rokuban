package main

import (
	"go/ast"
	"go/token"
	"strconv"
)

// callsGo は os/exec の Command と CommandContext にリテラルの "go" を渡す形を検査する。
// 検査はリテラルを前提にするので、exec.Command(goBin, …) のような変数経由は見落とす。
func callsGo(file *ast.File) bool {
	for _, spec := range file.Imports {
		path, _ := strconv.Unquote(spec.Path.Value)
		if path != "os/exec" {
			continue
		}
		name := "exec"
		if spec.Name != nil {
			name = spec.Name.Name
		}
		for node := range ast.Preorder(file) {
			if call, ok := node.(*ast.CallExpr); ok && callsGoCommand(call, name) {
				return true
			}
		}
	}
	return false
}

func callsGoCommand(call *ast.CallExpr, execName string) bool {
	var method string
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		if pkg, ok := fun.X.(*ast.Ident); ok && pkg.Name == execName {
			method = fun.Sel.Name
		}
	case *ast.Ident:
		if execName == "." {
			method = fun.Name
		}
	}

	var index int
	switch method {
	case "Command":
	case "CommandContext":
		index = 1 // context.Context の次がコマンド名。
	default:
		return false
	}
	if len(call.Args) <= index {
		return false
	}
	arg, ok := ast.Unparen(call.Args[index]).(*ast.BasicLit)
	if !ok || arg.Kind != token.STRING {
		return false
	}
	value, _ := strconv.Unquote(arg.Value)
	return value == "go"
}
