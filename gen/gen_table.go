package gen

import (
	"fmt"
	"io"
	"strings"
	"text/template"

	"github.com/opendoor-labs/pggen/include"
)

// Generate code for all of the tables
func (g *Generator) genTables(into io.Writer, tables []tableConfig) error {
	if len(tables) > 0 {
		g.infof("	generating %d tables\n", len(tables))
	} else {
		return nil
	}

	g.imports[`"database/sql"`] = true
	g.imports[`"context"`] = true
	g.imports[`"fmt"`] = true
	g.imports[`"strings"`] = true
	g.imports[`"github.com/lib/pq"`] = true
	g.imports[`"github.com/opendoor-labs/pggen/include"`] = true
	g.imports[`"github.com/opendoor-labs/pggen"`] = true

	for _, table := range tables {
		err := g.genTable(into, &table)
		if err != nil {
			return err
		}
	}

	return nil
}

type tableGenCtx struct {
	// taken from tableMeta
	PgName string
	// taken from tableMeta
	GoName string
	// taken from tableMeta
	PkeyCol *colMeta
	// taken from tableMeta
	PkeyColIdx int
	// taken from tableMeta
	Cols []colMeta
	// taken from tableMeta
	References []refMeta
	// The include spec which represents the transitive closure of
	// this tables family
	AllIncludeSpec           string
	HasCreatedAtField        bool
	CreatedAtFieldIsNullable bool
	CreatedAtHasTimezone     bool
	CreatedAtField           string
	HasUpdatedAtField        bool
	UpdatedAtHasTimezone     bool
	UpdatedAtFieldIsNullable bool
	UpdatedAtField           string
}

func tableGenCtxFromInfo(info *tableGenInfo) tableGenCtx {
	return tableGenCtx{
		PgName:         info.meta.PgName,
		GoName:         info.meta.GoName,
		PkeyCol:        info.meta.PkeyCol,
		PkeyColIdx:     info.meta.PkeyColIdx,
		Cols:           info.meta.Cols,
		References:     info.meta.References,
		AllIncludeSpec: info.allIncludeSpec.String(),

		HasCreatedAtField:        info.hasCreatedAtField,
		CreatedAtField:           pgToGoName(info.config.CreatedAtField),
		CreatedAtFieldIsNullable: info.createdAtFieldIsNullable,
		CreatedAtHasTimezone:     info.createdAtHasTimezone,

		HasUpdatedAtField:        info.hasUpdateAtField,
		UpdatedAtField:           pgToGoName(info.config.UpdatedAtField),
		UpdatedAtFieldIsNullable: info.updatedAtFieldIsNullable,
		UpdatedAtHasTimezone:     info.updatedAtHasTimezone,
	}
}

func (g *Generator) genTable(
	into io.Writer,
	table *tableConfig,
) (err error) {
	g.infof("		generating table '%s'\n", table.Name)
	defer func() {
		if err != nil {
			err = fmt.Errorf(
				"while generating table '%s': %s", table.Name, err.Error())
		}
	}()

	tableInfo := g.tables[table.Name]

	genCtx := tableGenCtxFromInfo(tableInfo)
	if genCtx.PkeyCol == nil {
		err = fmt.Errorf("no primary key for table")
		return
	}

	// Filter out all the references from tables that are not
	// mentioned in the TOML, or have explicitly asked us not to
	// infer relationships. We only want to generate code about the
	// part of the database schema that we have been explicitly
	// asked to generate code for.
	kept := 0
	for _, ref := range genCtx.References {
		if fromTable, inMap := g.tables[ref.PointsFrom.PgName]; inMap {
			if !fromTable.config.NoInferBelongsTo {
				genCtx.References[kept] = ref
				kept++
			}
		}

		if len(ref.PointsFromFields) != 1 {
			err = fmt.Errorf("multi-column foreign keys not supported")
			return
		}
	}
	genCtx.References = genCtx.References[:kept]

	genCtx.References = append(
		genCtx.References,
		g.tables[table.Name].explicitBelongsTo...,
	)

	if tableInfo.hasUpdateAtField || tableInfo.hasCreatedAtField {
		g.imports[`"time"`] = true
	}

	// Emit the type seperately to prevent double defintions
	var tableType strings.Builder
	err = tableTypeTmpl.Execute(&tableType, genCtx)
	if err != nil {
		return
	}
	var tableSig strings.Builder
	err = tableTypeFieldSigTmpl.Execute(&tableSig, genCtx)
	if err != nil {
		return
	}
	err = g.types.emitType(genCtx.GoName, tableSig.String(), tableType.String())
	if err != nil {
		return
	}

	return tableShimTmpl.Execute(into, genCtx)
}

var tableTypeFieldSigTmpl *template.Template = template.Must(template.New("table-type-field-sig-tmpl").Parse(`
{{- range .Cols }}
{{- if .Nullable }}
{{ .GoName }} {{ .TypeInfo.NullName }}
{{- else }}
{{ .GoName }} {{ .TypeInfo.Name }}
{{- end }}
{{- end }}
`))

var tableTypeTmpl *template.Template = template.Must(template.New("table-type-tmpl").Parse(`
type {{ .GoName }} struct {
	{{- range .Cols }}
	{{- if .Nullable }}
	{{ .GoName }} {{ .TypeInfo.NullName }}
	{{- else }}
	{{ .GoName }} {{ .TypeInfo.Name }}
	{{- end }} ` +
	"`" + `gorm:"column:{{ .PgName }}"
	{{- if .IsPrimary }} gorm:"is_primary" {{- end }}` +
	"`" + `
	{{- end }}
	{{- range .References }}
	{{- if .OneToOne }}
	{{ .PointsFrom.GoName }} *{{ .PointsFrom.GoName }}
	{{- else }}
	{{ .PointsFrom.PluralGoName }} []*{{ .PointsFrom.GoName }}
	{{- end }}
	{{- end }}
}
func (r *{{ .GoName }}) Scan(ctx context.Context, client *PGClient, rs *sql.Rows) error {
	if client.colIdxTabFor{{ .GoName }} == nil {
		err := client.fillColPosTab(
			ctx,
			genTimeColIdxTabFor{{ .GoName }},
			` + "`" + `{{ .PgName }}` + "`" + `,
			&client.colIdxTabFor{{ .GoName }},
		)
		if err != nil {
			return err
		}
	}

	var nullableTgts nullableScanTgtsFor{{ .GoName }}

	scanTgts := make([]interface{}, len(client.colIdxTabFor{{ .GoName }}))
	for genIdx, runIdx := range client.colIdxTabFor{{ .GoName }} {
		scanTgts[runIdx] = scannerTabFor{{ .GoName }}[genIdx](r, &nullableTgts)
	}

	err := rs.Scan(scanTgts...)
	if err != nil {
		return err
	}

	{{- range .Cols }}
	{{- if .Nullable }}
	r.{{ .GoName }} = {{ call .TypeInfo.NullConvertFunc (printf "nullableTgts.scan%s" .GoName) }}
	{{- end }}
	{{- end }}

	return nil
}

type nullableScanTgtsFor{{ .GoName }} struct {
	{{- range .Cols }}
	{{- if .Nullable }}
	scan{{ .GoName }} {{ .TypeInfo.ScanNullName }}
	{{- end }}
	{{- end }}
}

// a table mapping codegen-time col indicies to functions returning a scanner for the
// field that was at that column index at codegen-time.
var scannerTabFor{{ .GoName }} = [...]func(*{{ .GoName }}, *nullableScanTgtsFor{{ .GoName }}) interface{} {
	{{- range .Cols }}
	func (
		r *{{ $.GoName }},
		nullableTgts *nullableScanTgtsFor{{ $.GoName }},
	) interface{} {
		{{- if .Nullable }}
		return {{ call .TypeInfo.SqlReceiver (printf "nullableTgts.scan%s" .GoName) }}
		{{- else }}
		return {{ call .TypeInfo.SqlReceiver (printf "r.%s" .GoName) }}
		{{- end }}
	},
	{{- end }}
}

var genTimeColIdxTabFor{{ .GoName }} map[string]int = map[string]int{
	{{- range $i, $col := .Cols }}
	` + "`" + `{{ $col.PgName }}` + "`" + `: {{ $i }},
	{{- end }}
}
`))

var tableShimTmpl *template.Template = template.Must(template.New("table-shim-tmpl").Parse(`

func (p *PGClient) Get{{ .GoName }}(
	ctx context.Context,
	id {{ .PkeyCol.TypeInfo.Name }},
) (*{{ .GoName }}, error) {
	return p.impl.Get{{ .GoName }}(ctx, id)
}
func (tx *TxPGClient) Get{{ .GoName }}(
	ctx context.Context,
	id {{ .PkeyCol.TypeInfo.Name }},
) (*{{ .GoName }}, error) {
	return tx.impl.Get{{ .GoName }}(ctx, id)
}
func (p *pgClientImpl) Get{{ .GoName }}(
	ctx context.Context,
	id {{ .PkeyCol.TypeInfo.Name }},
) (*{{ .GoName }}, error) {
	values, err := p.List{{ .GoName }}(ctx, []{{ .PkeyCol.TypeInfo.Name }}{id})
	if err != nil {
		return nil, err
	}

	// List{{ .GoName }} always returns the same number of records as were
	// requested, so this is safe.
	return &values[0], err
}

func (p *PGClient) List{{ .GoName }}(
	ctx context.Context,
	ids []{{ .PkeyCol.TypeInfo.Name }},
) (ret []{{ .GoName }}, err error) {
	return p.impl.List{{ .GoName }}(ctx, ids)
}
func (tx *TxPGClient) List{{ .GoName }}(
	ctx context.Context,
	ids []{{ .PkeyCol.TypeInfo.Name }},
) (ret []{{ .GoName }}, err error) {
	return tx.impl.List{{ .GoName }}(ctx, ids)
}
func (p *pgClientImpl) List{{ .GoName }}(
	ctx context.Context,
	ids []{{ .PkeyCol.TypeInfo.Name }},
) (ret []{{ .GoName }}, err error) {
	if len(ids) == 0 {
		return []{{ .GoName }}{}, nil
	}

	rows, err := p.db.QueryContext(
		ctx,
		"SELECT * FROM \"{{ .PgName }}\" WHERE \"{{ .PkeyCol.PgName }}\" = ANY($1)",
		pq.Array(ids),
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err == nil {
			err = rows.Close()
			if err != nil {
				ret = nil
			}
		} else {
			rowErr := rows.Close()
			if rowErr != nil {
				err = fmt.Errorf("%s AND %s", err.Error(), rowErr.Error())
			}
		}
	}()

	ret = make([]{{ .GoName }}, 0, len(ids))
	for rows.Next() {
		var value {{ .GoName }}
		err = value.Scan(ctx, p.client, rows)
		if err != nil {
			return nil, err
		}
		ret = append(ret, value)
	}

	if len(ret) != len(ids) {
		return nil, fmt.Errorf(
			"List{{ .GoName }}: asked for %d records, found %d",
			len(ids),
			len(ret),
		)
	}

	return ret, nil
}

// Insert a {{ .GoName }} into the database. Returns the primary
// key of the inserted row.
func (p *PGClient) Insert{{ .GoName }}(
	ctx context.Context,
	value *{{ .GoName }},
) (ret {{ .PkeyCol.TypeInfo.Name }}, err error) {
	return p.impl.Insert{{ .GoName }}(ctx, value)
}
// Insert a {{ .GoName }} into the database. Returns the primary
// key of the inserted row.
func (tx *TxPGClient) Insert{{ .GoName }}(
	ctx context.Context,
	value *{{ .GoName }},
) (ret {{ .PkeyCol.TypeInfo.Name }}, err error) {
	return tx.impl.Insert{{ .GoName }}(ctx, value)
}
// Insert a {{ .GoName }} into the database. Returns the primary
// key of the inserted row.
func (p *pgClientImpl) Insert{{ .GoName }}(
	ctx context.Context,
	value *{{ .GoName }},
) (ret {{ .PkeyCol.TypeInfo.Name }}, err error) {
	var ids []{{ .PkeyCol.TypeInfo.Name }}
	ids, err = p.BulkInsert{{ .GoName }}(ctx, []{{ .GoName }}{*value})
	if err != nil {
		return
	}

	if len(ids) != 1 {
		err = fmt.Errorf("inserting a {{ .GoName }}: %d ids (expected 1)", len(ids))
		return
	}

	ret = ids[0]
	return
}

// Insert a list of {{ .GoName }}. Returns a list of the primary keys of
// the inserted rows.
func (p *PGClient) BulkInsert{{ .GoName }}(
	ctx context.Context,
	values []{{ .GoName }},
) ([]{{ .PkeyCol.TypeInfo.Name }}, error) {
	return p.impl.BulkInsert{{ .GoName }}(ctx, values)
}
// Insert a list of {{ .GoName }}. Returns a list of the primary keys of
// the inserted rows.
func (tx *TxPGClient) BulkInsert{{ .GoName }}(
	ctx context.Context,
	values []{{ .GoName }},
) ([]{{ .PkeyCol.TypeInfo.Name }}, error) {
	return tx.impl.BulkInsert{{ .GoName }}(ctx, values)
}
// Insert a list of {{ .GoName }}. Returns a list of the primary keys of
// the inserted rows.
func (p *pgClientImpl) BulkInsert{{ .GoName }}(
	ctx context.Context,
	values []{{ .GoName }},
) ([]{{ .PkeyCol.TypeInfo.Name }}, error) {
	if len(values) == 0 {
		return []{{ .PkeyCol.TypeInfo.Name }}{}, nil
	}

	var fields []string = []string{
		{{- range .Cols }}
		{{- if (not .IsPrimary) }}
		` + "`" + `{{ .PgName }}` + "`" + `,
		{{- end }}
		{{- end }}
	}

	{{- if (or .HasCreatedAtField .HasUpdatedAtField) }}
	now := time.Now()
	{{- end }}

	{{- if .HasCreatedAtField }}
	for i := range values {
		{{- if .CreatedAtHasTimezone }}
		createdAt := now
		{{- else }}
		createdAt := now.UTC()
		{{- end }}

		{{- if .HasCreatedAtField }}
		{{- if .CreatedAtFieldIsNullable }}
		values[i].{{ .CreatedAtField }} = &createdAt
		{{- else }}
		values[i].{{ .CreatedAtField }} = createdAt
		{{- end }}
		{{- end }}
	}
	{{- end }}

	{{- if .HasUpdatedAtField }}
	for i := range values {
		{{- if .CreatedAtHasTimezone }}
		updatedAt := now
		{{- else }}
		updatedAt := now.UTC()
		{{- end }}

		{{- if .HasUpdatedAtField }}
		{{- if .UpdatedAtFieldIsNullable }}
		values[i].{{ .UpdatedAtField }} = &updatedAt
		{{- else }}
		values[i].{{ .UpdatedAtField }} = updatedAt
		{{- end }}
		{{- end }}
	}
	{{- end }}

	args := make([]interface{}, 0, {{ len .Cols }} * len(values))
	for _, v := range values {
		{{- range .Cols }}
		{{- if (not .IsPrimary) }}
		args = append(args, {{ call .TypeInfo.SqlArgument (printf "v.%s" .GoName) }})
		{{- end }}
		{{- end }}
	}

	bulkInsertQuery := genBulkInsertStmt(
		"{{ .PgName }}",
		fields,
		len(values),
		"{{ .PkeyCol.PgName }}",
	)

	rows, err := p.db.QueryContext(ctx, bulkInsertQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]{{ .PkeyCol.TypeInfo.Name }}, 0, len(values))
	for rows.Next() {
		var id {{ .PkeyCol.TypeInfo.Name }}
		err = rows.Scan({{ call .PkeyCol.TypeInfo.SqlReceiver "id" }})
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}

	return ids, nil
}

// bit indicies for 'fieldMask' parameters
const (
	{{- range $i, $c := .Cols }}
	{{ $.GoName }}{{ $c.GoName }}FieldIndex int = {{ $i }}
	{{- end }}
	{{ $.GoName }}MaxFieldIndex int = ({{ len .Cols }} - 1)
)

// A field set saying that all fields in {{ .GoName }} should be updated.
// For use as a 'fieldMask' parameter
var {{ .GoName }}AllFields pggen.FieldSet = pggen.NewFieldSetFilled({{ len .Cols }})

var fieldsFor{{ .GoName }} []string = []string{
	{{- range .Cols }}
	` + "`" + `{{ .PgName }}` + "`" + `,
	{{- end }}
}

// Update a {{ .GoName }}. 'value' must at the least have
// a primary key set. The 'fieldMask' field set indicates which fields
// should be updated in the database.
//
// Returns the primary key of the updated row.
func (p *PGClient) Update{{ .GoName }}(
	ctx context.Context,
	value *{{ .GoName }},
	fieldMask pggen.FieldSet,
) (ret {{ .PkeyCol.TypeInfo.Name }}, err error) {
	return p.impl.Update{{ .GoName }}(ctx, value, fieldMask)
}
// Update a {{ .GoName }}. 'value' must at the least have
// a primary key set. The 'fieldMask' field set indicates which fields
// should be updated in the database.
//
// Returns the primary key of the updated row.
func (tx *TxPGClient) Update{{ .GoName }}(
	ctx context.Context,
	value *{{ .GoName }},
	fieldMask pggen.FieldSet,
) (ret {{ .PkeyCol.TypeInfo.Name }}, err error) {
	return tx.impl.Update{{ .GoName }}(ctx, value, fieldMask)
}
func (p *pgClientImpl) Update{{ .GoName }}(
	ctx context.Context,
	value *{{ .GoName }},
	fieldMask pggen.FieldSet,
) (ret {{ .PkeyCol.TypeInfo.Name }}, err error) {
	if !fieldMask.Test({{ .GoName }}{{ .PkeyCol.GoName }}FieldIndex) {
		err = fmt.Errorf("primary key required for updates to '{{ .PgName }}'")
		return
	}

	{{- if .HasUpdatedAtField }}
	{{- if .UpdatedAtHasTimezone }}
	now := time.Now()
	{{- else }}
	now := time.Now().UTC()
	{{- end }}
	{{- if .UpdatedAtFieldIsNullable }}
	value.{{ .UpdatedAtField }} = &now
	{{- else }}
	value.{{ .UpdatedAtField }} = now
	{{- end }}
	fieldMask.Set({{ .GoName }}{{ .UpdatedAtField }}FieldIndex, true)
	{{- end }}

	updateStmt := genUpdateStmt(
		"{{ .PgName }}",
		"{{ .PkeyCol.PgName }}",
		fieldsFor{{ .GoName }},
		fieldMask,
		"{{ .PkeyCol.PgName }}",
	)

	args := make([]interface{}, 0, {{ len .Cols }})

	{{- range .Cols }}
	if fieldMask.Test({{ $.GoName }}{{ .GoName }}FieldIndex) {
		args = append(args, {{ call .TypeInfo.SqlArgument (printf "value.%s" .GoName) }})
	}
	{{- end }}

	// add the primary key arg for the WHERE condition
	args = append(args, value.{{ .PkeyCol.GoName }})

	var id {{ .PkeyCol.TypeInfo.Name }}
	err = p.db.QueryRowContext(ctx, updateStmt, args...).
                Scan({{ call .PkeyCol.TypeInfo.SqlReceiver "id" }})
	if err != nil {
		return
	}

	return id, nil
}

// Updsert a {{ .GoName }} value. If the given value conflicts with
// an existing row in the database, use the provided value to update that row
// rather than inserting it. Only the fields specified by 'fieldMask' are
// actually updated. All other fields are left as-is.
func (p *PGClient) Upsert{{ .GoName }}(
	ctx context.Context,
	value *{{ .GoName }},
	constraintNames []string,
	fieldMask pggen.FieldSet,
) (ret {{ .PkeyCol.TypeInfo.Name }}, err error) {
	var val []{{ .PkeyCol.TypeInfo.Name }}
	val, err = p.impl.BulkUpsert{{ .GoName }}(ctx, []{{ .GoName }}{*value}, constraintNames, fieldMask)
	if err != nil {
		return
	}
	if len(val) == 1 {
		return val[0], nil
	}

	// only possible if no upsert fields were specified by the field mask
	return value.{{ .PkeyCol.GoName }}, nil
}
// Updsert a {{ .GoName }} value. If the given value conflicts with
// an existing row in the database, use the provided value to update that row
// rather than inserting it. Only the fields specified by 'fieldMask' are
// actually updated. All other fields are left as-is.
func (tx *TxPGClient) Upsert{{ .GoName }}(
	ctx context.Context,
	value *{{ .GoName }},
	constraintNames []string,
	fieldMask pggen.FieldSet,
) (ret {{ .PkeyCol.TypeInfo.Name }}, err error) {
	var val []{{ .PkeyCol.TypeInfo.Name }}
	val, err = tx.impl.BulkUpsert{{ .GoName }}(ctx, []{{ .GoName }}{*value}, constraintNames, fieldMask)
	if err != nil {
		return
	}
	if len(val) == 1 {
		return val[0], nil
	}

	// only possible if no upsert fields were specified by the field mask
	return value.{{ .PkeyCol.GoName }}, nil
}


// Updsert a set of {{ .GoName }} values. If any of the given values conflict with
// existing rows in the database, use the provided values to update the rows which
// exist in the database rather than inserting them. Only the fields specified by
// 'fieldMask' are actually updated. All other fields are left as-is.
func (p *PGClient) BulkUpsert{{ .GoName }}(
	ctx context.Context,
	values []{{ .GoName }},
	constraintNames []string,
	fieldMask pggen.FieldSet,
) (ret []{{ .PkeyCol.TypeInfo.Name }}, err error) {
	return p.impl.BulkUpsert{{ .GoName }}(ctx, values, constraintNames, fieldMask)
}
// Updsert a set of {{ .GoName }} values. If any of the given values conflict with
// existing rows in the database, use the provided values to update the rows which
// exist in the database rather than inserting them. Only the fields specified by
// 'fieldMask' are actually updated. All other fields are left as-is.
func (tx *TxPGClient) BulkUpsert{{ .GoName }}(
	ctx context.Context,
	values []{{ .GoName }},
	constraintNames []string,
	fieldMask pggen.FieldSet,
) (ret []{{ .PkeyCol.TypeInfo.Name }}, err error) {
	return tx.impl.BulkUpsert{{ .GoName }}(ctx, values, constraintNames, fieldMask)
}
func (p *pgClientImpl) BulkUpsert{{ .GoName }}(
	ctx context.Context,
	values []{{ .GoName }},
	constraintNames []string,
	fieldMask pggen.FieldSet,
) ([]{{ .PkeyCol.TypeInfo.Name }}, error) {
	if len(values) == 0 {
		return []{{ .PkeyCol.TypeInfo.Name }}{}, nil
	}

	if constraintNames == nil || len(constraintNames) == 0 {
		constraintNames = []string{` + "`" + `{{ .PkeyCol.PgName }}` + "`" + `}
	}

	{{ if (or .HasCreatedAtField .HasUpdatedAtField) }}
	now := time.Now()

	{{- if .HasCreatedAtField }}
	{{- if .CreatedAtHasTimezone }}
	createdAt := now
	{{- else }}
	createdAt := now.UTC()
	{{- end }}
	for i := range values {
		{{- if .CreatedAtFieldIsNullable }}
		values[i].{{ .CreatedAtField }} = &createdAt
		{{- else }}
		values[i].{{ .CreatedAtField }} = createdAt
		{{- end }}
	}
	{{- end}}

	{{- if .HasUpdatedAtField }}
	{{- if .UpdatedAtHasTimezone }}
	updatedAt := now
	{{- else }}
	updatedAt := now.UTC()
	{{- end }}
	for i := range values {
		{{- if .UpdatedAtFieldIsNullable }}
		values[i].{{ .UpdatedAtField }} = &updatedAt
		{{- else }}
		values[i].{{ .UpdatedAtField }} = updatedAt
		{{- end }}
	}
	fieldMask.Set({{ .GoName }}{{ .UpdatedAtField }}FieldIndex, true)
	{{- end }}
	{{- end }}

	var stmt strings.Builder
	genInsertCommon(
		&stmt,
		` + "`" + `{{ .PgName }}` + "`" + `,
		fieldsFor{{ .GoName }},
		len(values),
		` + "`" + `{{ .PkeyCol.PgName }}` + "`" + `,
		fieldMask.Test({{ .GoName }}{{ .PkeyCol.GoName }}FieldIndex),
	)

	if fieldMask.CountSetBits() > 0 {
		stmt.WriteString("ON CONFLICT (")
		stmt.WriteString(strings.Join(constraintNames, ","))
		stmt.WriteString(") DO UPDATE SET ")

		updateCols := make([]string, 0, {{ len .Cols }})
		updateExprs := make([]string, 0, {{ len .Cols }})
		{{- range .Cols }}
		if fieldMask.Test({{ $.GoName }}{{ .GoName }}FieldIndex) {
			updateCols = append(updateCols, ` + "`" + `{{ .PgName }}` + "`" + `)
			updateExprs = append(updateExprs, ` + "`" + `excluded.{{ .PgName }}` + "`" + `)
		}
		{{- end }}
		if len(updateCols) > 1 {
			stmt.WriteRune('(')
		}
		stmt.WriteString(strings.Join(updateCols, ","))
		if len(updateCols) > 1 {
			stmt.WriteRune(')')
		}
		stmt.WriteString(" = ")
		if len(updateCols) > 1 {
			stmt.WriteRune('(')
		}
		stmt.WriteString(strings.Join(updateExprs, ","))
		if len(updateCols) > 1 {
			stmt.WriteRune(')')
		}
	} else {
		stmt.WriteString("ON CONFLICT DO NOTHING")
	}

	stmt.WriteString(` + "`" + ` RETURNING "{{ .PkeyCol.PgName }}"` + "`" + `)

	args := make([]interface{}, 0, {{ len .Cols }} * len(values))
	for _, v := range values {
		{{- range $i, $col := .Cols }}
		{{- if (eq $i $.PkeyColIdx) }}
		if fieldMask.Test({{ $.GoName }}{{ $col.GoName }}FieldIndex) {
			args = append(args, {{ call .TypeInfo.SqlArgument (printf "v.%s" $col.GoName) }})
		}
		{{- else }}
		args = append(args, {{ call .TypeInfo.SqlArgument (printf "v.%s" $col.GoName) }})
		{{- end }}
		{{- end }}
	}

	rows, err := p.db.QueryContext(ctx, stmt.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]{{ .PkeyCol.TypeInfo.Name }}, 0, len(values))
	for rows.Next() {
		var id {{ .PkeyCol.TypeInfo.Name }}
		err = rows.Scan({{ call .PkeyCol.TypeInfo.SqlReceiver "id" }})
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}

	return ids, nil
}

func (p *PGClient) Delete{{ .GoName }}(
	ctx context.Context,
	id {{ .PkeyCol.TypeInfo.Name }},
) error {
	return p.impl.BulkDelete{{ .GoName }}(ctx, []{{ .PkeyCol.TypeInfo.Name }}{id})
}
func (tx *TxPGClient) Delete{{ .GoName }}(
	ctx context.Context,
	id {{ .PkeyCol.TypeInfo.Name }},
) error {
	return tx.impl.BulkDelete{{ .GoName }}(ctx, []{{ .PkeyCol.TypeInfo.Name }}{id})
}

func (p *PGClient) BulkDelete{{ .GoName }}(
	ctx context.Context,
	ids []{{ .PkeyCol.TypeInfo.Name }},
) error {
	return p.impl.BulkDelete{{ .GoName }}(ctx, ids)
}
func (tx *TxPGClient) BulkDelete{{ .GoName }}(
	ctx context.Context,
	ids []{{ .PkeyCol.TypeInfo.Name }},
) error {
	return tx.impl.BulkDelete{{ .GoName }}(ctx, ids)
}
func (p *pgClientImpl) BulkDelete{{ .GoName }}(
	ctx context.Context,
	ids []{{ .PkeyCol.TypeInfo.Name }},
) error {
	if len(ids) == 0 {
		return nil
	}

	res, err := p.db.ExecContext(
		ctx,
		"DELETE FROM \"{{ .PgName }}\" WHERE \"{{ .PkeyCol.PgName }}\" = ANY($1)",
		pq.Array(ids),
	)
	if err != nil {
		return err
	}

	nrows, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if nrows != int64(len(ids)) {
		return fmt.Errorf(
			"BulkDelete{{ .GoName }}: %d rows deleted, expected %d",
			nrows,
			len(ids),
		)
	}

	return err
}

var {{ .GoName }}AllIncludes *include.Spec = include.Must(include.Parse(
	` + "`" + `{{ .AllIncludeSpec }}` + "`" + `,
))

func (p *PGClient) {{ .GoName }}FillIncludes(
	ctx context.Context,
	rec *{{ .GoName }},
	includes *include.Spec,
) error {
	return p.impl.{{ .GoName }}BulkFillIncludes(ctx, []*{{ .GoName }}{rec}, includes)
}
func (tx *TxPGClient) {{ .GoName }}FillIncludes(
	ctx context.Context,
	rec *{{ .GoName }},
	includes *include.Spec,
) error {
	return tx.impl.{{ .GoName }}BulkFillIncludes(ctx, []*{{ .GoName }}{rec}, includes)
}

func (p *PGClient) {{ .GoName }}BulkFillIncludes(
	ctx context.Context,
	recs []*{{ .GoName }},
	includes *include.Spec,
) error {
	return p.impl.{{ .GoName }}BulkFillIncludes(ctx, recs, includes)
}
func (tx *TxPGClient) {{ .GoName }}BulkFillIncludes(
	ctx context.Context,
	recs []*{{ .GoName }},
	includes *include.Spec,
) error {
	return tx.impl.{{ .GoName }}BulkFillIncludes(ctx, recs, includes)
}
func (p *pgClientImpl) {{ .GoName }}BulkFillIncludes(
	ctx context.Context,
	recs []*{{ .GoName }},
	includes *include.Spec,
) error {
	loadedRecordTab := map[string]interface{}{}

	return p.impl{{ .GoName }}BulkFillIncludes(ctx, recs, includes, loadedRecordTab)
}

func (p *pgClientImpl) impl{{ .GoName }}BulkFillIncludes(
	ctx context.Context,
	recs []*{{ .GoName }},
	includes *include.Spec,
	loadedRecordTab map[string]interface{},
) (err error) {
	if includes.TableName != "{{ .PgName }}" {
		return fmt.Errorf(
			"expected includes for '{{ .PgName }}', got '%s'",
			includes.TableName,
		)
	}

	loadedTab, inMap := loadedRecordTab[` + "`" + `{{ .PgName }}` + "`" + `]
	if inMap {
		idToRecord := loadedTab.(map[{{ .PkeyCol.TypeInfo.Name }}]*{{ .GoName }})
		for _, r := range recs {
			_, alreadyLoaded := idToRecord[r.{{ .PkeyCol.GoName }}]
			if !alreadyLoaded {
				idToRecord[r.{{ .PkeyCol.GoName }}] = r
			}
		}
	} else {
		idToRecord := make(map[{{ .PkeyCol.TypeInfo.Name }}]*{{ .GoName }}, len(recs))
		for _, r := range recs {
			idToRecord[r.{{ .PkeyCol.GoName }}] = r
		}
		loadedRecordTab[` + "`" + `{{ .PgName }}` + "`" + `] = idToRecord
	}

	{{- if .References }}
	var subSpec *include.Spec
	var inIncludeSet bool
	{{- end }}

	{{- range .References }}
	// Fill in the {{ .PointsFrom.PluralGoName }} if it is in includes
	subSpec, inIncludeSet = includes.Includes[` + "`" + `{{ .PointsFrom.PgName }}` + "`" + `]
	if inIncludeSet {
		err = p.private{{ $.GoName }}Fill{{ .PointsFrom.GoName }}(ctx, loadedRecordTab)
		if err != nil {
			return
		}

		subRecs := make([]*{{ .PointsFrom.GoName }}, 0, len(recs))
		for _, outer := range recs {
			{{- if .OneToOne }}
			if outer.{{ .PointsFrom.GoName }} != nil {
				subRecs = append(subRecs, outer.{{ .PointsFrom.GoName }})
			}
			{{- else }}
			for i := range outer.{{ .PointsFrom.PluralGoName }} {
				{{- if .Nullable }}
				if outer.{{ .PointsFrom.PluralGoName }}[i] == nil {
					continue
				}
				{{- end }}
				subRecs = append(subRecs, outer.{{ .PointsFrom.PluralGoName }}[i])
			}
			{{- end }}
		}

		err = p.impl{{ .PointsFrom.GoName }}BulkFillIncludes(ctx, subRecs, subSpec, loadedRecordTab)
		if err != nil {
			return
		}
	}
	{{- end }}

	return
}

{{- range .References }}

// For a give set of {{ $.GoName }}, fill in all the {{ .PointsFrom.GoName }}
// connected to them using a single query.
func (p *pgClientImpl) private{{ $.GoName }}Fill{{ .PointsFrom.GoName }}(
	ctx context.Context,
	loadedRecordTab map[string]interface{},
) error {
	parentLoadedTab, inMap := loadedRecordTab[` + "`" + `{{ .PointsTo.PgName }}` + "`" + `]
	if !inMap {
		return fmt.Errorf("internal pggen error: table not pre-loaded")
	}
	parentIDToRecord := parentLoadedTab.(map[{{ (index .PointsToFields 0).TypeInfo.Name }}]*{{ .PointsTo.GoName }})
	ids := make([]{{ (index .PointsToFields 0).TypeInfo.Name }}, 0, len(parentIDToRecord))
	for _, rec := range parentIDToRecord{
		ids = append(ids, rec.{{ (index .PointsToFields 0).GoName }})
	}

	var childIDToRecord map[{{ .PointsFrom.PkeyCol.TypeInfo.Name }}]*{{ .PointsFrom.GoName }}
	childLoadedTab, inMap := loadedRecordTab[` + "`" + `{{ .PointsFrom.PgName }}` + "`" + `]
	if inMap {
		childIDToRecord = childLoadedTab.(map[{{ .PointsFrom.PkeyCol.TypeInfo.Name }}]*{{ .PointsFrom.GoName }})
	} else {
		childIDToRecord = map[{{ .PointsFrom.PkeyCol.TypeInfo.Name }}]*{{ .PointsFrom.GoName }}{}
	}

	rows, err := p.db.QueryContext(
		ctx,
		` + "`" +
	`SELECT * FROM "{{ .PointsFrom.PgName }}"
		 WHERE "{{ (index .PointsFromFields 0).PgName }}" = ANY($1)
		 {{- if .OneToOne }}
		 LIMIT 1
		 {{- end }}
		 ` +
	"`" + `,
		pq.Array(ids),
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	// pull all the child records from the database and associate them with
	// the correct parent.
	for rows.Next() {
		var scannedChildRec {{ .PointsFrom.GoName }}
		err = scannedChildRec.Scan(ctx, p.client, rows)
		if err != nil {
			return err
		}

		var childRec *{{ .PointsFrom.GoName }}

		preloadedChildRec, alreadyLoaded := childIDToRecord[scannedChildRec.{{ .PointsFrom.PkeyCol.GoName }}]
		if alreadyLoaded {
			childRec = preloadedChildRec
		} else {
			childRec = &scannedChildRec
		}

		{{- if .Nullable }}
		// we know that the foreign key can't be null because of the SQL query
		parentRec := parentIDToRecord[*childRec.{{ (index .PointsFromFields 0).GoName }}]
		{{- else }}
		parentRec := parentIDToRecord[childRec.{{ (index .PointsFromFields 0).GoName }}]
		{{- end }}

		{{- if .OneToOne }}
		parentRec.{{ .PointsFrom.GoName }} = childRec
		break
		{{- else }}
		parentRec.{{ .PointsFrom.PluralGoName }} = append(parentRec.{{ .PointsFrom.PluralGoName }}, childRec)
		{{- end }}
	}

	return nil
}

{{ end }}
`))

// Information about tables required for code generation.
//
// The reason there is both a *Meta and *GenInfo struct for tables
// is that `tableMeta` is meant to be narrowly focused on metadata
// that postgres provides us, while things in `tableGenInfo` are
// more specific to `pggen`'s internal needs.
type tableGenInfo struct {
	config *tableConfig
	// Table relationships that have been explicitly configured
	// rather than infered from the database schema itself.
	explicitBelongsTo []refMeta
	// The include spec which represents the transitive closure of
	// this tables family
	allIncludeSpec *include.Spec
	// If true, this table does have an update timestamp field
	hasUpdateAtField bool
	// True if the update at field can be null
	updatedAtFieldIsNullable bool
	// True if the updated at field has a time zone
	updatedAtHasTimezone bool
	// If true, this table does have a create timestamp field
	hasCreatedAtField bool
	// True if the created at field can be null
	createdAtFieldIsNullable bool
	// True if the created at field has a time zone
	createdAtHasTimezone bool
	// The table metadata as postgres reports it
	meta tableMeta
}

// nullFlags computes the null flags specifying the nullness of this
// table in the same format used by the `null_flags` config option
func (info tableGenInfo) nullFlags() string {
	nf := make([]byte, 0, len(info.meta.Cols))
	for _, c := range info.meta.Cols {
		if c.Nullable {
			nf = append(nf, 'n')
		} else {
			nf = append(nf, '-')
		}
	}
	return string(nf)
}

func (g *Generator) populateTableInfo(tables []tableConfig) error {
	g.tables = map[string]*tableGenInfo{}
	g.tableTyNameToTableName = map[string]string{}
	for i, table := range tables {
		info := &tableGenInfo{}
		info.config = &tables[i]

		meta, err := g.tableMeta(table.Name)
		if err != nil {
			return fmt.Errorf("table '%s': %s", table.Name, err.Error())
		}
		info.meta = meta

		g.tables[table.Name] = info
		g.tableTyNameToTableName[meta.GoName] = meta.PgName
	}

	// fill in all the reference we can automatically detect
	for _, table := range g.tables {
		err := g.fillTableReferences(&table.meta)
		if err != nil {
			return err
		}
	}

	err := g.buildExplicitBelongsToMapping(tables, g.tables)
	if err != nil {
		return err
	}

	// fill in all the allIncludeSpecs
	for _, info := range g.tables {
		err := ensureSpec(g.tables, info)
		if err != nil {
			return err
		}
	}

	for _, info := range g.tables {
		g.setTimestampFlags(info)
	}

	return nil
}

func (g *Generator) setTimestampFlags(info *tableGenInfo) {
	if len(info.config.CreatedAtField) > 0 {
		for _, cm := range info.meta.Cols {
			if cm.PgName == info.config.CreatedAtField {
				info.hasCreatedAtField = true
				info.createdAtFieldIsNullable = cm.Nullable
				info.createdAtHasTimezone = cm.TypeInfo.IsTimestampWithZone
				break
			}
		}

		if !info.hasCreatedAtField {
			g.warnf(
				"table '%s' has no '%s' created at timestamp\n",
				info.config.Name,
				info.config.CreatedAtField,
			)
		}
	}

	if len(info.config.UpdatedAtField) > 0 {
		for _, cm := range info.meta.Cols {
			if cm.PgName == info.config.UpdatedAtField {
				info.hasUpdateAtField = true
				info.updatedAtFieldIsNullable = cm.Nullable
				info.updatedAtHasTimezone = cm.TypeInfo.IsTimestampWithZone
				break
			}
		}

		if !info.hasUpdateAtField {
			g.warnf(
				"table '%s' has no '%s' updated at timestamp\n",
				info.config.Name,
				info.config.UpdatedAtField,
			)
		}
	}
}

func ensureSpec(tables map[string]*tableGenInfo, info *tableGenInfo) error {
	if info.allIncludeSpec != nil {
		// Some other `ensureSpec` already filled this in for us. Great!
		return nil
	}

	info.allIncludeSpec = &include.Spec{
		TableName: info.meta.PgName,
		Includes:  map[string]*include.Spec{},
	}

	ensureReferencedSpec := func(ref *refMeta) error {
		subInfo := tables[ref.PointsFrom.PgName]
		if subInfo == nil {
			// This table is referenced in the database schema but not in the
			// config file.
			return nil
		}

		err := ensureSpec(tables, subInfo)
		if err != nil {
			return err
		}
		subSpec := subInfo.allIncludeSpec
		info.allIncludeSpec.Includes[subSpec.TableName] = subSpec

		return nil
	}

	for _, ref := range info.meta.References {
		err := ensureReferencedSpec(&ref)
		if err != nil {
			return err
		}
	}
	for _, ref := range info.explicitBelongsTo {
		err := ensureReferencedSpec(&ref)
		if err != nil {
			return err
		}
	}

	if len(info.allIncludeSpec.Includes) == 0 {
		info.allIncludeSpec.Includes = nil
	}

	return nil
}

func (g *Generator) buildExplicitBelongsToMapping(
	tables []tableConfig,
	infoTab map[string]*tableGenInfo,
) error {
	for _, table := range tables {
		pointsFromTable := g.tables[table.Name]

		for _, belongsTo := range table.BelongsTo {
			if len(belongsTo.Table) == 0 {
				return fmt.Errorf(
					"%s: belongs_to requires 'name' key",
					table.Name,
				)
			}

			if len(belongsTo.KeyField) == 0 {
				return fmt.Errorf(
					"%s: belongs_to requires 'key_field' key",
					table.Name,
				)
			}

			var belongsToColMeta *colMeta
			for i, col := range pointsFromTable.meta.Cols {
				if col.PgName == belongsTo.KeyField {
					belongsToColMeta = &pointsFromTable.meta.Cols[i]
				}
			}
			if belongsToColMeta == nil {
				return fmt.Errorf(
					"table '%s' has no field '%s'",
					table.Name,
					belongsTo.KeyField,
				)
			}

			pointsToMeta := infoTab[belongsTo.Table].meta
			ref := refMeta{
				PointsTo:         &g.tables[belongsTo.Table].meta,
				PointsToFields:   []*colMeta{pointsToMeta.PkeyCol},
				PointsFrom:       &g.tables[table.Name].meta,
				PointsFromFields: []*colMeta{belongsToColMeta},
				OneToOne:         belongsTo.OneToOne,
				Nullable:         belongsToColMeta.Nullable,
			}

			info := infoTab[belongsTo.Table]
			info.explicitBelongsTo = append(info.explicitBelongsTo, ref)
			infoTab[belongsTo.Table] = info
		}
	}

	return nil
}
