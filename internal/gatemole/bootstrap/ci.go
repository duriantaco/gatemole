package bootstrap

import "strings"

func isWorkflowFile(path string) bool {
	return strings.HasPrefix(path, ".github/workflows/") && (strings.HasSuffix(path, ".yml") || strings.HasSuffix(path, ".yaml"))
}

func isOpenAPIFile(path string) bool {
	base := slashBase(path)
	return base == "openapi.yaml" || base == "openapi.yml" || base == "swagger.yaml" || base == "swagger.yml"
}

func isCoverageFile(path string) bool {
	return slashBase(path) == "coverage.xml"
}
