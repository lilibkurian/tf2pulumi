package nodejs

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/hashicorp/hil/ast"
	"github.com/hashicorp/terraform/config"
	"github.com/pkg/errors"
	"github.com/pulumi/pulumi/pkg/util/contract"
	"github.com/pulumi/pulumi-terraform/pkg/tfbridge"

	"github.com/pgavlin/firewalker/il"
)

// We translate from HIL to Typescript in several passes as necessitated by the semantics of `pulumi.Output<T>`:
// - In the first pass, we generate a type-annotated version of the HIL AST and gather all nested references to
//   properties of type `pulumi.Output<T>`.
// - In the second pass, we transform the AST for `pulumi.Output<T>.apply`:
//    - If there are no nested output references, we skip this transform
//    - Otherwise, all referenced outputs are folded into a top-level apply and their references are replaced with
//      the corresponding resolved value

type boundType uint32

const (
	typeInvalid boundType = 0
	typeBool    boundType = 1
	typeString  boundType = 1 << 1
	typeNumber  boundType = 1 << 2
	typeUnknown boundType = 1 << 3

	typeList   boundType = 1 << 4
	typeMap    boundType = 1 << 5
	typeOutput boundType = 1 << 6

	elementTypeMask boundType = typeBool | typeString | typeNumber | typeUnknown
)

func (t boundType) isList() bool {
	return t & typeList != 0
}

func (t boundType) listOf() boundType {
	return t | typeList
}

func (t boundType) isMap() bool {
	return t & typeMap != 0
}

func (t boundType) mapOf() boundType {
	return t | typeMap
}

func (t boundType) isOutput() bool {
	return t & typeOutput != 0
}

func (t boundType) outputOf() boundType {
	return t | typeOutput
}

func (t boundType) elementType() boundType {
	return t & elementTypeMask
}

type boundNode interface {
	typ() boundType
}

type boundArithmetic struct {
	hilNode *ast.Arithmetic

	exprs []boundNode
}

func (n *boundArithmetic) typ() boundType {
	return typeNumber
}

type boundCall struct {
	hilNode *ast.Call
	exprType boundType

	args []boundNode
}

func (n *boundCall) typ() boundType {
	return n.exprType
}

type boundConditional struct {
	hilNode *ast.Conditional
	exprType boundType

	condExpr boundNode
	trueExpr boundNode
	falseExpr boundNode
}

func (n *boundConditional) typ() boundType {
	return n.exprType
}

type boundIndex struct {
	hilNode *ast.Index
	exprType boundType

	targetExpr boundNode
	keyExpr boundNode
}

func (n *boundIndex) typ() boundType {
	return n.exprType
}

type boundLiteral struct {
	exprType boundType
	value    interface{}
}

func (n *boundLiteral) typ() boundType {
	return n.exprType
}

type boundOutput struct {
	hilNode *ast.Output

	exprs []boundNode
}

func (n *boundOutput) typ() boundType {
	return typeString
}

type boundVariableAccess struct {
	hilNode *ast.VariableAccess
	elements []string
	exprType boundType

	tfVar config.InterpolatedVariable
	ilNode il.Node
}

func (n *boundVariableAccess) typ() boundType {
	return n.exprType
}

type hilBinder struct {
	graph *il.Graph
	hasCountIndex bool
}

func (b *hilBinder) bindArithmetic(n *ast.Arithmetic) (boundNode, error) {
	exprs, err := b.bindExprs(n.Exprs)
	if err != nil {
		return nil, err
	}

	return &boundArithmetic{hilNode: n, exprs: exprs}, nil
}

func (b *hilBinder) bindCall(n *ast.Call) (boundNode, error) {
	args, err := b.bindExprs(n.Args)
	if err != nil {
		return nil, err
	}

	exprType := typeUnknown
	switch n.Func {
	case "element", "lookup":
		// nothing to do
	case "file":
		exprType = typeString
	case "split":
		exprType = typeList
	default:
		return nil, errors.Errorf("NYI: call to %s", n.Func)
	}

	return &boundCall{hilNode: n, exprType: exprType, args: args}, nil
}

func (b *hilBinder) bindConditional(n *ast.Conditional) (boundNode, error) {
	condExpr, err := b.bindExpr(n.CondExpr)
	if err != nil {
		return nil, err
	}
	trueExpr, err := b.bindExpr(n.TrueExpr)
	if err != nil {
		return nil, err
	}
	falseExpr, err := b.bindExpr(n.FalseExpr)
	if err != nil {
		return nil, err
	}

	exprType := trueExpr.typ()
	if exprType != falseExpr.typ() {
		exprType = typeUnknown
	}

	return &boundConditional{
		hilNode: n,
		exprType: exprType,
		condExpr: condExpr,
		trueExpr: trueExpr,
		falseExpr: falseExpr,
	}, nil
}

func (b *hilBinder) bindIndex(n *ast.Index) (boundNode, error) {
	boundTarget, err := b.bindExpr(n.Target)
	if err != nil {
		return nil, err
	}
	boundKey, err := b.bindExpr(n.Key)
	if err != nil {
		return nil, err
	}

	exprType := typeUnknown
	targetType := boundTarget.typ()
	if targetType.isList() {
		exprType = targetType.elementType()
	}

	return &boundIndex{
		hilNode: n,
		exprType: exprType,
		targetExpr: boundTarget,
		keyExpr: boundKey,
	}, nil
}

func (b *hilBinder) bindLiteral(n *ast.LiteralNode) (boundNode, error) {
	exprType := typeUnknown
	switch n.Typex {
	case ast.TypeBool:
		exprType = typeBool
	case ast.TypeInt, ast.TypeFloat:
		exprType = typeNumber
	case ast.TypeString:
		exprType = typeString
	default:
		return nil, errors.Errorf("Unexpected literal type %v", n.Typex)
	}

	return &boundLiteral{exprType: exprType, value: n.Value}, nil
}

func (b *hilBinder) bindOutput(n *ast.Output) (boundNode, error) {
	exprs, err := b.bindExprs(n.Exprs)
	if err != nil {
		return nil, err
	}

	// Project a single-element output to the element itself.
	if len(exprs) == 1 {
		return exprs[0], nil
	}

	return &boundOutput{hilNode: n, exprs: exprs}, nil
}

func (b *hilBinder) bindVariableAccess(n *ast.VariableAccess) (boundNode, error) {
	tfVar, err := config.NewInterpolatedVariable(n.Name)
	if err != nil {
		return nil, err
	}

	elements, exprType, ilNode := []string(nil), typeUnknown, il.Node(nil)
	switch v := tfVar.(type) {
	case *config.CountVariable:
		// "count."
		if v.Type != config.CountValueIndex {
			return nil, errors.Errorf("unsupported count variable %s", v.FullKey())
		}

		if !b.hasCountIndex {
			return nil, errors.Errorf("no count index in scope")
		}

		exprType = typeNumber
	case *config.LocalVariable:
		// "local."
		return nil, errors.New("NYI: local variables")
	case *config.ModuleVariable:
		// "module."
		return nil, errors.New("NYI: module variables")
	case *config.PathVariable:
		// "path."
		return nil, errors.New("NYI: path variables")
	case *config.ResourceVariable:
		// default

		// look up the resource.
		r, ok := b.graph.Resources[v.ResourceId()]
		if !ok {
			return nil, errors.Errorf("unknown resource %v", v.ResourceId())
		}
		ilNode = r

		var resInfo *tfbridge.ResourceInfo
		var sch schemas
		if r.Provider.Info != nil {
			resInfo = r.Provider.Info.Resources[v.Type]
			sch.tfRes = r.Provider.Info.P.ResourcesMap[v.Type]
			sch.pulumi = &tfbridge.SchemaInfo{Fields: resInfo.Fields}
		}

		// name{.property}+
		elements = strings.Split(v.Field, ".")
		for _, e := range elements {
			sch = sch.propertySchemas(e)
		}

		// Handle multi-references (splats and indexes)
		exprType = sch.boundType()
		if v.Multi && v.Index == -1 {
			exprType = exprType.listOf()
		}
	case *config.SelfVariable:
		// "self."
		return nil, errors.New("NYI: self variables")
	case *config.SimpleVariable:
		// "[^.]\+"
		return nil, errors.New("NYI: simple variables")
	case *config.TerraformVariable:
		// "terraform."
		return nil, errors.New("NYI: terraform variables")
	case *config.UserVariable:
		// "var."
		if v.Elem != "" {
			return nil, errors.New("NYI: user variable elements")
		}

		// look up the variable.
		vn, ok := b.graph.Variables[v.Name]
		if !ok {
			return nil, errors.Errorf("unknown variable %s", v.Name)
		}
		ilNode = vn

		// If the variable does not have a default, its type is string. If it does have a default, its type is string
		// iff the default's type is also string. Note that we don't try all that hard here.
		exprType = typeString
		if vn.DefaultValue != nil {
			if _, ok := vn.DefaultValue.(string); !ok {
				exprType = typeUnknown
			}
		}
	default:
		return nil, errors.Errorf("unexpected variable type %T", v)
	}

	return &boundVariableAccess{
		hilNode: n,
		elements: elements,
		exprType: exprType,
		tfVar: tfVar,
		ilNode: ilNode,
	}, nil
}

func (b *hilBinder) bindExprs(ns []ast.Node) ([]boundNode, error) {
	boundNodes := make([]boundNode, len(ns))
	for i, n := range ns {
		bn, err := b.bindExpr(n)
		if err != nil {
			return nil, err
		}
		boundNodes[i] = bn
	}
	return boundNodes, nil
}

func (b *hilBinder) bindExpr(n ast.Node) (boundNode, error) {
	switch n := n.(type) {
	case *ast.Arithmetic:
		return b.bindArithmetic(n)
	case *ast.Call:
		return b.bindCall(n)
	case *ast.Conditional:
		return b.bindConditional(n)
	case *ast.Index:
		return b.bindIndex(n)
	case *ast.LiteralNode:
		return b.bindLiteral(n)
	case *ast.Output:
		return b.bindOutput(n)
	case *ast.VariableAccess:
		return b.bindVariableAccess(n)
	default:
		return nil, errors.Errorf("unexpected HIL node type %T", n)
	}
}

type hilGenerator struct {
	w *bytes.Buffer
	countIndex string
}

func (g *hilGenerator) genArithmetic(n *boundArithmetic) {
	op := ""
	switch n.hilNode.Op {
	case ast.ArithmeticOpAdd:
		op = "+"
	case ast.ArithmeticOpSub:
		op = "-"
	case ast.ArithmeticOpMul:
		op = "*"
	case ast.ArithmeticOpDiv:
		op = "/"
	case ast.ArithmeticOpMod:
		op = "%"
	case ast.ArithmeticOpLogicalAnd:
		op = "&&"
	case ast.ArithmeticOpLogicalOr:
		op = "||"
	case ast.ArithmeticOpEqual:
		op = "==="
	case ast.ArithmeticOpNotEqual:
		op = "!=="
	case ast.ArithmeticOpLessThan:
		op = "<"
	case ast.ArithmeticOpLessThanOrEqual:
		op = "<="
	case ast.ArithmeticOpGreaterThan:
		op = ">"
	case ast.ArithmeticOpGreaterThanOrEqual:
		op = ">="
	}
	op = fmt.Sprintf(" %s ", op)

	g.gen("(")
	for i, n := range n.exprs {
		if i != 0 {
			g.gen(op)
		}
		g.gen(n)
	}
	g.gen(")")
}

func (g *hilGenerator) genCall(n *boundCall) {
	switch n.hilNode.Func {
	case "element":
		g.gen(n.args[0], "[", n.args[1], "]")
	case "file":
		g.gen("fs.readFileSync(", n.args[0], ", \"utf-8\")")
	case "lookup":
		hasDefault := len(n.args) == 3
		if hasDefault {
			g.gen("(")
		}
		g.gen("(<any>", n.args[0], ")[", n.args[1], "]")
		if hasDefault {
			g.gen(" || ", n.args[2], ")")
		}
	case "split":
		g.gen(n.args[1], ".split(", n.args[0], ")")
	default:
		contract.Failf("unexpected function in genCall: %v", n.hilNode.Func)
	}
}

func (g *hilGenerator) genConditional(n *boundConditional) {
	g.gen("(", n.condExpr, " ? ", n.trueExpr, " : ", n.falseExpr, ")")
}

func (g *hilGenerator) genIndex(n *boundIndex) {
	g.gen(n.targetExpr, "[", n.keyExpr, "]")
}

func (g *hilGenerator) genLiteral(n *boundLiteral) {
	switch n.exprType {
	case typeBool, typeNumber:
		fmt.Fprintf(g.w, "%v", n.value)
	case typeString:
		fmt.Fprintf(g.w, "%q", n.value)
	default:
		contract.Failf("unexpected literal type in genLiteral: %v", n.exprType)
	}
}

func (g *hilGenerator) genOutput(n *boundOutput) {
	for i, s := range n.exprs {
		if i > 0 {
			g.gen(" + ")
		}
		if s.typ() == typeString {
			g.gen(s)
		} else {
			g.gen("`${", s, "}`")
		}
	}
}

func (g *hilGenerator) genVariableAccess(n *boundVariableAccess) {
	switch v := n.tfVar.(type) {
	case *config.CountVariable:
		g.gen(g.countIndex)
	case *config.ResourceVariable:
		receiver, accessor := resName(v.Type, v.Name), strings.Join(n.elements, ".")
		if v.Multi {
			if v.Index == -1 {
				accessor = fmt.Sprintf("map(v => v.%s)", accessor)
			} else {
				receiver = fmt.Sprintf("%s[%d]", receiver, v.Index)
			}
		}
		g.gen(receiver, ".", accessor)
	case *config.UserVariable:
		g.gen(tfbridge.TerraformToPulumiName(v.Name, nil, false))
	default:
		contract.Failf("unexpected TF var type in genVariableAccess: %T", n.tfVar)
	}
}

func (g *hilGenerator) gen(vs ...interface{}) {
	for _, v := range vs {
		switch v := v.(type) {
		case string:
			g.w.WriteString(v)
		case *boundArithmetic:
			g.genArithmetic(v)
		case *boundCall:
			g.genCall(v)
		case *boundConditional:
			g.genConditional(v)
		case *boundIndex:
			g.genIndex(v)
		case *boundLiteral:
			g.genLiteral(v)
		case *boundOutput:
			g.genOutput(v)
		case *boundVariableAccess:
			g.genVariableAccess(v)
		default:
			contract.Failf("unexpected type in gen: %T", v)
		}
	}
}
