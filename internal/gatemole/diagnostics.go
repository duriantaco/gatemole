package gatemole

import (
	"fmt"
	"strings"
)

type DiagnosticError struct {
	Diagnostics []Diagnostic
}

func (e DiagnosticError) Error() string {
	if len(e.Diagnostics) == 0 {
		return "diagnostics failed"
	}
	var parts []string
	for _, diagnostic := range e.Diagnostics {
		parts = append(parts, FormatDiagnostic(diagnostic))
	}
	return strings.Join(parts, "; ")
}

func HasErrorDiagnostics(diagnostics []Diagnostic) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == "error" {
			return true
		}
	}
	return false
}

func FormatDiagnostic(diagnostic Diagnostic) string {
	location := diagnostic.Path
	if location == "" {
		location = diagnostic.Span.File
	}
	if diagnostic.Span.Line > 0 {
		location = fmt.Sprintf("%s:%d:%d", location, diagnostic.Span.Line, diagnostic.Span.Column)
	}
	return fmt.Sprintf("%s %s: %s", location, diagnostic.Code, diagnostic.Message)
}

func stringDiagnostics(owner string, messages []string) []Diagnostic {
	diagnostics := make([]Diagnostic, 0, len(messages))
	for _, message := range messages {
		diagnostics = append(diagnostics, Diagnostic{
			Severity: "error",
			Code:     "validation.error",
			Message:  message,
			Path:     owner,
		})
	}
	return diagnostics
}
