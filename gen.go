package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/types"
	"io"

	"github.com/dave/jennifer/jen"
	"sigs.k8s.io/controller-tools/pkg/genall"
	"sigs.k8s.io/controller-tools/pkg/loader"
	"sigs.k8s.io/controller-tools/pkg/markers"
)

//go:generate go run sigs.k8s.io/controller-tools/cmd/helpgen generate:headerFile=./boilerplate.go.txt,year=2019 paths=.

var (
	enableTypeMarker = markers.Must(markers.MakeDefinition("shallowcopy:generate", markers.DescribesType, false))
)

type copyStructs struct {
	StructName string
	Fields     []string
}

// +controllertools:marker:generateHelp

// Generator generates code containing ShallowCopy method implementations.
type Generator struct{}

func (Generator) RegisterMarkers(into *markers.Registry) error {
	if err := markers.RegisterAll(into, enableTypeMarker); err != nil {
		return err
	}

	into.AddHelp(
		enableTypeMarker,
		markers.SimpleHelp("object", "enables or disables shallowcopy implementation generation for this type"),
	)

	return nil
}

func enabledOnType(info *markers.TypeInfo) bool {
	if typeMarker := info.Markers.Get(enableTypeMarker.Name); typeMarker != nil {
		return typeMarker.(bool)
	}

	return false
}

func (Generator) Generate(ctx *genall.GenerationContext) error {
	for _, root := range ctx.Roots {
		ctx.Checker.Check(root, func(node ast.Node) bool {
			// ignore interfaces
			_, isIface := node.(*ast.InterfaceType)
			return !isIface
		})

		root.NeedTypesInfo()

		var structs []copyStructs

		if err := markers.EachType(ctx.Collector, root, func(info *markers.TypeInfo) {
			// copy when enabled specifically on this type
			if !enabledOnType(info) {
				return
			}

			// avoid copying non-exported types, etc
			if !shouldBeCopied(root, info) {
				return
			}

			typeInfo := root.TypesInfo.TypeOf(info.RawSpec.Name)
			if typeInfo == types.Typ[types.Invalid] {
				root.AddError(loader.ErrFromNode(fmt.Errorf("unknown type %s", info.Name), info.RawSpec))
			}

			stype, ok := typeInfo.Underlying().(*types.Struct)
			if !ok {
				root.AddError(loader.ErrFromNode(fmt.Errorf("%s is not a struct type", info.Name), info.RawSpec))

				return
			}

			data := copyStructs{
				StructName: info.Name,
				Fields:     make([]string, 0, stype.NumFields()),
			}

			for i := 0; i < stype.NumFields(); i++ {
				field := stype.Field(i)

				data.Fields = append(data.Fields, field.Name())
			}

			structs = append(structs, data)
		}); err != nil {
			root.AddError(err)
			return nil
		}

		if len(structs) > 0 {
			code := jen.NewFile(root.Name)

			for _, s := range structs {
				code.Func().
					Params(jen.Id("o").Id(s.StructName)).
					Id("ShallowCopy").
					Params().
					Params(jen.Id(s.StructName)).
					Block(jen.Return(
						jen.Id(s.StructName).Values(jen.DictFunc(func(d jen.Dict) {
							for _, field := range s.Fields {
								d[jen.Id(field)] = jen.Id("o").Dot(field)
							}
						})),
					))
			}

			var b bytes.Buffer

			err := code.Render(&b)
			if err != nil {
				root.AddError(err)

				return nil
			}

			outContents, err := format.Source(b.Bytes())
			if err != nil {
				root.AddError(err)

				return nil
			}

			writeOut(ctx, root, outContents)
		}
	}

	return nil
}

// shouldBeCopied checks if we're supposed to make shallowcopy methods on the given type.
//
// This is the case if it's exported *and* either:
// - has a partial manual ShallowCopy implementation (in which case we fill in the rest)
// - aliases to a non-basic type eventually
// - is a struct
func shouldBeCopied(pkg *loader.Package, info *markers.TypeInfo) bool {
	if !ast.IsExported(info.Name) {
		return false
	}

	typeInfo := pkg.TypesInfo.TypeOf(info.RawSpec.Name)
	if typeInfo == types.Typ[types.Invalid] {
		pkg.AddError(loader.ErrFromNode(fmt.Errorf("unknown type %s", info.Name), info.RawSpec))
		return false
	}

	// according to gengo, everything named is an alias, except for an alias to a pointer,
	// which is just a pointer, afaict.  Just roll with it.
	if asPtr, isPtr := typeInfo.(*types.Named).Underlying().(*types.Pointer); isPtr {
		typeInfo = asPtr
	}

	lastType := typeInfo
	if _, isNamed := typeInfo.(*types.Named); isNamed {
		// if it has a manual shallowcopy, we're fine
		if hasShallowCopyMethod(pkg, typeInfo) {
			return true
		}

		for underlyingType := typeInfo.Underlying(); underlyingType != lastType; lastType, underlyingType = underlyingType, underlyingType.Underlying() {
			// if it has a manual shallowcopy, we're fine
			if hasShallowCopyMethod(pkg, underlyingType) {
				return true
			}

			// aliases to other things besides basics need copy methods
			// (basics can be straight-up shallow-copied)
			if _, isBasic := underlyingType.(*types.Basic); !isBasic {
				return true
			}
		}
	}

	// structs are the only thing that's not a basic that's copiable by default
	_, isStruct := lastType.(*types.Struct)
	return isStruct
}

// hasShallowCopyMethod checks if this type has a manual ShallowCopy method.
func hasShallowCopyMethod(pkg *loader.Package, typeInfo types.Type) bool {
	shallowCopyMethod, ind, _ := types.LookupFieldOrMethod(typeInfo, true /* check pointers too */, pkg.Types, "ShallowCopy")
	if len(ind) != 1 {
		// ignore embedded methods
		return false
	}
	if shallowCopyMethod == nil {
		return false
	}

	methodSig := shallowCopyMethod.Type().(*types.Signature)
	if methodSig.Params() != nil && methodSig.Params().Len() != 0 {
		return false
	}
	if methodSig.Results() == nil || methodSig.Results().Len() != 1 {
		return false
	}

	return true
}

// writeFormatted outputs the given code, after gofmt-ing it.  If we couldn't gofmt,
// we write the unformatted code for debugging purposes.
func writeOut(ctx *genall.GenerationContext, root *loader.Package, outBytes []byte) {
	outputFile, err := ctx.Open(root, "zz_generated.shallowcopy.go")
	if err != nil {
		root.AddError(err)
		return
	}
	defer outputFile.Close()
	n, err := outputFile.Write(outBytes)
	if err != nil {
		root.AddError(err)
		return
	}
	if n < len(outBytes) {
		root.AddError(io.ErrShortWrite)
	}
}
