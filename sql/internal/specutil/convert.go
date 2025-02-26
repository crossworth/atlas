// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package specutil

import (
	"fmt"
	"strconv"
	"strings"

	"ariga.io/atlas/schemahcl"
	"ariga.io/atlas/sql/internal/sqlx"
	"ariga.io/atlas/sql/schema"
	"ariga.io/atlas/sql/sqlspec"

	"github.com/zclconf/go-cty/cty"
)

// List of convert function types.
type (
	ConvertTableFunc       func(*sqlspec.Table, *schema.Schema) (*schema.Table, error)
	ConvertTableColumnFunc func(*sqlspec.Column, *schema.Table) (*schema.Column, error)
	ConvertViewFunc        func(*sqlspec.View, *schema.Schema) (*schema.View, error)
	ConvertViewColumnFunc  func(*sqlspec.Column, *schema.View) (*schema.Column, error)
	ConvertTypeFunc        func(*sqlspec.Column) (schema.Type, error)
	ConvertPrimaryKeyFunc  func(*sqlspec.PrimaryKey, *schema.Table) (*schema.Index, error)
	ConvertIndexFunc       func(*sqlspec.Index, *schema.Table) (*schema.Index, error)
	ConvertViewIndexFunc   func(*sqlspec.Index, *schema.View) (*schema.Index, error)
	ConvertCheckFunc       func(*sqlspec.Check) (*schema.Check, error)
	ColumnTypeSpecFunc     func(schema.Type) (*sqlspec.Column, error)
	TableSpecFunc          func(*schema.Table) (*sqlspec.Table, error)
	TableColumnSpecFunc    func(*schema.Column, *schema.Table) (*sqlspec.Column, error)
	ViewSpecFunc           func(*schema.View) (*sqlspec.View, error)
	ViewColumnSpecFunc     func(*schema.Column, *schema.View) (*sqlspec.Column, error)
	PrimaryKeySpecFunc     func(*schema.Index) (*sqlspec.PrimaryKey, error)
	IndexSpecFunc          func(*schema.Index) (*sqlspec.Index, error)
	ForeignKeySpecFunc     func(*schema.ForeignKey) (*sqlspec.ForeignKey, error)
	CheckSpecFunc          func(*schema.Check) *sqlspec.Check
)

type (
	// ScanDoc represents a scanned HCL document.
	ScanDoc struct {
		Schemas      []*sqlspec.Schema
		Tables       []*sqlspec.Table
		Views        []*sqlspec.View
		Materialized []*sqlspec.View
		Funcs        []*sqlspec.Func
		Procs        []*sqlspec.Func
	}

	// ScanFuncs represents a set of scan functions
	// used to convert the HCL document to the Realm.
	ScanFuncs struct {
		Table ConvertTableFunc
		View  ConvertViewFunc
		Func  func(*sqlspec.Func) (*schema.Func, error)
		Proc  func(*sqlspec.Func) (*schema.Proc, error)
	}

	// Funcs represents a set of spec functions
	// used to convert the Realm to an HCL document.
	Funcs struct {
		Table TableSpecFunc
		View  ViewSpecFunc
		Func  func(*schema.Func) (*sqlspec.Func, error)
		Proc  func(*schema.Proc) (*sqlspec.Func, error)
	}
)

const (
	typeView         = "view"
	typeTable        = "table"
	typeColumn       = "column"
	typeSchema       = "schema"
	typeMaterialized = "materialized"
)

// Scan populates the Realm from the schemas and table specs.
func Scan(r *schema.Realm, doc *ScanDoc, funcs *ScanFuncs) error {
	byName := make(map[string]*schema.Schema)
	for _, s := range doc.Schemas {
		s1 := schema.New(s.Name)
		if err := convertCommentFromSpec(s, &s1.Attrs); err != nil {
			return err
		}
		r.AddSchemas(s1)
		byName[s.Name] = s1
	}
	tableFKs := make(map[*schema.Table][]*sqlspec.ForeignKey)
	for _, st := range doc.Tables {
		name, err := SchemaName(st.Schema)
		if err != nil {
			return fmt.Errorf("specutil: cannot extract schema name for table %q: %w", st.Name, err)
		}
		s, ok := byName[name]
		if !ok {
			return fmt.Errorf("specutil: schema %q not found for table %q", name, st.Name)
		}
		t, err := funcs.Table(st, s)
		if err != nil {
			return fmt.Errorf("specutil: cannot convert table %q: %w", st.Name, err)
		}
		tableFKs[t] = st.ForeignKeys
		s.AddTables(t)
	}
	// Link the foreign keys.
	for t, fks := range tableFKs {
		if err := linkForeignKeys(t, fks); err != nil {
			return err
		}
	}
	viewDeps := make(map[*schema.View][]*schemahcl.Ref, len(doc.Views))
	for _, sv := range doc.Views {
		name, err := SchemaName(sv.Schema)
		if err != nil {
			return fmt.Errorf("specutil: cannot extract schema name for view %q: %w", sv.Name, err)
		}
		s, ok := byName[name]
		if !ok {
			return fmt.Errorf("specutil: schema %q not found for view %q", name, sv.Name)
		}
		v, err := funcs.View(sv, s)
		if err != nil {
			return fmt.Errorf("specutil: cannot convert view %q: %w", sv.Name, err)
		}
		s.AddViews(v)
		if deps, ok := sv.Attr("depends_on"); ok {
			refs, err := deps.Refs()
			if err != nil {
				return fmt.Errorf("specutil: expect list of references for attribute view.%s.depends_on: %w", sv.Name, err)
			}
			viewDeps[v] = refs
		}
	}
	for _, m := range doc.Materialized {
		name, err := SchemaName(m.Schema)
		if err != nil {
			return fmt.Errorf("specutil: cannot extract schema name for materialized %q: %w", m.Name, err)
		}
		s, ok := byName[name]
		if !ok {
			return fmt.Errorf("specutil: schema %q not found for materialized %q", name, m.Name)
		}
		v, err := funcs.View(m, s)
		if err != nil {
			return fmt.Errorf("specutil: cannot convert materialized %q: %w", m.Name, err)
		}
		s.AddViews(v.SetMaterialized(true))
		if deps, ok := m.Attr("depends_on"); ok {
			refs, err := deps.Refs()
			if err != nil {
				return fmt.Errorf("specutil: expect list of references for attribute materialized.%s.depends_on: %w", m.Name, err)
			}
			viewDeps[v] = refs
		}
	}
	// Link views' dependencies.
	for v, refs := range viewDeps {
		srcT := typeView
		if v.Materialized() {
			srcT = typeMaterialized
		}
		for i, r := range refs {
			switch p, err := r.Path(); {
			case err != nil:
				return fmt.Errorf("specutil: extract reference for %s.%s: %w", srcT, v.Name, err)
			case len(p) == 0:
				return fmt.Errorf("specutil: empty reference for %s.%s", srcT, v.Name)
			case p[0].T == typeView:
				q, n, err := refName(r, typeView)
				if err != nil {
					return fmt.Errorf("specutil: extract view name from %s.%s.depends_on[%d]: %w", srcT, v.Name, i, err)
				}
				v1, err := findT(v.Schema, q, n, func(s *schema.Schema, name string) (*schema.View, bool) {
					return s.View(name)
				})
				if err != nil {
					return fmt.Errorf("specutil: find view refrence for %s.%s.depends_on[%d]: %w", srcT, v.Name, i, err)
				}
				v.AddDeps(v1)
			case p[0].T == typeMaterialized:
				q, n, err := refName(r, typeMaterialized)
				if err != nil {
					return fmt.Errorf("specutil: extract materialized name from %s.%s.depends_on[%d]: %w", srcT, v.Name, i, err)
				}
				v1, err := findT(v.Schema, q, n, func(s *schema.Schema, name string) (*schema.View, bool) {
					return s.Materialized(name)
				})
				if err != nil {
					return fmt.Errorf("specutil: find materialized refrence for %s.%s.depends_on[%d]: %w", srcT, v.Name, i, err)
				}
				v.AddDeps(v1)
			case p[0].T == typeTable:
				q, n, err := refName(r, typeTable)
				if err != nil {
					return fmt.Errorf("specutil: extract table name from %s.%s.depends_on[%d]: %w", srcT, v.Name, i, err)
				}
				t1, err := findT(v.Schema, q, n, func(s *schema.Schema, name string) (*schema.Table, bool) {
					return s.Table(name)
				})
				if err != nil {
					return fmt.Errorf("specutil: find table refrence for %s.%s.depends_on[%d]: %w", srcT, v.Name, i, err)
				}
				v.AddDeps(t1)
			}
		}
	}
	if funcs.Func != nil {
		for _, sf := range doc.Funcs {
			name, err := SchemaName(sf.Schema)
			if err != nil {
				return fmt.Errorf("specutil: cannot extract schema name for function %q: %w", sf.Name, err)
			}
			s, ok := byName[name]
			if !ok {
				return fmt.Errorf("specutil: schema %q not found for function %q", name, sf.Name)
			}
			f, err := funcs.Func(sf)
			if err != nil {
				return fmt.Errorf("specutil: cannot convert function %q: %w", sf.Name, err)
			}
			s.AddFuncs(f)
		}
	}
	if funcs.Proc != nil {
		for _, sf := range doc.Procs {
			name, err := SchemaName(sf.Schema)
			if err != nil {
				return fmt.Errorf("specutil: cannot extract schema name for procedure %q: %w", sf.Name, err)
			}
			s, ok := byName[name]
			if !ok {
				return fmt.Errorf("specutil: schema %q not found for procedure %q", name, sf.Name)
			}
			f, err := funcs.Proc(sf)
			if err != nil {
				return fmt.Errorf("specutil: cannot convert procedure %q: %w", sf.Name, err)
			}
			s.AddProcs(f)
		}
	}
	return nil
}

// Table converts a sqlspec.Table to a schema.Table. Table conversion is done without converting
// ForeignKeySpecs into ForeignKeys, as the target tables do not necessarily exist in the schema
// at this point. Instead, the linking is done by the Schema function.
func Table(spec *sqlspec.Table, parent *schema.Schema, convertColumn ConvertTableColumnFunc,
	convertPK ConvertPrimaryKeyFunc, convertIndex ConvertIndexFunc, convertCheck ConvertCheckFunc) (*schema.Table, error) {
	t := &schema.Table{
		Name:   spec.Name,
		Schema: parent,
	}
	for _, csp := range spec.Columns {
		col, err := convertColumn(csp, t)
		if err != nil {
			return nil, err
		}
		t.AddColumns(col)
	}
	if spec.PrimaryKey != nil {
		pk, err := convertPK(spec.PrimaryKey, t)
		if err != nil {
			return nil, err
		}
		t.SetPrimaryKey(pk)
	}
	for _, idx := range spec.Indexes {
		i, err := convertIndex(idx, t)
		if err != nil {
			return nil, err
		}
		t.AddIndexes(i)
	}
	for _, c := range spec.Checks {
		c, err := convertCheck(c)
		if err != nil {
			return nil, err
		}
		t.AddChecks(c)
	}
	if err := convertCommentFromSpec(spec, &t.Attrs); err != nil {
		return nil, err
	}
	return t, nil
}

// View converts a sqlspec.View to a schema.View.
func View(spec *sqlspec.View, parent *schema.Schema, convertC ConvertViewColumnFunc, convertI ConvertViewIndexFunc) (*schema.View, error) {
	as, ok := spec.Extra.Attr("as")
	if !ok {
		return nil, fmt.Errorf("specutil: missing 'as' definition for view %q", spec.Name)
	}
	def, err := as.String()
	if err != nil {
		return nil, fmt.Errorf("specutil: expect string definition for attribute view.%s.as: %w", spec.Name, err)
	}
	v := schema.NewView(spec.Name, def).SetSchema(parent)
	for _, c := range spec.Columns {
		c, err := convertC(c, v)
		if err != nil {
			return nil, err
		}
		v.AddColumns(c)
	}
	for _, idx := range spec.Indexes {
		i, err := convertI(idx, v)
		if err != nil {
			return nil, err
		}
		v.AddIndexes(i)
	}
	if err := convertCommentFromSpec(spec, &v.Attrs); err != nil {
		return nil, err
	}
	if c, ok := spec.Extra.Attr("check_option"); ok {
		o, err := c.String()
		if err != nil {
			return nil, fmt.Errorf("specutil: expect string definition for attribute view.%s.check_option: %w", spec.Name, err)
		}
		v.SetCheckOption(o)
	}
	return v, nil
}

// Column converts a sqlspec.Column into a schema.Column.
func Column(spec *sqlspec.Column, conv ConvertTypeFunc) (*schema.Column, error) {
	out := &schema.Column{
		Name: spec.Name,
		Type: &schema.ColumnType{
			Null: spec.Null,
		},
	}
	d, err := Default(spec.Default)
	if err != nil {
		return nil, err
	}
	out.Default = d
	ct, err := conv(spec)
	if err != nil {
		return nil, err
	}
	out.Type.Type = ct
	if err := convertCommentFromSpec(spec, &out.Attrs); err != nil {
		return nil, err
	}
	return out, err
}

// Default converts a cty.Value (as defined in the spec) into a schema.Expr.
func Default(d cty.Value) (schema.Expr, error) {
	if d.IsNull() {
		return nil, nil // no default.
	}
	var x schema.Expr
	switch {
	case d.Type() == cty.String:
		x = &schema.Literal{V: d.AsString()}
	case d.Type() == cty.Number:
		x = &schema.Literal{V: d.AsBigFloat().String()}
	case d.Type() == cty.Bool:
		x = &schema.Literal{V: strconv.FormatBool(d.True())}
	case d.Type().IsCapsuleType():
		raw, ok := d.EncapsulatedValue().(*schemahcl.RawExpr)
		if !ok {
			return nil, fmt.Errorf("invalid default value %q", d.Type().FriendlyName())
		}
		x = &schema.RawExpr{X: raw.X}
	default:
		return nil, fmt.Errorf("unsupported value type for default: %T", d)
	}
	return x, nil
}

// Index converts a sqlspec.Index to a schema.Index. The optional arguments allow
// passing functions for mutating the created index-part (e.g. add attributes).
func Index(spec *sqlspec.Index, parent *schema.Table, partFns ...func(*sqlspec.IndexPart, *schema.IndexPart) error) (*schema.Index, error) {
	parts := make([]*schema.IndexPart, 0, len(spec.Columns)+len(spec.Parts))
	switch n, m := len(spec.Columns), len(spec.Parts); {
	case n == 0 && m == 0:
		return nil, fmt.Errorf("missing definition for index %q", spec.Name)
	case n > 0 && m > 0:
		return nil, fmt.Errorf(`multiple definitions for index %q, use "columns" or "on"`, spec.Name)
	case n > 0:
		for i, c := range spec.Columns {
			c, err := ColumnByRef(parent, c)
			if err != nil {
				return nil, err
			}
			parts = append(parts, &schema.IndexPart{
				SeqNo: i,
				C:     c,
			})
		}
	case m > 0:
		for i, p := range spec.Parts {
			part := &schema.IndexPart{SeqNo: i, Desc: p.Desc}
			switch {
			case p.Column == nil && p.Expr == "":
				return nil, fmt.Errorf(`"column" or "expr" are required for index %q at position %d`, spec.Name, i)
			case p.Column != nil && p.Expr != "":
				return nil, fmt.Errorf(`cannot use both "column" and "expr" in index %q at position %d`, spec.Name, i)
			case p.Expr != "":
				part.X = &schema.RawExpr{X: p.Expr}
			case p.Column != nil:
				c, err := ColumnByRef(parent, p.Column)
				if err != nil {
					return nil, err
				}
				part.C = c
			}
			for _, f := range partFns {
				if err := f(p, part); err != nil {
					return nil, err
				}
			}
			parts = append(parts, part)
		}
	}
	i := &schema.Index{
		Name:   spec.Name,
		Unique: spec.Unique,
		Table:  parent,
		Parts:  parts,
	}
	if err := convertCommentFromSpec(spec, &i.Attrs); err != nil {
		return nil, err
	}
	return i, nil
}

// Check converts a sqlspec.Check to a schema.Check.
func Check(spec *sqlspec.Check) (*schema.Check, error) {
	return &schema.Check{
		Name: spec.Name,
		Expr: spec.Expr,
	}, nil
}

// PrimaryKey converts a sqlspec.PrimaryKey to a schema.Index.
func PrimaryKey(spec *sqlspec.PrimaryKey, parent *schema.Table) (*schema.Index, error) {
	parts := make([]*schema.IndexPart, 0, len(spec.Columns))
	for seqno, c := range spec.Columns {
		c, err := ColumnByRef(parent, c)
		if err != nil {
			return nil, nil
		}
		parts = append(parts, &schema.IndexPart{
			SeqNo: seqno,
			C:     c,
		})
	}
	return &schema.Index{
		Table: parent,
		Parts: parts,
	}, nil
}

// linkForeignKeys creates the foreign keys defined in the Table's spec by creating references
// to column in the provided Schema. It is assumed that all tables referenced FK definitions in the spec
// are reachable from the provided schema or its connected realm.
func linkForeignKeys(tbl *schema.Table, fks []*sqlspec.ForeignKey) error {
	for _, spec := range fks {
		fk := &schema.ForeignKey{Symbol: spec.Symbol, Table: tbl}
		if spec.OnUpdate != nil {
			fk.OnUpdate = schema.ReferenceOption(FromVar(spec.OnUpdate.V))
		}
		if spec.OnDelete != nil {
			fk.OnDelete = schema.ReferenceOption(FromVar(spec.OnDelete.V))
		}
		if n, m := len(spec.Columns), len(spec.RefColumns); n != m {
			return fmt.Errorf("sqlspec: number of referencing and referenced columns do not match for foreign-key %q", fk.Symbol)
		}
		for _, ref := range spec.Columns {
			c, err := ColumnByRef(tbl, ref)
			if err != nil {
				return err
			}
			fk.Columns = append(fk.Columns, c)
		}
		for i, ref := range spec.RefColumns {
			t, c, err := externalRef(ref, tbl.Schema)
			if isLocalRef(ref) {
				t = fk.Table
				c, err = ColumnByRef(fk.Table, ref)
			}
			if err != nil {
				return err
			}
			if i > 0 && fk.RefTable != t {
				return fmt.Errorf("sqlspec: more than 1 table was referenced for foreign-key %q", fk.Symbol)
			}
			fk.RefTable = t
			fk.RefColumns = append(fk.RefColumns, c)
		}
		tbl.ForeignKeys = append(tbl.ForeignKeys, fk)
	}
	return nil
}

// FromSchema converts a schema.Schema into sqlspec.Schema and []sqlspec.Table.
func FromSchema(s *schema.Schema, funcs *Funcs) (*SchemaSpec, error) {
	spec := &SchemaSpec{
		Schema: &sqlspec.Schema{
			Name: s.Name,
		},
		Tables:       make([]*sqlspec.Table, 0, len(s.Tables)),
		Views:        make([]*sqlspec.View, 0, len(s.Views)),
		Materialized: make([]*sqlspec.View, 0, len(s.Views)),
	}
	for _, t := range s.Tables {
		table, err := funcs.Table(t)
		if err != nil {
			return nil, err
		}
		if s.Name != "" {
			table.Schema = SchemaRef(s.Name)
		}
		spec.Tables = append(spec.Tables, table)
	}
	for _, v := range s.Views {
		view, err := funcs.View(v)
		if err != nil {
			return nil, err
		}
		if s.Name != "" {
			view.Schema = SchemaRef(s.Name)
		}
		if v.Materialized() {
			spec.Materialized = append(spec.Materialized, view)
		} else {
			spec.Views = append(spec.Views, view)
		}
	}
	if funcs.Func != nil {
		for _, f := range s.Funcs {
			fn, err := funcs.Func(f)
			if err != nil {
				return nil, err
			}
			if s.Name != "" {
				fn.Schema = SchemaRef(s.Name)
			}
			spec.Funcs = append(spec.Funcs, fn)
		}
	}
	if funcs.Proc != nil {
		for _, p := range s.Procs {
			pr, err := funcs.Proc(p)
			if err != nil {
				return nil, err
			}
			if s.Name != "" {
				pr.Schema = SchemaRef(s.Name)
			}
			spec.Procs = append(spec.Procs, pr)
		}
	}
	convertCommentFromSchema(s.Attrs, &spec.Schema.Extra.Attrs)
	return spec, nil
}

// FromTable converts a schema.Table to a sqlspec.Table.
func FromTable(t *schema.Table, colFn TableColumnSpecFunc, pkFn PrimaryKeySpecFunc, idxFn IndexSpecFunc,
	fkFn ForeignKeySpecFunc, ckFn CheckSpecFunc) (*sqlspec.Table, error) {
	spec := &sqlspec.Table{
		Name: t.Name,
	}
	for _, c := range t.Columns {
		col, err := colFn(c, t)
		if err != nil {
			return nil, err
		}
		spec.Columns = append(spec.Columns, col)
	}
	if t.PrimaryKey != nil {
		pk, err := pkFn(t.PrimaryKey)
		if err != nil {
			return nil, err
		}
		spec.PrimaryKey = pk
	}
	for _, idx := range t.Indexes {
		i, err := idxFn(idx)
		if err != nil {
			return nil, err
		}
		spec.Indexes = append(spec.Indexes, i)
	}
	for _, fk := range t.ForeignKeys {
		f, err := fkFn(fk)
		if err != nil {
			return nil, err
		}
		spec.ForeignKeys = append(spec.ForeignKeys, f)
	}
	for _, attr := range t.Attrs {
		if c, ok := attr.(*schema.Check); ok {
			spec.Checks = append(spec.Checks, ckFn(c))
		}
	}
	convertCommentFromSchema(t.Attrs, &spec.Extra.Attrs)
	return spec, nil
}

// FromView converts a schema.View to a sqlspec.View.
func FromView(v *schema.View, colFn ViewColumnSpecFunc, idxFn IndexSpecFunc) (*sqlspec.View, error) {
	spec := &sqlspec.View{
		Name: v.Name,
	}
	for _, c := range v.Columns {
		cs, err := colFn(c, v)
		if err != nil {
			return nil, err
		}
		spec.Columns = append(spec.Columns, cs)
	}
	for _, idx := range v.Indexes {
		i, err := idxFn(idx)
		if err != nil {
			return nil, err
		}
		spec.Indexes = append(spec.Indexes, i)
	}
	as := v.Def
	// In case the view definition is multi-line,
	// format it as indented heredoc with two spaces.
	if lines := strings.Split(v.Def, "\n"); len(lines) > 1 {
		as = fmt.Sprintf("<<-SQL\n  %s\n  SQL", strings.Join(lines, "\n  "))
	}
	embed := &schemahcl.Resource{
		Attrs: []*schemahcl.Attr{
			schemahcl.StringAttr("as", as),
		},
	}
	if c := (schema.ViewCheckOption{}); sqlx.Has(v.Attrs, &c) {
		switch strings.ToUpper(c.V) {
		case schema.ViewCheckOptionNone, "":
		case schema.ViewCheckOptionLocal, schema.ViewCheckOptionCascaded:
			embed.Attrs = append(embed.Attrs, VarAttr("check_option", c.V))
		default:
			embed.Attrs = append(embed.Attrs, schemahcl.StringAttr("check_option", c.V))
		}
	}
	var (
		deps         = make([]*schemahcl.Ref, 0, len(v.Deps))
		nameT, nameV = make(map[string]int), make(map[string]int)
	)
	// Qualify table/view names if there are
	// multiple tables/views with the same name.
	if v.Schema.Realm != nil {
		for _, s := range v.Schema.Realm.Schemas {
			for _, t := range s.Tables {
				nameT[t.Name]++
			}
			for _, v := range s.Views {
				nameV[v.Name]++
			}
		}
	}
	for _, d := range v.Deps {
		path := make([]string, 0, 2)
		switch d := d.(type) {
		case *schema.Table:
			if nameT[d.Name] > 1 {
				path = append(path, d.Schema.Name)
			}
			deps = append(deps, schemahcl.BuildRef([]schemahcl.PathIndex{
				{T: typeTable, V: append(path, d.Name)},
			}))
		case *schema.View:
			vt := typeView
			if d.Materialized() {
				vt = typeMaterialized
			}
			if nameV[d.Name] > 1 {
				path = append(path, d.Schema.Name)
			}
			deps = append(deps, schemahcl.BuildRef([]schemahcl.PathIndex{
				{T: vt, V: append(path, d.Name)},
			}))
		}
	}
	if len(deps) > 0 {
		embed.Attrs = append(embed.Attrs, schemahcl.RefsAttr("depends_on", deps...))
	}
	convertCommentFromSchema(v.Attrs, &embed.Attrs)
	spec.Extra.Children = append(spec.Extra.Children, embed)
	return spec, nil
}

// FromPrimaryKey converts schema.Index to a sqlspec.PrimaryKey.
func FromPrimaryKey(s *schema.Index) (*sqlspec.PrimaryKey, error) {
	c := make([]*schemahcl.Ref, 0, len(s.Parts))
	for _, v := range s.Parts {
		c = append(c, ColumnRef(v.C.Name))
	}
	return &sqlspec.PrimaryKey{
		Columns: c,
	}, nil
}

// FromColumn converts a *schema.Column into a *sqlspec.Column using the ColumnTypeSpecFunc.
func FromColumn(col *schema.Column, columnTypeSpec ColumnTypeSpecFunc) (*sqlspec.Column, error) {
	ct, err := columnTypeSpec(col.Type.Type)
	if err != nil {
		return nil, err
	}
	spec := &sqlspec.Column{
		Name: col.Name,
		Type: ct.Type,
		Null: col.Type.Null,
		DefaultExtension: schemahcl.DefaultExtension{
			Extra: schemahcl.Resource{Attrs: ct.DefaultExtension.Extra.Attrs},
		},
	}
	if col.Default != nil {
		lv, err := ExprValue(col.Default)
		if err != nil {
			return nil, err
		}
		spec.Default = lv
	}
	convertCommentFromSchema(col.Attrs, &spec.Extra.Attrs)
	return spec, nil
}

// FromGenExpr returns the spec for a generated expression.
func FromGenExpr(x schema.GeneratedExpr, t func(string) string) *schemahcl.Resource {
	return &schemahcl.Resource{
		Type: "as",
		Attrs: []*schemahcl.Attr{
			schemahcl.StringAttr("expr", x.Expr),
			VarAttr("type", t(x.Type)),
		},
	}
}

// ConvertGenExpr converts the "as" attribute or the block under the given resource.
func ConvertGenExpr(r *schemahcl.Resource, c *schema.Column, t func(string) string) error {
	asA, okA := r.Attr("as")
	asR, okR := r.Resource("as")
	switch {
	case okA && okR:
		return fmt.Errorf("multiple as definitions for column %q", c.Name)
	case okA:
		expr, err := asA.String()
		if err != nil {
			return err
		}
		c.Attrs = append(c.Attrs, &schema.GeneratedExpr{
			Type: t(""), // default type.
			Expr: expr,
		})
	case okR:
		var spec struct {
			Expr string `spec:"expr"`
			Type string `spec:"type"`
		}
		if err := asR.As(&spec); err != nil {
			return err
		}
		c.Attrs = append(c.Attrs, &schema.GeneratedExpr{
			Expr: spec.Expr,
			Type: t(spec.Type),
		})
	}
	return nil
}

// ExprValue converts a schema.Expr to a cty.Value.
func ExprValue(expr schema.Expr) (cty.Value, error) {
	expr = schema.UnderlyingExpr(expr)
	switch x := expr.(type) {
	case *schema.RawExpr:
		return schemahcl.RawExprValue(&schemahcl.RawExpr{X: x.X}), nil
	case *schema.Literal:
		switch {
		case oneOfPrefix(x.V, "0x", "0X", "0b", "0B", "b'", "B'", "x'", "X'"):
			return schemahcl.RawExprValue(&schemahcl.RawExpr{X: x.V}), nil
		case sqlx.IsQuoted(x.V, '\'', '"'):
			// Normalize single quotes to double quotes.
			s, err := sqlx.Unquote(x.V)
			if err != nil {
				return cty.NilVal, err
			}
			return cty.StringVal(s), nil
		case strings.ToLower(x.V) == "true", strings.ToLower(x.V) == "false":
			return cty.BoolVal(strings.ToLower(x.V) == "true"), nil
		case strings.Contains(x.V, "."):
			f, err := strconv.ParseFloat(x.V, 64)
			if err != nil {
				return cty.NilVal, err
			}
			return cty.NumberFloatVal(f), nil
		case sqlx.IsLiteralNumber(x.V):
			i, err := strconv.ParseInt(x.V, 10, 64)
			if err != nil {
				return cty.NilVal, err
			}
			return cty.NumberIntVal(i), nil
		default:
			return cty.NilVal, fmt.Errorf("unsupported literal value %q", x.V)
		}
	default:
		return cty.NilVal, fmt.Errorf("converting expr %T to literal value", expr)
	}
}

// FromIndex converts schema.Index to sqlspec.Index.
func FromIndex(idx *schema.Index, partFns ...func(*schema.Index, *schema.IndexPart, *sqlspec.IndexPart) error) (*sqlspec.Index, error) {
	spec := &sqlspec.Index{Name: idx.Name, Unique: idx.Unique}
	convertCommentFromSchema(idx.Attrs, &spec.Extra.Attrs)
	spec.Parts = make([]*sqlspec.IndexPart, len(idx.Parts))
	for i, p := range idx.Parts {
		part := &sqlspec.IndexPart{Desc: p.Desc}
		switch {
		case p.C == nil && p.X == nil:
			return nil, fmt.Errorf("missing column or expression for key part of index %q", idx.Name)
		case p.C != nil && p.X != nil:
			return nil, fmt.Errorf("multiple key part definitions for index %q", idx.Name)
		case p.C != nil:
			part.Column = ColumnRef(p.C.Name)
		case p.X != nil:
			x, ok := p.X.(*schema.RawExpr)
			if !ok {
				return nil, fmt.Errorf("unexpected expression %T for index %q", p.X, idx.Name)
			}
			part.Expr = x.X
		}
		for _, f := range partFns {
			if err := f(idx, p, part); err != nil {
				return nil, err
			}
		}
		spec.Parts[i] = part
	}
	if parts, ok := columnsOnly(spec.Parts); ok {
		spec.Parts = nil
		spec.Columns = parts
		return spec, nil
	}
	return spec, nil
}

func columnsOnly(parts []*sqlspec.IndexPart) ([]*schemahcl.Ref, bool) {
	columns := make([]*schemahcl.Ref, len(parts))
	for i, p := range parts {
		if p.Desc || p.Column == nil || len(p.Extra.Attrs) != 0 {
			return nil, false
		}
		columns[i] = p.Column
	}
	return columns, true
}

// FromForeignKey converts schema.ForeignKey to sqlspec.ForeignKey.
func FromForeignKey(s *schema.ForeignKey) (*sqlspec.ForeignKey, error) {
	c := make([]*schemahcl.Ref, 0, len(s.Columns))
	for _, v := range s.Columns {
		c = append(c, ColumnRef(v.Name))
	}
	r := make([]*schemahcl.Ref, 0, len(s.RefColumns))
	for _, v := range s.RefColumns {
		ref := ColumnRef(v.Name)
		if s.Table != s.RefTable {
			ref = externalColRef(v.Name, s.RefTable.Name)
		}
		r = append(r, ref)
	}
	fk := &sqlspec.ForeignKey{
		Symbol:     s.Symbol,
		Columns:    c,
		RefColumns: r,
	}
	if s.OnUpdate != "" {
		fk.OnUpdate = &schemahcl.Ref{V: Var(string(s.OnUpdate))}
	}
	if s.OnDelete != "" {
		fk.OnDelete = &schemahcl.Ref{V: Var(string(s.OnDelete))}
	}
	return fk, nil
}

// FromCheck converts schema.Check to sqlspec.Check.
func FromCheck(s *schema.Check) *sqlspec.Check {
	return &sqlspec.Check{
		Name: s.Name,
		Expr: s.Expr,
	}
}

// SchemaName returns the name from a ref to a schema.
func SchemaName(ref *schemahcl.Ref) (string, error) {
	vs, err := ref.ByType(typeSchema)
	if err != nil {
		return "", err
	}
	if len(vs) != 1 {
		return "", fmt.Errorf("specutil: expected 1 schema ref, got %d", len(vs))
	}
	return vs[0], nil
}

// ColumnByRef returns a column from the table by its reference.
func ColumnByRef(t *schema.Table, ref *schemahcl.Ref) (*schema.Column, error) {
	vs, err := ref.ByType(typeColumn)
	if err != nil {
		return nil, err
	}
	if len(vs) != 1 {
		return nil, fmt.Errorf("specutil: expected 1 column ref, got %d", len(vs))
	}
	c, ok := t.Column(vs[0])
	if !ok {
		return nil, fmt.Errorf("specutil: unknown column %q in table %q", vs[0], t.Name)
	}
	return c, nil
}

func externalRef(ref *schemahcl.Ref, sch *schema.Schema) (*schema.Table, *schema.Column, error) {
	qualifier, name, err := tableName(ref)
	if err != nil {
		return nil, nil, err
	}
	t, err := findT(sch, qualifier, name, func(s *schema.Schema, name string) (*schema.Table, bool) {
		return s.Table(name)
	})
	if err != nil {
		return nil, nil, err
	}
	c, err := ColumnByRef(t, ref)
	if err != nil {
		return nil, nil, err
	}
	return t, c, nil
}

// findT finds the table/view referenced by ref in the provided schema. If the table/view
// is not in the provided schema.Schema other schemas in the connected schema.Realm are
// searched as well.
func findT[T schema.View | schema.Table](sch *schema.Schema, qualifier, name string, findT func(*schema.Schema, string) (*T, bool)) (*T, error) {
	var (
		matches []*T             // Found references.
		schemas []*schema.Schema // Schemas to search.
	)
	switch {
	case sch.Realm == nil || qualifier == sch.Name:
		schemas = []*schema.Schema{sch}
	case qualifier == "":
		schemas = sch.Realm.Schemas
	default:
		s, ok := sch.Realm.Schema(qualifier)
		if ok {
			schemas = []*schema.Schema{s}
		}
	}
	for _, s := range schemas {
		t, ok := findT(s, name)
		if ok {
			matches = append(matches, t)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return nil, fmt.Errorf("specutil: refrenced table/view %q not found", name)
	default:
		return nil, fmt.Errorf("specutil: multiple refrences tables/views found for %q", name)
	}
}

func tableName(ref *schemahcl.Ref) (string, string, error) {
	return refName(ref, typeTable)
}

func refName(ref *schemahcl.Ref, typeName string) (qualifier, name string, err error) {
	vs, err := ref.ByType(typeName)
	if err != nil {
		return "", "", err
	}
	switch len(vs) {
	case 1:
		name = vs[0]
	case 2:
		qualifier, name = vs[0], vs[1]
	default:
		return "", "", fmt.Errorf("sqlspec: unexpected number of references in %q", vs)
	}
	return
}

func isLocalRef(r *schemahcl.Ref) bool {
	return strings.HasPrefix(r.V, "$column")
}

// ColumnRef returns the reference of a column by its name.
func ColumnRef(cName string) *schemahcl.Ref {
	return schemahcl.BuildRef([]schemahcl.PathIndex{
		{T: typeColumn, V: []string{cName}},
	})
}

func externalColRef(cName string, tName string) *schemahcl.Ref {
	return schemahcl.BuildRef([]schemahcl.PathIndex{
		{T: typeTable, V: []string{tName}},
		{T: typeColumn, V: []string{cName}},
	})
}

func qualifiedExternalColRef(cName, tName, sName string) *schemahcl.Ref {
	return schemahcl.BuildRef([]schemahcl.PathIndex{
		{T: typeTable, V: []string{sName, tName}},
		{T: typeColumn, V: []string{cName}},
	})
}

// SchemaRef returns the schemahcl.Ref to the schema with the given name.
func SchemaRef(name string) *schemahcl.Ref {
	return schemahcl.BuildRef([]schemahcl.PathIndex{
		{T: typeSchema, V: []string{name}},
	})
}

// Attrer is the interface that wraps the Attr method.
type Attrer interface {
	Attr(string) (*schemahcl.Attr, bool)
}

// convertCommentFromSpec converts a spec comment attribute to a schema element attribute.
func convertCommentFromSpec(spec Attrer, attrs *[]schema.Attr) error {
	if c, ok := spec.Attr("comment"); ok {
		s, err := c.String()
		if err != nil {
			return err
		}
		*attrs = append(*attrs, &schema.Comment{Text: s})
	}
	return nil
}

// convertCommentFromSchema converts a schema element comment attribute to a spec comment attribute.
func convertCommentFromSchema(src []schema.Attr, target *[]*schemahcl.Attr) {
	var c schema.Comment
	if sqlx.Has(src, &c) {
		*target = append(*target, schemahcl.StringAttr("comment", c.Text))
	}
}

// ReferenceVars holds the HCL variables
// for foreign keys' referential-actions.
var ReferenceVars = []string{
	Var(string(schema.NoAction)),
	Var(string(schema.Restrict)),
	Var(string(schema.Cascade)),
	Var(string(schema.SetNull)),
	Var(string(schema.SetDefault)),
}

// Var formats a string as variable to make it HCL compatible.
// The result is simple, replace each space with underscore.
func Var(s string) string { return strings.ReplaceAll(s, " ", "_") }

// FromVar is the inverse function of Var.
func FromVar(s string) string { return strings.ReplaceAll(s, "_", " ") }

func oneOfPrefix(s string, ps ...string) bool {
	for _, p := range ps {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
