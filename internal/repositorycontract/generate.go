package repositorycontract

// Deploy templates are the canonical Repository Contract source. Generate the
// compiled runtime payload after changing them.
//go:generate go run ./cmd/gentemplates -source ../../deploy/platform/repository-contract -output templates_generated.go
