package symbols

import (
	"path/filepath"
	"sort"
	"strings"
)

// Queries are written explicitly rather than taken from the runtime's inferred
// set, which is incomplete for our purposes: the built-in Go query covers only
// functions and methods, so types, constants and package vars — all of which
// the previous regex scan reported — would silently disappear.
//
// Every query must keep at least the declarations matched by
// workspaceSymbolPatterns in internal/tools/workspace_tools.go. The regression
// test in symbols_test.go enforces that.

// The interface pattern overlaps the generic type_spec pattern on purpose: Go's
// grammar has no distinct node for an interface declaration, so a more specific
// query is the only way to classify one. Overlapping matches are collapsed by
// dedupe(), which keeps the more specific kind. Structs are deliberately left
// as "type" — calling a Go struct a class would misdescribe the language.
const goQuery = `
(function_declaration name: (identifier) @name) @definition.function
(method_declaration name: (field_identifier) @name) @definition.method
(type_declaration (type_spec name: (type_identifier) @name)) @definition.type
(type_declaration (type_spec name: (type_identifier) @name type: (interface_type))) @definition.interface
(type_declaration (type_alias name: (type_identifier) @name)) @definition.type
(const_spec name: (identifier) @name) @definition.constant
(var_spec name: (identifier) @name) @definition.variable
`

// typescriptQuery also serves .tsx via the separate tsx grammar. Arrow
// functions assigned to an exported const are the dominant declaration style in
// TS codebases, so lexical_declaration must be covered or the outline is close
// to useless; the previous regex matched them via `^export const`.
const typescriptQuery = `
(function_declaration name: (identifier) @name) @definition.function
(generator_function_declaration name: (identifier) @name) @definition.function
(class_declaration name: (type_identifier) @name) @definition.class
(abstract_class_declaration name: (type_identifier) @name) @definition.class
(interface_declaration name: (type_identifier) @name) @definition.interface
(enum_declaration name: (identifier) @name) @definition.type
(type_alias_declaration name: (type_identifier) @name) @definition.type
(method_definition name: (property_identifier) @name) @definition.method
(public_field_definition name: (property_identifier) @name) @definition.variable
(lexical_declaration (variable_declarator name: (identifier) @name)) @definition.variable
(variable_declaration (variable_declarator name: (identifier) @name)) @definition.variable
`

const javascriptQuery = `
(function_declaration name: (identifier) @name) @definition.function
(generator_function_declaration name: (identifier) @name) @definition.function
(class_declaration name: (identifier) @name) @definition.class
(method_definition name: (property_identifier) @name) @definition.method
(field_definition property: (property_identifier) @name) @definition.variable
(lexical_declaration (variable_declarator name: (identifier) @name)) @definition.variable
(variable_declaration (variable_declarator name: (identifier) @name)) @definition.variable
`

// pythonQuery captures the decorated form separately so the reported range
// covers the decorators too; reading a decorated function without its
// decorators gives a misleading picture of what it does.
const pythonQuery = `
(function_definition name: (identifier) @name) @definition.function
(class_definition name: (identifier) @name) @definition.class
(decorated_definition (function_definition name: (identifier) @name)) @definition.function
(decorated_definition (class_definition name: (identifier) @name)) @definition.class
`

const rustQuery = `
(function_item name: (identifier) @name) @definition.function
(function_signature_item name: (identifier) @name) @definition.function
(struct_item name: (type_identifier) @name) @definition.type
(enum_item name: (type_identifier) @name) @definition.type
(union_item name: (type_identifier) @name) @definition.type
(trait_item name: (type_identifier) @name) @definition.interface
(type_item name: (type_identifier) @name) @definition.type
(mod_item name: (identifier) @name) @definition.module
(impl_item type: (type_identifier) @name) @definition.type
(const_item name: (identifier) @name) @definition.constant
(static_item name: (identifier) @name) @definition.constant
(macro_definition name: (identifier) @name) @definition.function
`

const javaQuery = `
(class_declaration name: (identifier) @name) @definition.class
(interface_declaration name: (identifier) @name) @definition.interface
(enum_declaration name: (identifier) @name) @definition.type
(record_declaration name: (identifier) @name) @definition.class
(annotation_type_declaration name: (identifier) @name) @definition.interface
(method_declaration name: (identifier) @name) @definition.method
(constructor_declaration name: (identifier) @name) @definition.constructor
(field_declaration (variable_declarator name: (identifier) @name)) @definition.variable
`

// Reference queries capture call sites. They are compiled into the same query
// as the definitions so a single parse yields both: resolving which function
// encloses a call needs the definition spans anyway.

const goRefQuery = `
(call_expression function: (identifier) @name) @reference.call
(call_expression function: (selector_expression field: (field_identifier) @name)) @reference.call
`

const typescriptRefQuery = `
(call_expression function: (identifier) @name) @reference.call
(call_expression function: (member_expression property: (property_identifier) @name)) @reference.call
(new_expression constructor: (identifier) @name) @reference.call
`

// JSX element usage is how a React component is actually "called". Without
// these patterns find_callers reports zero call sites for every component in
// the codebase — a confident, wrong answer.
//
// Only the tsx and javascript grammars define JSX nodes; adding them to the
// plain typescript grammar fails query compilation outright, which is why this
// is kept separate rather than folded into typescriptRefQuery.
const jsxRefQuery = `
(jsx_opening_element name: (identifier) @name) @reference.call
(jsx_self_closing_element name: (identifier) @name) @reference.call
`

const javascriptRefQuery = typescriptRefQuery

const pythonRefQuery = `
(call function: (identifier) @name) @reference.call
(call function: (attribute attribute: (identifier) @name)) @reference.call
`

const rustRefQuery = `
(call_expression function: (identifier) @name) @reference.call
(call_expression function: (field_expression field: (field_identifier) @name)) @reference.call
(call_expression function: (scoped_identifier name: (identifier) @name)) @reference.call
(macro_invocation macro: (identifier) @name) @reference.call
`

const javaRefQuery = `
(method_invocation name: (identifier) @name) @reference.call
(object_creation_expression type: (type_identifier) @name) @reference.call
`

// languageQueries maps a grammar name to its query. Keep in sync with the
// grammar_subset build tags in the Dockerfile: a grammar listed here but not
// linked in degrades to ErrUnsupported at call time.
var languageQueries = map[string]string{
	"go":         goQuery + goRefQuery,
	"typescript": typescriptQuery + typescriptRefQuery,
	"tsx":        typescriptQuery + typescriptRefQuery + jsxRefQuery,
	"javascript": javascriptQuery + javascriptRefQuery + jsxRefQuery,
	"python":     pythonQuery + pythonRefQuery,
	"rust":       rustQuery + rustRefQuery,
	"java":       javaQuery + javaRefQuery,
}

// extensionLanguages maps file extensions to grammar names. It mirrors the
// extensions covered by workspaceSymbolPatterns so no file type regresses.
var extensionLanguages = map[string]string{
	".go":   "go",
	".ts":   "typescript",
	".tsx":  "tsx",
	".js":   "javascript",
	".jsx":  "javascript",
	".mjs":  "javascript",
	".cjs":  "javascript",
	".py":   "python",
	".pyi":  "python",
	".rs":   "rust",
	".java": "java",
}

var languageSpecs = func() map[string]*languageSpec {
	out := make(map[string]*languageSpec, len(languageQueries))
	for name, query := range languageQueries {
		out[name] = &languageSpec{name: name, query: query}
	}
	return out
}()

func lookupLanguage(path string) (*languageSpec, bool) {
	return lookupLanguageByExt(strings.ToLower(filepath.Ext(path)))
}

func lookupLanguageByExt(ext string) (*languageSpec, bool) {
	name, ok := extensionLanguages[ext]
	if !ok {
		return nil, false
	}
	spec, ok := languageSpecs[name]
	return spec, ok
}

func sortedExtensions() []string {
	out := make([]string, 0, len(extensionLanguages))
	for ext := range extensionLanguages {
		out = append(out, ext)
	}
	sort.Strings(out)
	return out
}
