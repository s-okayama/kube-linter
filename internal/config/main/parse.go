package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"text/template"

	"golang.stackrox.io/kube-linter/internal/stringutils"
	"golang.stackrox.io/kube-linter/internal/utils"

	"github.com/Masterminds/sprig/v3"
	"github.com/forestgiant/sliceutil"
	"github.com/iancoleman/strcase"
	"github.com/pkg/errors"
	"k8s.io/gengo/parser"
	"k8s.io/gengo/types"
)

const (
	metadataMarker   = "+"
	viperTagKey      = "viper"
	paramsStructName = "Config"
)

const (
	fileTemplateStr = `// Code generated by kube-linter flag codegen. DO NOT EDIT.
// +build !flagcodegen

package config

import (
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// AddFlags, walks through config.Check struct and bind its Member to Cobra command 
// and add respective Viper flag 
func AddFlags(c *cobra.Command, v *viper.Viper) {
	{{- range . }}

	{{- if .FlagDesc.AddFlag }}
	{{- if eq .FlagDesc.Type "stringSlice" }}		
	c.Flags().StringSlice("{{ .FlagDesc.Name }}", nil, "{{ .FlagDesc.Description }}")
	{{- else if eq .FlagDesc.Type "string" }}
	c.Flags().String("{{ .FlagDesc.Name }}", "", "{{ .FlagDesc.Description }}")
	{{- else if eq .FlagDesc.Type "boolean" }}
	c.Flags().Bool("{{ .FlagDesc.Name }}", false, "{{ .FlagDesc.Description }}")
	{{- else if eq .FlagDesc.Type "float32" }}
	c.Flags().Float32("{{ .FlagDesc.Name }}", 0, "{{ .FlagDesc.Description }}")
	{{- else if eq .FlagDesc.Type "float64" }}
	c.Flags().Float64("{{ .FlagDesc.Name }}", 0, "{{ .FlagDesc.Description }}")
	{{- else if eq .FlagDesc.Type "integer" }}
	c.Flags().Int("{{ .FlagDesc.Name }}", 0, "{{ .FlagDesc.Description }}")
	{{- end }}
	{{- if ne .FlagDesc.Parent "" }}
	v.BindPFlag("{{ .FlagDesc.Parent }}.{{ .FlagDesc.Name }}", c.Flags().Lookup("{{ .FlagDesc.Name }}"))
	{{- else }}
	v.BindPFlag("{{ .FlagDesc.Name }}", c.Flags().Lookup("{{ .FlagDesc.Name }}"))
	{{- end }}
	{{- end }}

	{{- end }}
}
`
)

var (
	fileTemplate = template.Must(template.New("gen").Funcs(sprig.TxtFuncMap()).Parse(fileTemplateStr))
)

type flagType string

// This types are used in template to add appropriate Cobra flags
const (
	stringType      flagType = "string"
	integerType     flagType = "integer"
	booleanType     flagType = "boolean"
	float32Type     flagType = "float32"
	float64Type     flagType = "float64"
	stringSliceType flagType = "stringSlice"
	objectType      flagType = "object"
)

type flagDesc struct {
	Name        string
	Parent      string
	Type        flagType
	Description string
	AddFlag     bool
}

type templateElem struct {
	FlagDesc flagDesc
}

func getName(member types.Member) string {
	if jsonTag := reflect.StructTag(member.Tags).Get("json"); jsonTag != "" {
		name, _ := stringutils.Split2(jsonTag, ",")
		if name != "" {
			return strcase.ToKebab(member.Name)
		}
	}
	return strcase.ToKebab(member.Name)
}

func getDescription(member types.Member) string {
	firstCommentLineWithMetadata := len(member.CommentLines)
	for i, commentLine := range member.CommentLines {
		if strings.HasPrefix(commentLine, metadataMarker) {
			firstCommentLineWithMetadata = i
			break
		}
	}
	return strings.Join(member.CommentLines[:firstCommentLineWithMetadata], " ")
}

// hasViperExcludeTag checks if provided tags has viper=exclude tag
func hasViperExcludeTag(tags map[string][]string) bool {
	viperTags := tags[viperTagKey]
	return sliceutil.Contains(viperTags, "exclude")

}

func constructFlagDescFromStruct(typeSpec *types.Type) ([]flagDesc, error) {
	var flagDescs []flagDesc
	for _, member := range types.FlattenMembers(typeSpec.Members) {

		extractedTags := types.ExtractCommentTags("+", member.CommentLines)
		if hasViperExcludeTag(extractedTags) {
			continue
		}

		desc := flagDesc{
			Name:        getName(member),
			Description: getDescription(member),
			AddFlag:     true,
		}

		relevantTyp := member.Type

		switch kind := relevantTyp.Kind; kind {
		case types.Builtin:
			switch relevantTyp {
			case types.String:
				desc.Type = stringType
			case types.Int:
				desc.Type = integerType
			case types.Float32:
				desc.Type = float32Type
			case types.Float64:
				desc.Type = float64Type
			case types.Bool:
				desc.Type = booleanType
			default:
				return nil, errors.Errorf("currently unsupported type %v", member.Type)
			}
		case types.Struct:
			desc.Type = objectType
			desc.AddFlag = false
			subDesc, err := constructFlagDescFromStruct(member.Type)
			// Template uses Parent name in mapstructure in order to
			// correctly map config values
			for i := range subDesc {
				subDesc[i].Parent = desc.Name
			}

			if err != nil {
				return nil, errors.Wrapf(err, "handling field %s", member.Name)
			}

			flagDescs = append(flagDescs, subDesc...)
		case types.Slice:
			switch sliceType := relevantTyp.Elem.String(); sliceType {
			case "string":
				desc.Type = stringSliceType
			default:
				return nil, errors.Errorf("currently unsupported Slice of type %s", sliceType)
			}
		default:
			return nil, errors.Errorf("currently unsupported type %v", member.Type)
		}

		flagDescs = append(flagDescs, desc)
	}
	return flagDescs, nil
}

func mainCmd() error {
	b := parser.New()

	// This avoids parsing generated files in the package (since we add +build !flagcodegen to them,
	// which makes the parsing much quicker since the parser doesn't have to load any imported packages).
	b.AddBuildTags("flagcodegen")
	if err := b.AddDir("../config"); err != nil {
		return err
	}
	typeUniverse, err := b.FindTypes()
	if err != nil {
		return err
	}

	pkgNames := b.FindPackages()
	if len(pkgNames) != 1 {
		return errors.Errorf("found unexpected number of packages in %+v: %d", pkgNames, len(pkgNames))
	}

	pkg := typeUniverse.Package(pkgNames[0])
	paramsType := pkg.Type(paramsStructName)

	flagDescs, err := constructFlagDescFromStruct(paramsType)
	if err != nil {
		return err
	}

	var templateObj []templateElem

	for _, flagDesc := range flagDescs {
		buf := bytes.NewBuffer(nil)
		enc := json.NewEncoder(buf)
		enc.SetIndent("", "\t")
		if err := enc.Encode(flagDesc); err != nil {
			return errors.Wrapf(err, "couldn't marshal param %v", flagDesc)
		}

		templateObj = append(templateObj, templateElem{
			FlagDesc: flagDesc,
		})
	}

	outFileName := filepath.Join("..", "config", "flags.go")
	outF, err := os.Create(outFileName)
	if err != nil {
		return errors.Wrap(err, "creating output file")
	}
	defer utils.IgnoreError(outF.Close)
	if err := fileTemplate.Execute(outF, templateObj); err != nil {
		return errors.Wrap(err, "Writing template to File")
	}
	return nil
}

func main() {
	if err := mainCmd(); err != nil {
		fmt.Printf("Error executing command: %v", err)
		os.Exit(1)
	}
}