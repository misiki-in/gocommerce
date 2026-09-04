package identity

import _ "embed"

// The module's slice of the API contract, merged into the served document at
// startup so /doc describes every route this store actually answers.
//
//go:embed openapi.json
var openapiFragment []byte

// OpenAPI implements gocommerce.OpenAPIContributor.
func (m *Module) OpenAPI() []byte { return openapiFragment }
