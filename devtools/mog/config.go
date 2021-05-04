package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"go/types"
	"strings"
)

type config struct {
	SourcePkg sourcePkg
	Structs   []structConfig
}

type structConfig struct {
	// Source struct name.
	Source           string
	Target           target
	Output           string
	FuncNameFragment string // general namespace for conversion functions
	IgnoreFields     stringSet
	FuncFrom         string
	FuncTo           string
	Fields           []fieldConfig
	typeInfo         *types.Info
}

type stringSet map[string]struct{}

func newStringSetFromSlice(s []string) stringSet {
	ss := make(stringSet, len(s))
	for _, i := range s {
		ss[i] = struct{}{}
	}
	return ss
}

type target struct {
	Package string
	Struct  string
}

func (t target) String() string {
	return t.Package + "." + t.Struct
}

func newTarget(v string) target {
	i := strings.LastIndex(v, ".")
	if i == -1 {
		return target{Struct: v}
	}
	return target{Package: v[:i], Struct: v[i+1:]}
}

type fieldConfig struct {
	SourceName string
	SourceExpr ast.Expr
	TargetName string
	FuncFrom   string
	FuncTo     string
	// TODO: Pointer pointerSettings

	cfg    structConfig // for dynamic
	cfgSet bool
}

func (c fieldConfig) DynFuncFrom(sourcePtr, targetPtr bool) string {
	if c.FuncFrom != "" {
		return c.FuncFrom
	}
	if !c.cfgSet {
		return ""
	}
	return funcNameFrom(c.cfg, sourcePtr, targetPtr)
}

func (c fieldConfig) DynFuncTo(sourcePtr, targetPtr bool) string {
	if c.FuncTo != "" {
		return c.FuncTo
	}
	if !c.cfgSet {
		return ""
	}
	return funcNameTo(c.cfg, sourcePtr, targetPtr)
}

func configsFromAnnotations(pkg sourcePkg) (config, error) {
	names := pkg.StructNames()
	c := config{Structs: make([]structConfig, 0, len(names))}
	c.SourcePkg = pkg

	for _, name := range names {
		strct := pkg.Structs[name]
		cfg, err := parseStructAnnotation(name, strct.Doc)
		if err != nil {
			return c, fmt.Errorf("from source struct %v: %w", name, err)
		}

		for _, field := range strct.Fields {
			f, err := parseFieldAnnotation(field)
			if err != nil {
				return c, fmt.Errorf("from source struct %v: %w", name, err)
			}
			cfg.Fields = append(cfg.Fields, f)
		}

		// TODO: test case
		if err := cfg.Validate(); err != nil {
			return c, fmt.Errorf("invalid config for %v: %w", name, err)
		}
		cfg.typeInfo = pkg.pkg.TypesInfo

		c.Structs = append(c.Structs, cfg)
	}

	return c, nil
}

// TODO: syntax of mog annotations should be in readme
func parseStructAnnotation(name string, doc []*ast.Comment) (structConfig, error) {
	c := structConfig{Source: name}

	i := structAnnotationIndex(doc)
	if i < 0 {
		return c, fmt.Errorf("missing struct annotation")
	}

	buf := new(strings.Builder)
	for _, line := range doc[i+1:] {
		buf.WriteString(strings.TrimLeft(line.Text, "/"))
	}
	for _, part := range strings.Fields(buf.String()) {
		kv := strings.Split(part, "=")
		if len(kv) != 2 {
			return c, fmt.Errorf("invalid term '%v' in annotation, expected only one =", part)
		}
		value := kv[1]
		switch kv[0] {
		case "target":
			c.Target = newTarget(value)
		case "output":
			c.Output = value
		case "name":
			c.FuncNameFragment = value
		case "ignore-fields":
			c.IgnoreFields = newStringSetFromSlice(strings.Split(value, ","))
		case "func-from":
			c.FuncFrom = value
		case "func-to":
			c.FuncTo = value
		default:
			return c, fmt.Errorf("invalid annotation key %v in term '%v'", kv[0], part)
		}
	}

	return c, nil
}

func (c structConfig) Validate() error {
	var errs []error
	fmsg := "missing value for required annotation %q"
	if c.Target.Struct == "" {
		errs = append(errs, fmt.Errorf(fmsg, "target"))
	}
	if c.Output == "" {
		errs = append(errs, fmt.Errorf(fmsg, "output"))
	}
	if c.FuncNameFragment == "" {
		errs = append(errs, fmt.Errorf(fmsg, "name"))
	}
	return fmtErrors("invalid annotations", errs)
}

// TODO: syntax of mog annotations should be in readme
func parseFieldAnnotation(field *ast.Field) (fieldConfig, error) {
	var c fieldConfig

	name, err := fieldName(field)
	if err != nil {
		return c, err
	}

	c.SourceName = name
	c.SourceExpr = field.Type

	text := getFieldAnnotationLine(field.Doc)
	if text == "" {
		return c, nil
	}

	for _, part := range strings.Fields(text) {
		kv := strings.Split(part, "=")
		if len(kv) != 2 {
			return c, fmt.Errorf("invalid term '%v' in annotation, expected only one =", part)
		}
		value := kv[1]
		switch kv[0] {
		case "target":
			c.TargetName = value
		case "pointer":
			// TODO(rb): remove as unnecessary?
		case "func-from":
			c.FuncFrom = value
		case "func-to":
			c.FuncTo = value
		default:
			return c, fmt.Errorf("invalid annotation key %v in term '%v'", kv[0], part)
		}
	}
	return c, nil
}

// TODO test cases for embedded types
func fieldName(field *ast.Field) (string, error) {
	if len(field.Names) > 0 {
		return field.Names[0].Name, nil
	}

	switch n := field.Type.(type) {
	case *ast.Ident:
		return n.Name, nil
	case *ast.SelectorExpr:
		return n.Sel.Name, nil
	}

	buf := new(bytes.Buffer)
	_ = format.Node(buf, new(token.FileSet), field.Type)
	return "", fmt.Errorf("failed to determine field name for type %v", buf.String())
}

func getFieldAnnotationLine(doc *ast.CommentGroup) string {
	if doc == nil {
		return ""
	}

	prefix := "mog: "
	for _, line := range doc.List {
		text := strings.TrimSpace(strings.TrimLeft(line.Text, "/"))
		if strings.HasPrefix(text, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(text, prefix))
		}
	}
	return ""
}

func fmtErrors(msg string, errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf(msg+": %w", errs[0])
	default:
		b := new(strings.Builder)

		for _, err := range errs {
			b.WriteString("\n   ")
			b.WriteString(err.Error())
		}
		return fmt.Errorf(msg+":%s\n", b.String())
	}
}

// TODO: test cases
func applyAutoConvertFunctions(cfgs []structConfig) []structConfig {
	// Index the structs by name so any struct can refer to conversion
	// functions for any other struct.
	byName := make(map[string]structConfig, len(cfgs))
	for _, s := range cfgs {
		byName[s.Source] = s
	}

	for structIdx, s := range cfgs {
		for fieldIdx, f := range s.Fields {
			if _, ignored := s.IgnoreFields[f.SourceName]; ignored {
				continue
			}

			// User supplied override function.
			if f.FuncTo != "" || f.FuncFrom != "" {
				continue
			}

			var (
				ident *ast.Ident
			)
			switch x := f.SourceExpr.(type) {
			case *ast.Ident:
				ident = x
			case *ast.StarExpr:
				var ok bool
				ident, ok = x.X.(*ast.Ident)
				if !ok {
					continue
				}
			default:
				continue
			}

			// Pull up type information for type of this field and attempt
			// auto-convert.
			//
			// Maybe explicitly skip primitives or stuff like strings?
			structCfg, ok := byName[ident.Name]
			if !ok {
				// TODO: log warning that auto convert did not work
				continue
			}

			// Capture this information and use it dynamically to generate
			// FuncFrom/FuncTo based on the LHS/RHS pointerness.
			f.cfg = structCfg
			f.cfgSet = true

			s.Fields[fieldIdx] = f
		}
		cfgs[structIdx] = s
	}
	return cfgs
}