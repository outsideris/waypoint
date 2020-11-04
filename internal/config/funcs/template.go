package funcs

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

// MakeTemplateFuncs adds the template functions to the function map. The
// template family of functions has access to the context, except they cannot
// call further template functions.
func MakeTemplateFuncs(hclCtx *hcl.EvalContext) map[string]function.Function {
	// Create a child context cause we're going to put template stubs in it.
	hclCtx = hclCtx.NewChild()
	hclCtx.Functions = map[string]function.Function{}

	// Get all our specs
	specs := map[string]*function.Spec{
		"templatestring": makeTemplateString(hclCtx),
	}

	// Override each to prevent template calls within template calls, for now.
	for k, spec := range specs {
		kCopy := k // have to copy since loops reuse variables

		specCopy := *spec
		specCopy.Type = func(args []cty.Value) (cty.Type, error) {
			return cty.NilType, fmt.Errorf(
				"cannot call %s from inside a template function",
				kCopy)
		}

		hclCtx.Functions[k] = function.New(&specCopy)
	}

	result := map[string]function.Function{}
	for k, spec := range specs {
		result[k] = function.New(spec)
	}

	return result
}

func makeTemplateString(hclCtx *hcl.EvalContext) *function.Spec {
	loadTmpl := func(v string) (hcl.Expression, error) {
		expr, diags := hclsyntax.ParseTemplate([]byte(v), "template", hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			return nil, diags
		}

		return expr, nil
	}

	return &function.Spec{
		Params: []function.Parameter{
			{
				Name: "template",
				Type: cty.String,
			},
		},
		VarParam: &function.Parameter{
			Name: "vars",
			Type: cty.DynamicPseudoType,
		},
		Type: func(args []cty.Value) (cty.Type, error) {
			for _, arg := range args {
				if !arg.IsKnown() {
					return cty.DynamicPseudoType, nil
				}
			}

			// We'll render our template now to see what result type it produces.
			// A template consisting only of a single interpolation an potentially
			// return any type.
			expr, err := loadTmpl(args[0].AsString())
			if err != nil {
				return cty.DynamicPseudoType, err
			}

			// This is safe even if args[1] contains unknowns because the HCL
			// template renderer itself knows how to short-circuit those.
			val, err := renderTmpl(expr, hclCtx, args[1:]...)
			return val.Type(), err
		},
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			expr, err := loadTmpl(args[0].AsString())
			if err != nil {
				return cty.DynamicVal, err
			}

			return renderTmpl(expr, hclCtx, args[1:]...)
		},
	}
}

func renderTmpl(expr hcl.Expression, parentCtx *hcl.EvalContext, varsVal ...cty.Value) (cty.Value, error) {
	// Validate all user-supplied variables are maps/objects
	for _, v := range varsVal {
		if varsTy := v.Type(); !(varsTy.IsMapType() || varsTy.IsObjectType()) {
			return cty.DynamicVal, function.NewArgErrorf(1, "invalid vars value: must be a map")
		}
	}

	// Add user-supplied variables to our context
	child := parentCtx.NewChild()
	child.Variables = map[string]cty.Value{}
	for _, val := range varsVal {
		for k, v := range val.AsValueMap() {
			child.Variables[k] = v
		}
	}

	// We require all of the variables to be valid HCL identifiers, because
	// otherwise there would be no way to refer to them in the template
	// anyway. Rejecting this here gives better feedback to the user
	// than a syntax error somewhere in the template itself.
	for n := range child.Variables {
		if !hclsyntax.ValidIdentifier(n) {
			// This error message intentionally doesn't describe _all_ of
			// the different permutations that are technically valid as an
			// HCL identifier, but rather focuses on what we might
			// consider to be an "idiomatic" variable name.
			return cty.DynamicVal, function.NewArgErrorf(1, "invalid template variable name %q: must start with a letter, followed by zero or more letters, digits, and underscores", n)
		}
	}

	// We'll pre-check references in the template here so we can give a
	// more specialized error message than HCL would by default, so it's
	// clearer that this problem is coming from a templatefile call.
	hasVar := func(n string) bool {
		for ctx := child; ctx != nil; ctx = ctx.Parent() {
			if _, ok := ctx.Variables[n]; ok {
				return true
			}
		}

		return false
	}
	for _, traversal := range expr.Variables() {
		root := traversal.RootName()
		if !hasVar(root) {
			return cty.DynamicVal, function.NewArgErrorf(1, "vars map does not contain key %q, referenced at %s", root, traversal[0].SourceRange())
		}
	}

	val, diags := expr.Value(child)
	if diags.HasErrors() {
		return cty.DynamicVal, diags
	}
	return val, nil
}
