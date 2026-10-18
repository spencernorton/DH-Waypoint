package config

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/go-viper/mapstructure/v2"
	"github.com/iancoleman/strcase"
	"github.com/spencernorton/DH-Waypoint/internal/config/configresolver"
	"go.uber.org/fx"
)

type Params struct {
	fx.In
	Specs     []Spec                    `group:"config_specs"`
	Resolvers []configresolver.Resolver `group:"config_resolvers"`
	Validate  *validator.Validate
}

type Result struct {
	fx.Out
	Resolved ResolvedConfig
}

func New(p Params) (r Result, err error) {
	resolvers := p.Resolvers
	sort.Slice(resolvers, func(i, j int) bool {
		if resolvers[i].Priority() == resolvers[j].Priority() {
			return strings.Compare(resolvers[i].Key(), resolvers[j].Key()) < 0
		}

		return resolvers[i].Priority() < resolvers[j].Priority()
	})

	res := &ResolvedConfig{
		NodeMap: make(map[string]ResolvedNode),
	}

	for _, spec := range p.Specs {
		resolved, resolveErr := resolveRootNode(resolvers, p.Validate, spec)
		if resolveErr != nil {
			err = resolveErr
			return
		}

		res.NodeMap[spec.Key] = resolved
	}

	if unknownErr := rejectUnknownMapKeys(resolvers, *res); unknownErr != nil {
		err = unknownErr
		return
	}

	r.Resolved = *res

	return
}

type Spec struct {
	Key          string
	DefaultValue interface{}
}

type ResolvedConfig struct {
	NodeMap map[string]ResolvedNode
}

func (r ResolvedConfig) Nodes() []ResolvedNode {
	nodes := make([]ResolvedNode, 0, len(r.NodeMap))
	for _, node := range r.NodeMap {
		nodes = append(nodes, node)
	}

	sortNodes(nodes)

	return nodes
}

type ResolvedNode struct {
	Spec
	IsStruct     bool
	ResolverKey  string
	Type         reflect.Type
	ParentPath   []string
	PathString   string
	StructKey    string
	Value        interface{}
	ValueRaw     interface{}
	ValueLabel   string
	DefaultLabel string
	ChildMap     map[string]ResolvedNode
}

func (r ResolvedNode) Children() []ResolvedNode {
	children := make([]ResolvedNode, 0, len(r.ChildMap))
	for _, child := range r.ChildMap {
		children = append(children, child)
	}

	sortNodes(children)

	return children
}

func sortNodes(nodes []ResolvedNode) {
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].IsStruct != nodes[j].IsStruct {
			return !nodes[i].IsStruct
		}

		return strings.Compare(nodes[i].Key, nodes[j].Key) < 0
	})
}

func resolveRootNode(
	resolvers []configresolver.Resolver,
	val *validator.Validate,
	spec Spec,
) (ResolvedNode, error) {
	return resolveStructNode(resolvers, val, []string{}, spec.Key, "", reflect.ValueOf(spec.DefaultValue))
}

func resolveStructNode(
	resolvers []configresolver.Resolver,
	val *validator.Validate,
	parentPath []string,
	key string,
	structKey string,
	value reflect.Value,
) (ResolvedNode, error) {
	if value.Type().Kind() != reflect.Struct {
		return ResolvedNode{}, errors.New("default value must be a struct")
	}

	thisPath := parentPath
	thisPath = append(thisPath, key)
	defaultValue := value.Interface()
	children := make(map[string]ResolvedNode)

	for i := range value.Type().NumField() {
		field := value.Type().Field(i)
		fieldKey := strcase.ToSnake(field.Name)
		fieldValue := value.FieldByName(field.Name)

		switch field.Type.Kind() {
		case reflect.Struct:
			structResolved, err := resolveStructNode(
				resolvers,
				val,
				thisPath,
				fieldKey,
				field.Name,
				fieldValue,
			)
			if err != nil {
				return ResolvedNode{}, err
			}

			children[fieldKey] = structResolved
		default:
			dv := fieldValue.Interface()
			rv := dv

			var rk string

			for _, resolver := range resolvers {
				if resolved, ok, err := resolver.Resolve(append(thisPath, fieldKey), field.Type); err != nil {
					return ResolvedNode{}, err
				} else if ok {
					rv = resolved
					rk = resolver.Key()

					break
				}
			}
			// Preserve named string values before building diagnostic nodes.
			// Such types may intentionally redact String/JSON output while the
			// typed application configuration retains the original credential.
			if field.Type.Kind() == reflect.String {
				v := reflect.ValueOf(rv)
				if v.IsValid() && v.Kind() == reflect.String && v.Type() != field.Type && v.Type().ConvertibleTo(field.Type) {
					rv = v.Convert(field.Type).Interface()
				}
			}

			children[fieldKey] = ResolvedNode{
				Spec: Spec{
					Key:          fieldKey,
					DefaultValue: dv,
				},
				ResolverKey:  rk,
				Type:         field.Type,
				ParentPath:   thisPath,
				PathString:   strings.Join(append(thisPath, fieldKey), "."),
				StructKey:    field.Name,
				Value:        rv,
				ValueRaw:     rv,
				ValueLabel:   createValueLabel(rv),
				DefaultLabel: createValueLabel(dv),
			}
		}
	}

	valueMap := make(map[string]interface{}, len(children))
	for _, c := range children {
		valueMap[c.StructKey] = c.ValueRaw
	}

	resolvedValue := reflect.New(value.Type())
	decodeConfig := &mapstructure.DecoderConfig{
		Metadata: nil,
		Result:   resolvedValue.Interface(),
		MatchName: func(mapKey, fieldName string) bool {
			return mapKey == fieldName
		},
	}

	decoder, decoderErr := mapstructure.NewDecoder(decodeConfig)
	if decoderErr != nil {
		return ResolvedNode{}, decoderErr
	}

	if decodeErr := decoder.Decode(valueMap); decodeErr != nil {
		return ResolvedNode{}, decodeErr
	}

	if validateErr := val.Struct(resolvedValue.Interface()); validateErr != nil {
		return ResolvedNode{}, validateErr
	}

	return ResolvedNode{
		Spec: Spec{
			Key:          key,
			DefaultValue: defaultValue,
		},
		ParentPath: parentPath,
		PathString: strings.Join(thisPath, "."),
		StructKey:  structKey,
		IsStruct:   true,
		Type:       value.Type(),
		Value:      reflect.Indirect(resolvedValue).Interface(),
		ValueRaw:   valueMap,
		ChildMap:   children,
	}, nil
}

func createValueLabel(value interface{}) string {
	var label string

	switch value.(type) {
	case string:
		label = fmt.Sprintf("'%s'", value)
	default:
		label = fmt.Sprintf("%v", value)
	}

	if len(label) > 20 {
		label = label[:20] + "..."
	}

	return label
}

// rejectUnknownMapKeys fails startup when a file-based (map) resolver
// carries a key that matches no configuration path. A misspelled key
// in config.yml must be a loud error, not a silently ignored setting
// that reads as "feature disabled". Environment variables are exempt
// (the process env legitimately holds unrelated keys); env typos are
// still visible via `dh-waypoint config show` provenance.
func rejectUnknownMapKeys(resolvers []configresolver.Resolver, resolved ResolvedConfig) error {
	known := map[string]bool{}
	knownLeaf := map[string]bool{}
	var walk func(n ResolvedNode)
	walk = func(n ResolvedNode) {
		known[n.PathString] = true
		if !n.IsStruct {
			knownLeaf[n.PathString] = true
		}
		for _, child := range n.ChildMap {
			walk(child)
		}
	}
	for _, node := range resolved.NodeMap {
		walk(node)
	}
	knownPath := func(path string) bool {
		if known[path] {
			return true
		}
		// Descendants of a leaf are map-typed configuration
		// (e.g. classifier.keywords.<list name>). Descendants of a
		// struct node must match a real field exactly.
		for i := len(path) - 1; i > 0; i-- {
			if path[i] == '.' && knownLeaf[path[:i]] {
				return true
			}
		}
		return false
	}
	for _, resolver := range resolvers {
		raw, ok := resolver.(interface{ RawMap() map[string]interface{} })
		if !ok {
			continue
		}
		var unknown []string
		var visit func(prefix string, m map[string]interface{})
		visit = func(prefix string, m map[string]interface{}) {
			for k, v := range m {
				path := k
				if prefix != "" {
					path = prefix + "." + k
				}
				if child, isMap := v.(map[string]interface{}); isMap && len(child) > 0 {
					visit(path, child)
					continue
				}
				if !knownPath(path) {
					unknown = append(unknown, path)
				}
			}
		}
		visit("", raw.RawMap())
		if len(unknown) > 0 {
			sort.Strings(unknown)
			return fmt.Errorf("config %s: unknown configuration key(s): %s", resolver.Key(), strings.Join(unknown, ", "))
		}
	}
	return nil
}
