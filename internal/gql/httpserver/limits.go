package httpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

const (
	maxBodyBytes     = 1 << 20
	maxQueryBytes    = 64 << 10
	maxParserTokens  = 10000
	maxSelections    = 1000
	maxDepth         = 20
	maxServiceFields = 32
	maxPageSize      = 1000
	maxInputList     = 1000
	maxQueryString   = 4096
	maxCountBudget   = 5000
)

// operationLimits runs before any resolver, including on cached documents and
// persisted queries. Expansion is counted with early exit: compact documents
// containing repeatedly spread fragments cannot evade the operation budget.
type operationLimits struct{}

func (operationLimits) ExtensionName() string                   { return "OperationLimits" }
func (operationLimits) Validate(graphql.ExecutableSchema) error { return nil }

func (operationLimits) MutateOperationParameters(_ context.Context, p *graphql.RawParams) *gqlerror.Error {
	if len(p.Query) > maxQueryBytes {
		return limitError("query exceeds 65536 bytes")
	}
	return nil
}

func (operationLimits) MutateOperationContext(_ context.Context, op *graphql.OperationContext) *gqlerror.Error {
	selections, serviceFields := 0, 0
	var visit func(ast.SelectionSet, int) *gqlerror.Error
	visit = func(set ast.SelectionSet, depth int) *gqlerror.Error {
		if len(set) > 0 && depth > maxDepth {
			return limitError("selection depth exceeds 20")
		}
		for _, selection := range set {
			selections++
			if selections > maxSelections {
				return limitError("expanded selections exceed 1000")
			}
			switch s := selection.(type) {
			case *ast.Field:
				parent := s.ObjectDefinition.Name
				if strings.HasSuffix(parent, "Query") || strings.HasSuffix(parent, "Mutation") {
					serviceFields++
					if serviceFields > maxServiceFields {
						return limitError("query and mutation fields exceed 32")
					}
				}
				for _, arg := range s.ArgumentMap(op.Variables) {
					if err := validateInput(arg); err != nil {
						return limitError(err.Error())
					}
				}
				if err := visit(s.SelectionSet, depth+1); err != nil {
					return err
				}
			case *ast.FragmentSpread:
				if err := visit(s.Definition.SelectionSet, depth+1); err != nil {
					return err
				}
			case *ast.InlineFragment:
				if err := visit(s.SelectionSet, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return visit(op.Operation.SelectionSet, 1)
}

func limitError(message string) *gqlerror.Error {
	return &gqlerror.Error{Message: message, Extensions: map[string]any{"code": "OPERATION_LIMIT_EXCEEDED"}}
}

func validateInput(value any) error {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if child == nil {
				continue
			}
			switch key {
			case "limit", "page", "offset":
				n, err := graphql.UnmarshalInt(child)
				maxValue := 2147483647
				if key == "limit" {
					maxValue = maxPageSize
				}
				if err != nil || n < 0 || n > maxValue {
					return fmt.Errorf("%s must be between 0 and %d", key, maxValue)
				}
			case "aggregationBudget":
				n, err := graphql.UnmarshalFloat(child)
				if err != nil || !(n >= 1 && n <= maxCountBudget) {
					return fmt.Errorf("aggregationBudget must be between 1 and %d", maxCountBudget)
				}
			case "queryString":
				if str, ok := child.(string); ok && len(str) > maxQueryString {
					return fmt.Errorf("queryString exceeds %d bytes", maxQueryString)
				}
			}
			if err := validateInput(child); err != nil {
				return err
			}
		}
	case []any:
		if len(v) > maxInputList {
			return fmt.Errorf("input lists cannot exceed %d entries", maxInputList)
		}
		for _, child := range v {
			if err := validateInput(child); err != nil {
				return err
			}
		}
	}
	return nil
}
