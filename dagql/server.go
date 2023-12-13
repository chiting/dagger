package dagql

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/99designs/gqlgen/graphql"
	"github.com/opencontainers/go-digest"
	"github.com/sourcegraph/conc/pool"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vito/dagql/idproto"
)

// Server represents a GraphQL server whose schema is dynamically modified at
// runtime.
type Server struct {
	root        Object
	classes     map[string]ObjectType
	scalars     map[string]ScalarType
	cache       *CacheMap[digest.Digest, any]
	installLock sync.Mutex
}

// NewServer returns a new Server with the given root object.
func NewServer[T Typed](root T) *Server {
	queryClass := NewClass[T]()
	srv := &Server{
		root: Instance[T]{
			Constructor: idproto.New(root.Type().Name()),
			Self:        root,
			Class:       queryClass,
		},
		classes: map[string]ObjectType{
			root.Type().Name(): queryClass,
		},
		scalars: map[string]ScalarType{
			"Boolean": Boolean{},
			"Int":     Int{},
			"Float":   Float{},
			"String":  String{},
			// instead of a single ID type, each object has its own ID type
			// "ID": ID{},
		},
		cache: NewCacheMap[digest.Digest, any](),
	}
	return srv
}

// Root returns the root object of the server. It is suitable for passing to
// Resolve to resolve a query.
func (s *Server) Root() Object {
	return s.root
}

var _ graphql.ExecutableSchema = (*Server)(nil)

// Schema returns the current schema of the server.
func (s *Server) Schema() *ast.Schema {
	// TODO track when the schema changes, cache until it changes again
	queryType := s.Root().Type().Name()
	schema := &ast.Schema{}
	for _, t := range s.classes {
		def := t.Definition()
		if def.Name == queryType {
			schema.Query = def
		}
		schema.AddTypes(def)
	}
	for _, t := range s.scalars {
		schema.AddTypes(t.Definition())
	}
	return schema
}

// Complexity returns the complexity of the given field.
func (s *Server) Complexity(typeName, field string, childComplexity int, args map[string]interface{}) (int, bool) {
	// TODO
	return 1, false
}

// Exec implements graphql.ExecutableSchema.
func (s *Server) Exec(ctx context.Context) graphql.ResponseHandler {
	return func(ctx context.Context) *graphql.Response {
		gqlOp := graphql.GetOperationContext(ctx)

		if err := gqlOp.Validate(ctx); err != nil {
			return graphql.ErrorResponse(ctx, "validate: %s", err)
		}

		doc := gqlOp.Doc

		results := make(map[string]any)
		for _, op := range doc.Operations {
			switch op.Operation {
			case ast.Query:
				// TODO prospective
				if gqlOp.OperationName != "" && gqlOp.OperationName != op.Name {
					continue
				}
				sels, err := s.parseASTSelections(gqlOp, op.SelectionSet)
				if err != nil {
					return graphql.ErrorResponse(ctx, "failed to convert selections: %s", err)
				}
				results, err = s.Resolve(ctx, s.root, sels...)
				if err != nil {
					return graphql.ErrorResponse(ctx, "failed to resolve: %s", err)
				}
			case ast.Mutation:
				// TODO
				return graphql.ErrorResponse(ctx, "mutations not supported")
			case ast.Subscription:
				// TODO
				return graphql.ErrorResponse(ctx, "subscriptions not supported")
			}
		}

		data, err := json.Marshal(results)
		if err != nil {
			gqlOp.Error(ctx, err)
			return graphql.ErrorResponse(ctx, "marshal: %s", err)
		}

		return &graphql.Response{
			Data: json.RawMessage(data),
		}
	}
}

// Resolve resolves the given selections on the given object.
//
// Each selection is resolved in parallel, and the results are returned in a
// map whose keys correspond to the selection's field name or alias.
func (s *Server) Resolve(ctx context.Context, self Object, sels ...Selection) (map[string]any, error) {
	results := new(sync.Map)

	pool := new(pool.ErrorPool)
	for _, sel := range sels {
		sel := sel
		pool.Go(func() error {
			res, err := s.resolvePath(ctx, self, sel)
			if err != nil {
				return fmt.Errorf("%s: %w", sel.Name(), err)
			}
			results.Store(sel.Name(), res)
			return nil
		})
	}
	if err := pool.Wait(); err != nil {
		return nil, err
	}

	resultsMap := make(map[string]any)
	results.Range(func(key, value any) bool {
		resultsMap[key.(string)] = value
		return true
	})
	return resultsMap, nil
}

// Load loads the object with the given ID.
func (s *Server) Load(ctx context.Context, id *idproto.ID) (Object, error) {
	var res Typed = s.root
	for i, idSel := range id.Constructor {
		stepID := id.Clone()
		stepID.Constructor = id.Constructor[:i+1]
		// TODO: kind of annoying but technically correct; for the ID to match, the
		// return type at this point in time also has to match.
		fieldDef, err := s.field(res.Type().Name(), idSel.Field)
		if err != nil {
			return nil, err
		}
		stepID.TypeName = fieldDef.Type.Name()

		obj, err := s.toSelectable(stepID, res)
		if err != nil {
			return nil, fmt.Errorf("instantiate from id: %w", err)
		}
		sel := Selector{
			Field: idSel.Field,
			Args:  make(map[string]Typed, len(idSel.Args)),
			Nth:   int(idSel.Nth),
		}
		for _, arg := range idSel.Args {
			val, err := s.fromLiteral(ctx, arg.Value)
			if err != nil {
				return nil, err
			}
			sel.Args[arg.Name] = val
		}
		res, err = obj.Select(ctx, sel)
		if err != nil {
			return nil, err
		}
		if sel.Nth != 0 {
			enum, ok := res.(Enumerable)
			if !ok {
				return nil, fmt.Errorf("cannot sub-select %dth item from %T", sel.Nth, res)
			}
			res, err = enum.Nth(sel.Nth)
			if err != nil {
				return nil, err
			}
		}
	}
	return s.toSelectable(id, res)
}

func (s *Server) parseASTSelections(gqlOp *graphql.OperationContext, astSels ast.SelectionSet) ([]Selection, error) {
	vars := gqlOp.Variables

	sels := []Selection{}
	for _, sel := range astSels {
		switch x := sel.(type) {
		case *ast.Field:
			args := make(map[string]Typed, len(x.Arguments))
			for _, arg := range x.Arguments {
				val, err := arg.Value.Value(vars)
				if err != nil {
					return nil, err
				}
				arg := x.Definition.Arguments.ForName(arg.Name)
				if arg == nil {
					return nil, fmt.Errorf("unknown argument: %q", arg.Name)
				}
				scalar, ok := s.scalars[arg.Type.Name()]
				if !ok {
					return nil, fmt.Errorf("unknown scalar: %q", arg.Type.Name())
				}
				typed, err := scalar.New(val)
				if err != nil {
					return nil, err
				}
				args[arg.Name] = typed
			}
			subsels, err := s.parseASTSelections(gqlOp, x.SelectionSet)
			if err != nil {
				return nil, err
			}
			sels = append(sels, Selection{
				Alias: x.Alias,
				Selector: Selector{
					Field: x.Name,
					Args:  args,
				},
				Subselections: subsels,
			})
		case *ast.FragmentSpread:
			fragment := gqlOp.Doc.Fragments.ForName(x.Name)
			if fragment == nil {
				return nil, fmt.Errorf("unknown fragment: %s", x.Name)
			}
			subsels, err := s.parseASTSelections(gqlOp, fragment.SelectionSet)
			if err != nil {
				return nil, err
			}
			sels = append(sels, subsels...)
		default:
			return nil, fmt.Errorf("unknown field type: %T", x)
		}
	}

	return sels, nil
}

func (s *Server) resolvePath(ctx context.Context, self Object, sel Selection) (any, error) {
	class, ok := s.classes[self.Type().Name()]
	if !ok {
		return nil, fmt.Errorf("unknown type: %q", self.Type().Name())
	}
	fieldDef, ok := class.FieldDefinition(sel.Selector.Field)
	if fieldDef == nil {
		return nil, fmt.Errorf("unknown field: %q", sel.Selector.Field)
	}
	chainedID := sel.Selector.AppendToID(self.ID(), fieldDef)

	// digest, err := chain.Canonical().Digest()
	// if err != nil {
	// 	return nil, err
	// }

	// if field.Pure && !chain.Tainted() { // TODO test !chain.Tainted(); intent is to not cache any queries that depend on a tainted input
	// 	val, err = s.cache.GetOrInitialize(ctx, digest, func(ctx context.Context) (any, error) {
	// 		return root.Resolve(ctx, sel.Selector)
	// 	})
	// } else {
	val, err := self.Select(ctx, sel.Selector)
	// }
	if err != nil {
		return nil, err
	}

	var isNull bool
	if n, ok := val.(Nullable); ok {
		val, ok = n.Unwrap()
		isNull = !ok
	}

	var res any
	if isNull {
		res = nil
	} else if len(sel.Subselections) == 0 {
		res = val
	} else if len(sel.Subselections) > 0 {
		switch {
		case fieldDef.Type.NamedType != "":
			node, err := s.toSelectable(chainedID, val)
			if err != nil {
				return nil, fmt.Errorf("instantiate: %w", err)
			}
			res, err = s.Resolve(ctx, node, sel.Subselections...)
			if err != nil {
				return nil, err
			}
		case fieldDef.Type.Elem != nil:
			enum, ok := val.(Enumerable)
			if !ok {
				return nil, fmt.Errorf("cannot sub-select %T", val)
			}
			// TODO arrays of arrays
			results := []any{} // TODO subtle: favor [] over null result
			for nth := 1; nth <= enum.Len(); nth++ {
				val, err := enum.Nth(nth)
				if err != nil {
					return nil, err
				}
				node, err := s.toSelectable(chainedID.Nth(nth), val)
				if err != nil {
					return nil, fmt.Errorf("instantiate %dth array element: %w", nth, err)
				}
				res, err := s.Resolve(ctx, node, sel.Subselections...)
				if err != nil {
					return nil, err
				}
				results = append(results, res)
			}
			res = results
		default:
			return nil, fmt.Errorf("cannot sub-select %T", val)
		}
	}

	if sel.Selector.Nth != 0 {
		enum, ok := res.(Enumerable)
		if !ok {
			return nil, fmt.Errorf("cannot sub-select %dth item from %T", sel.Selector.Nth, val)
		}
		return enum.Nth(sel.Selector.Nth)
	}

	return res, nil
}

func (s *Server) field(typeName, fieldName string) (*ast.FieldDefinition, error) {
	classes, ok := s.classes[typeName]
	if !ok {
		return nil, fmt.Errorf("unknown type: %q", typeName)
	}
	fieldDef, ok := classes.FieldDefinition(fieldName)
	if !ok {
		return nil, fmt.Errorf("unknown field: %q", fieldName)
	}
	return fieldDef, nil
}

func (s *Server) fromLiteral(ctx context.Context, lit *idproto.Literal) (Typed, error) {
	switch v := lit.Value.(type) {
	case *idproto.Literal_Id:
		id := v.Id
		class, ok := s.classes[id.TypeName]
		if !ok {
			return nil, fmt.Errorf("unknown class: %q", id.TypeName)
		}
		return class.NewID(id), nil
	case *idproto.Literal_Int:
		return NewInt(int(v.Int)), nil
	case *idproto.Literal_Float:
		return NewFloat(v.Float), nil
	case *idproto.Literal_String_:
		return NewString(v.String_), nil
	case *idproto.Literal_Bool:
		return NewBoolean(v.Bool), nil
	case *idproto.Literal_List:
		list := make(Array[Typed], len(v.List.Values))
		for i, val := range v.List.Values {
			typed, err := s.fromLiteral(ctx, val)
			if err != nil {
				return nil, err
			}
			list[i] = typed
		}
		return list, nil
	case *idproto.Literal_Object:
		return nil, fmt.Errorf("TODO: objects")
	default:
		panic(fmt.Sprintf("unsupported literal type %T", v))
	}
}

func (s *Server) toSelectable(chainedID *idproto.ID, val Typed) (Object, error) {
	if sel, ok := val.(Object); ok {
		// We always support returning something that's already Selectable, e.g. an
		// object loaded from its ID.
		return sel, nil
	}
	class, ok := s.classes[val.Type().Name()]
	if !ok {
		return nil, fmt.Errorf("unknown type %q", val.Type().Name())
	}
	return class.New(chainedID, val)
}

// Selection represents a selection of a field on an object.
type Selection struct {
	Alias         string
	Selector      Selector
	Subselections []Selection
}

// Name returns the name of the selection, which is either the alias or the
// field name.
func (sel Selection) Name() string {
	if sel.Alias != "" {
		return sel.Alias
	}
	return sel.Selector.Field
}
