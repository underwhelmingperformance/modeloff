package protocol_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

func TestCommand_dispatcher_covers_the_sealed_sum(t *testing.T) {
	t.Parallel()

	sealed := methodReceiverNames(t, "commands.go", "isCommand")
	dispatched := typeSwitchSelectorNames(t, "../session/handler.go", "dispatchCommand", "protocol")

	require.Equal(t, sealed, dispatched)
}

func TestMsgTarget_consumers_cover_the_sealed_sum(t *testing.T) {
	t.Parallel()

	sealed := methodReceiverNames(t, "target.go", "isMsgTarget")
	consumers := map[string][]string{
		"WindowName":            typeSwitchSelectorNames(t, "target.go", "WindowName", ""),
		"resolveMsgTarget":      typeSwitchSelectorNames(t, "../session/handler.go", "resolveMsgTarget", "protocol"),
		"toolAvailableInWindow": typeSwitchSelectorNames(t, "../modelclient/api.go", "toolAvailableInWindow", "protocol"),
	}

	require.Equal(t, map[string][]string{
		"WindowName":            sealed,
		"resolveMsgTarget":      sealed,
		"toolAvailableInWindow": sealed,
	}, consumers)
}

func parseFile(t *testing.T, name string) *ast.File {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
	require.NoError(t, err)

	return file
}

func methodReceiverNames(t *testing.T, name, method string) []string {
	t.Helper()

	var names []string
	for _, declaration := range parseFile(t, name).Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != method || function.Recv == nil || len(function.Recv.List) != 1 {
			continue
		}

		receiver, ok := typeName(function.Recv.List[0].Type)
		require.True(t, ok, "receiver for %s must be a named type", method)
		names = append(names, receiver)
	}
	sort.Strings(names)

	return names
}

func typeName(expression ast.Expr) (string, bool) {
	switch expression := expression.(type) {
	case *ast.Ident:
		return expression.Name, true
	case *ast.StarExpr:
		return typeName(expression.X)
	}

	return "", false
}

func typeSwitchSelectorNames(
	t *testing.T,
	name, functionName, packageName string,
) []string {
	t.Helper()

	function := functionDeclaration(t, name, functionName)
	var names []string
	ast.Inspect(function.Body, func(node ast.Node) bool {
		typeSwitch, ok := node.(*ast.TypeSwitchStmt)
		if !ok {
			return true
		}

		names = append(names, typeSwitchCaseNames(typeSwitch, packageName)...)
		return false
	})
	sort.Strings(names)

	return names
}

func functionDeclaration(t *testing.T, name, functionName string) *ast.FuncDecl {
	t.Helper()

	for _, declaration := range parseFile(t, name).Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == functionName {
			return function
		}
	}

	require.FailNow(t, "function declaration not found", functionName)
	return nil
}

func typeSwitchCaseNames(typeSwitch *ast.TypeSwitchStmt, packageName string) []string {
	var names []string
	for _, clauseNode := range typeSwitch.Body.List {
		clause := clauseNode.(*ast.CaseClause)
		for _, expression := range clause.List {
			if name, ok := typeCaseName(expression, packageName); ok {
				names = append(names, name)
			}
		}
	}

	return names
}

func typeCaseName(expression ast.Expr, packageName string) (string, bool) {
	identifier, ok := expression.(*ast.Ident)
	if ok && packageName == "" {
		return identifier.Name, true
	}

	selector, ok := expression.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}

	qualifier, ok := selector.X.(*ast.Ident)
	if !ok || qualifier.Name != packageName {
		return "", false
	}

	return selector.Sel.Name, true
}

func TestEvent_sum_does_not_expose_mutable_actor_handles(t *testing.T) {
	t.Parallel()

	members := methodReceiverNames(t, "../domain/protocol_events.go", "isProtocolEvent")
	typeDeclarations := make(map[string]ast.Expr)
	paths, err := filepath.Glob("../domain/*.go")
	require.NoError(t, err)
	for _, path := range paths {
		for _, declaration := range parseFile(t, path).Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.TYPE {
				continue
			}
			for _, specification := range general.Specs {
				typeSpec := specification.(*ast.TypeSpec)
				typeDeclarations[typeSpec.Name.Name] = typeSpec.Type
			}
		}
	}

	var missing []string
	var mutable []string
	for _, member := range members {
		declaration, ok := typeDeclarations[member]
		if !ok {
			missing = append(missing, member)
			continue
		}
		ast.Inspect(declaration, func(node ast.Node) bool {
			pointer, ok := node.(*ast.StarExpr)
			if !ok {
				return true
			}
			name, ok := typeName(pointer.X)
			if ok && name == "Instance" {
				mutable = append(mutable, member)
			}

			return true
		})
	}
	sort.Strings(missing)
	sort.Strings(mutable)

	type assertionSnapshot struct {
		MissingDeclarations []string
		MutableActorHandles []string
	}

	require.Equal(t, assertionSnapshot{}, assertionSnapshot{
		MissingDeclarations: missing,
		MutableActorHandles: mutable,
	})
}

// TestNotOperatorError_rendering pins the text each shape of the
// refusal renders. The error is typed, so a caller branches on it with
// `errors.As`; what this covers is what an operator reads when one is
// shown to them.
func TestNotOperatorError_rendering(t *testing.T) {
	t.Parallel()

	type rendering struct {
		Message string
	}

	type testCase struct {
		err  protocol.NotOperatorError
		want rendering
	}

	cases := map[string]testCase{
		"with command": {
			err:  protocol.NotOperatorError{Command: "AddModel"},
			want: rendering{Message: "permission denied: AddModel requires operator privileges"},
		},
		"without command": {
			err:  protocol.NotOperatorError{},
			want: rendering{Message: "permission denied: not an operator"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, rendering{Message: tc.err.Error()})
		})
	}
}

func TestModeOperator_value(t *testing.T) {
	t.Parallel()

	require.Equal(t, domain.Mode('o'), domain.ModeOperator)
}
