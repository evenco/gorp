// Copyright 2012 James Cooper. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

// Package gorp provides a simple way to marshal Go structs to and from
// SQL databases.  It uses the database/sql package, and should work with any
// compliant database/sql driver.
//
// Source code and project home:
// https://github.com/go-gorp/gorp
//
package gorp

import (
	"bytes"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/context"

	"github.com/evenco/go-errors"
	"github.com/evenco/go-logger"
	"github.com/evenco/go-loggerface"
)

// Oracle String (empty string is null)
type OracleString struct {
	sql.NullString
}

// Scan implements the Scanner interface.
func (os *OracleString) Scan(ctx context.Context, value interface{}) error {
	if value == nil {
		os.String, os.Valid = "", false
		return nil
	}
	os.Valid = true
	return os.NullString.Scan(value)
}

// Value implements the driver Valuer interface.
func (os OracleString) Value(ctx context.Context) (driver.Value, error) {
	if !os.Valid || os.String == "" {
		return nil, nil
	}
	return os.String, nil
}

// A nullable Time value
type NullTime struct {
	Time  time.Time
	Valid bool // Valid is true if Time is not NULL
}

// Scan implements the Scanner interface.
func (nt *NullTime) Scan(ctx context.Context, value interface{}) error {
	switch t := value.(type) {
	case time.Time:
		nt.Time, nt.Valid = t, true
	case []byte:
		nt.Valid = false
		for _, dtfmt := range []string{
			"2006-01-02 15:04:05.999999999",
			"2006-01-02T15:04:05.999999999",
			"2006-01-02 15:04:05",
			"2006-01-02T15:04:05",
			"2006-01-02 15:04",
			"2006-01-02T15:04",
			"2006-01-02",
			"2006-01-02 15:04:05-07:00",
		} {
			var err error
			if nt.Time, err = time.Parse(dtfmt, string(t)); err == nil {
				nt.Valid = true
				break
			}
		}
	}
	return nil
}

// Value implements the driver Valuer interface.
func (nt NullTime) Value(ctx context.Context) (driver.Value, error) {
	if !nt.Valid {
		return nil, nil
	}
	return nt.Time, nil
}

var zeroVal reflect.Value
var versFieldConst = "[gorp_ver_field]"

// The TypeConverter interface provides a way to map a value of one
// type to another type when persisting to, or loading from, a database.
//
// Example use cases: Implement type converter to convert bool types to "y"/"n" strings,
// or serialize a struct member as a JSON blob.
type TypeConverter interface {
	// ToDb converts val to another type. Called before INSERT/UPDATE operations
	ToDb(val interface{}) (interface{}, error)

	// FromDb returns a CustomScanner appropriate for this type. This will be used
	// to hold values returned from SELECT queries.
	//
	// In particular the CustomScanner returned should implement a Binder
	// function appropriate for the Go type you wish to convert the db value to
	//
	// If bool==false, then no custom scanner will be used for this field.
	FromDb(target interface{}) (CustomScanner, bool)
}

// CustomScanner binds a database column value to a Go type
type CustomScanner struct {
	// After a row is scanned, Holder will contain the value from the database column.
	// Initialize the CustomScanner with the concrete Go type you wish the database
	// driver to scan the raw column into.
	Holder interface{}
	// Target typically holds a pointer to the target struct field to bind the Holder
	// value to.
	Target interface{}
	// Binder is a custom function that converts the holder value to the target type
	// and sets target accordingly.  This function should return error if a problem
	// occurs converting the holder to the target.
	Binder func(holder interface{}, target interface{}) error
}

// Bind is called automatically by gorp after Scan()
func (me CustomScanner) Bind() error {
	return me.Binder(me.Holder, me.Target)
}

// DbMap is the root gorp mapping object. Create one of these for each
// database schema you wish to map.  Each DbMap contains a list of
// mapped tables.
//
// Example:
//
//     dialect := gorp.MySQLDialect{"InnoDB", "UTF8"}
//     dbmap := &gorp.DbMap{Db: db, Dialect: dialect}
//
type DbMap struct {
	// Db handle to use with this map
	Db *sql.DB

	// Dialect implementation to use with this map
	Dialect Dialect

	TypeConverter TypeConverter

	tables []*TableMap
	logger loggerface.Advanced
}

// TableMap represents a mapping between a Go struct and a database table
// Use dbmap.AddTable() or dbmap.AddTableWithName() to create these
type TableMap struct {
	// Name of database table.
	TableName      string
	SchemaName     string
	gotype         reflect.Type
	Columns        []*ColumnMap
	keys           []*ColumnMap
	uniqueTogether [][]string
	version        *ColumnMap
	insertPlan     bindPlan
	updatePlan     bindPlan
	deletePlan     bindPlan
	getPlan        bindPlan
	dbmap          *DbMap
}

// ResetSql removes cached insert/update/select/delete SQL strings
// associated with this TableMap.  Call this if you've modified
// any column names or the table name itself.
func (t *TableMap) ResetSql() {
	t.insertPlan = bindPlan{}
	t.updatePlan = bindPlan{}
	t.deletePlan = bindPlan{}
	t.getPlan = bindPlan{}
}

// SetKeys lets you specify the fields on a struct that map to primary
// key columns on the table.  If isAutoIncr is set, result.LastInsertId()
// will be used after INSERT to bind the generated id to the Go struct.
//
// Automatically calls ResetSql() to ensure SQL statements are regenerated.
//
// Panics if isAutoIncr is true, and fieldNames length != 1
//
func (t *TableMap) SetKeys(isAutoIncr bool, fieldNames ...string) *TableMap {
	if isAutoIncr && len(fieldNames) != 1 {
		panic(fmt.Sprintf(
			"gorp: SetKeys: fieldNames length must be 1 if key is auto-increment. (Saw %v fieldNames)",
			len(fieldNames)))
	}
	t.keys = make([]*ColumnMap, 0)
	for _, name := range fieldNames {
		colmap := t.ColMap(name)
		colmap.isPK = true
		colmap.isAutoIncr = isAutoIncr
		t.keys = append(t.keys, colmap)
	}
	t.ResetSql()

	return t
}

// SetUniqueTogether lets you specify uniqueness constraints across multiple
// columns on the table. Each call adds an additional constraint for the
// specified columns.
//
// Automatically calls ResetSql() to ensure SQL statements are regenerated.
//
// Panics if fieldNames length < 2.
//
func (t *TableMap) SetUniqueTogether(fieldNames ...string) *TableMap {
	if len(fieldNames) < 2 {
		panic(fmt.Sprintf(
			"gorp: SetUniqueTogether: must provide at least two fieldNames to set uniqueness constraint."))
	}

	columns := make([]string, 0)
	for _, name := range fieldNames {
		columns = append(columns, name)
	}
	t.uniqueTogether = append(t.uniqueTogether, columns)
	t.ResetSql()

	return t
}

// ColMap returns the ColumnMap pointer matching the given struct field
// name.  It panics if the struct does not contain a field matching this
// name.
func (t *TableMap) ColMap(field string) *ColumnMap {
	col := colMapOrNil(t, field)
	if col == nil {
		e := fmt.Sprintf("No ColumnMap in table %s type %s with field %s",
			t.TableName, t.gotype.Name(), field)

		panic(e)
	}
	return col
}

func colMapOrNil(t *TableMap, field string) *ColumnMap {
	for _, col := range t.Columns {
		if col.fieldName == field || col.ColumnName == field {
			return col
		}
	}
	return nil
}

// SetVersionCol sets the column to use as the Version field.  By default
// the "Version" field is used.  Returns the column found, or panics
// if the struct does not contain a field matching this name.
//
// Automatically calls ResetSql() to ensure SQL statements are regenerated.
func (t *TableMap) SetVersionCol(field string) *ColumnMap {
	c := t.ColMap(field)
	t.version = c
	t.ResetSql()
	return c
}

type bindPlan struct {
	query             string
	argFields         []string
	keyFields         []string
	versField         string
	autoIncrIdx       int
	autoIncrFieldName string
}

func (plan bindPlan) createBindInstance(elem reflect.Value, conv TypeConverter) (bindInstance, error) {
	bi := bindInstance{query: plan.query, autoIncrIdx: plan.autoIncrIdx, autoIncrFieldName: plan.autoIncrFieldName, versField: plan.versField}
	if plan.versField != "" {
		bi.existingVersion = elem.FieldByName(plan.versField).Int()
	}

	var err error

	for i := 0; i < len(plan.argFields); i++ {
		k := plan.argFields[i]
		if k == versFieldConst {
			newVer := bi.existingVersion + 1
			bi.args = append(bi.args, newVer)
			if bi.existingVersion == 0 {
				elem.FieldByName(plan.versField).SetInt(int64(newVer))
			}
		} else {
			val := elem.FieldByName(k).Interface()
			if conv != nil {
				val, err = conv.ToDb(val)
				if err != nil {
					return bindInstance{}, err
				}
			}
			bi.args = append(bi.args, val)
		}
	}

	for i := 0; i < len(plan.keyFields); i++ {
		k := plan.keyFields[i]
		val := elem.FieldByName(k).Interface()
		if conv != nil {
			val, err = conv.ToDb(val)
			if err != nil {
				return bindInstance{}, err
			}
		}
		bi.keys = append(bi.keys, val)
	}

	return bi, nil
}

type bindInstance struct {
	query             string
	args              []interface{}
	keys              []interface{}
	existingVersion   int64
	versField         string
	autoIncrIdx       int
	autoIncrFieldName string
}

func (t *TableMap) bindInsert(elem reflect.Value) (bindInstance, error) {
	plan := t.insertPlan
	if plan.query == "" {
		plan.autoIncrIdx = -1

		s := bytes.Buffer{}
		s2 := bytes.Buffer{}
		s.WriteString(fmt.Sprintf("insert into %s (", t.dbmap.Dialect.QuotedTableForQuery(t.SchemaName, t.TableName)))

		x := 0
		first := true
		for y := range t.Columns {
			col := t.Columns[y]
			if !(col.isAutoIncr && t.dbmap.Dialect.AutoIncrBindValue() == "") {
				if !col.Transient {
					if !first {
						s.WriteString(",")
						s2.WriteString(",")
					}
					s.WriteString(t.dbmap.Dialect.QuoteField(col.ColumnName))

					if col.isAutoIncr {
						s2.WriteString(t.dbmap.Dialect.AutoIncrBindValue())
						plan.autoIncrIdx = y
						plan.autoIncrFieldName = col.fieldName
					} else {
						s2.WriteString(t.dbmap.Dialect.BindVar(x))
						if col == t.version {
							plan.versField = col.fieldName
							plan.argFields = append(plan.argFields, versFieldConst)
						} else {
							plan.argFields = append(plan.argFields, col.fieldName)
						}

						x++
					}
					first = false
				}
			} else {
				plan.autoIncrIdx = y
				plan.autoIncrFieldName = col.fieldName
			}
		}
		s.WriteString(") values (")
		s.WriteString(s2.String())
		s.WriteString(")")
		if plan.autoIncrIdx > -1 {
			s.WriteString(t.dbmap.Dialect.AutoIncrInsertSuffix(t.Columns[plan.autoIncrIdx]))
		}
		s.WriteString(t.dbmap.Dialect.QuerySuffix())

		plan.query = s.String()
		t.insertPlan = plan
	}

	return plan.createBindInstance(elem, t.dbmap.TypeConverter)
}

func (t *TableMap) bindUpdate(elem reflect.Value) (bindInstance, error) {
	plan := t.updatePlan
	if plan.query == "" {

		s := bytes.Buffer{}
		s.WriteString(fmt.Sprintf("update %s set ", t.dbmap.Dialect.QuotedTableForQuery(t.SchemaName, t.TableName)))
		x := 0

		for y := range t.Columns {
			col := t.Columns[y]
			if !col.isAutoIncr && !col.Transient {
				if x > 0 {
					s.WriteString(", ")
				}
				s.WriteString(t.dbmap.Dialect.QuoteField(col.ColumnName))
				s.WriteString("=")
				s.WriteString(t.dbmap.Dialect.BindVar(x))

				if col == t.version {
					plan.versField = col.fieldName
					plan.argFields = append(plan.argFields, versFieldConst)
				} else {
					plan.argFields = append(plan.argFields, col.fieldName)
				}
				x++
			}
		}

		s.WriteString(" where ")
		for y := range t.keys {
			col := t.keys[y]
			if y > 0 {
				s.WriteString(" and ")
			}
			s.WriteString(t.dbmap.Dialect.QuoteField(col.ColumnName))
			s.WriteString("=")
			s.WriteString(t.dbmap.Dialect.BindVar(x))

			plan.argFields = append(plan.argFields, col.fieldName)
			plan.keyFields = append(plan.keyFields, col.fieldName)
			x++
		}
		if plan.versField != "" {
			s.WriteString(" and ")
			s.WriteString(t.dbmap.Dialect.QuoteField(t.version.ColumnName))
			s.WriteString("=")
			s.WriteString(t.dbmap.Dialect.BindVar(x))
			plan.argFields = append(plan.argFields, plan.versField)
		}
		s.WriteString(t.dbmap.Dialect.QuerySuffix())

		plan.query = s.String()
		t.updatePlan = plan
	}

	return plan.createBindInstance(elem, t.dbmap.TypeConverter)
}

func (t *TableMap) bindDelete(elem reflect.Value) (bindInstance, error) {
	plan := t.deletePlan
	if plan.query == "" {

		s := bytes.Buffer{}
		s.WriteString(fmt.Sprintf("delete from %s", t.dbmap.Dialect.QuotedTableForQuery(t.SchemaName, t.TableName)))

		for y := range t.Columns {
			col := t.Columns[y]
			if !col.Transient {
				if col == t.version {
					plan.versField = col.fieldName
				}
			}
		}

		s.WriteString(" where ")
		for x := range t.keys {
			k := t.keys[x]
			if x > 0 {
				s.WriteString(" and ")
			}
			s.WriteString(t.dbmap.Dialect.QuoteField(k.ColumnName))
			s.WriteString("=")
			s.WriteString(t.dbmap.Dialect.BindVar(x))

			plan.keyFields = append(plan.keyFields, k.fieldName)
			plan.argFields = append(plan.argFields, k.fieldName)
		}
		if plan.versField != "" {
			s.WriteString(" and ")
			s.WriteString(t.dbmap.Dialect.QuoteField(t.version.ColumnName))
			s.WriteString("=")
			s.WriteString(t.dbmap.Dialect.BindVar(len(plan.argFields)))

			plan.argFields = append(plan.argFields, plan.versField)
		}
		s.WriteString(t.dbmap.Dialect.QuerySuffix())

		plan.query = s.String()
		t.deletePlan = plan
	}

	return plan.createBindInstance(elem, t.dbmap.TypeConverter)
}

func (t *TableMap) bindGet() bindPlan {
	plan := t.getPlan
	if plan.query == "" {

		s := bytes.Buffer{}
		s.WriteString("select ")

		x := 0
		for _, col := range t.Columns {
			if !col.Transient {
				if x > 0 {
					s.WriteString(",")
				}
				s.WriteString(t.dbmap.Dialect.QuoteField(col.ColumnName))
				plan.argFields = append(plan.argFields, col.fieldName)
				x++
			}
		}
		s.WriteString(" from ")
		s.WriteString(t.dbmap.Dialect.QuotedTableForQuery(t.SchemaName, t.TableName))
		s.WriteString(" where ")
		for x := range t.keys {
			col := t.keys[x]
			if x > 0 {
				s.WriteString(" and ")
			}
			s.WriteString(t.dbmap.Dialect.QuoteField(col.ColumnName))
			s.WriteString("=")
			s.WriteString(t.dbmap.Dialect.BindVar(x))

			plan.keyFields = append(plan.keyFields, col.fieldName)
		}
		s.WriteString(t.dbmap.Dialect.QuerySuffix())

		plan.query = s.String()
		t.getPlan = plan
	}

	return plan
}

// ColumnMap represents a mapping between a Go struct field and a single
// column in a table.
// Unique and MaxSize only inform the
// CreateTables() function and are not used by Insert/Update/Delete/Get.
type ColumnMap struct {
	// Column name in db table
	ColumnName string

	// If true, this column is skipped in generated SQL statements
	Transient bool

	// If true, " unique" is added to create table statements.
	// Not used elsewhere
	Unique bool

	// Passed to Dialect.ToSqlType() to assist in informing the
	// correct column type to map to in CreateTables()
	// Not used elsewhere
	MaxSize int

	fieldName  string
	gotype     reflect.Type
	isPK       bool
	isAutoIncr bool
	isNotNull  bool
}

// Rename allows you to specify the column name in the table
//
// Example:  table.ColMap("Updated").Rename("date_updated")
//
func (c *ColumnMap) Rename(colname string) *ColumnMap {
	c.ColumnName = colname
	return c
}

// SetTransient allows you to mark the column as transient. If true
// this column will be skipped when SQL statements are generated
func (c *ColumnMap) SetTransient(b bool) *ColumnMap {
	c.Transient = b
	return c
}

// SetUnique adds "unique" to the create table statements for this
// column, if b is true.
func (c *ColumnMap) SetUnique(b bool) *ColumnMap {
	c.Unique = b
	return c
}

// SetNotNull adds "not null" to the create table statements for this
// column, if nn is true.
func (c *ColumnMap) SetNotNull(nn bool) *ColumnMap {
	c.isNotNull = nn
	return c
}

// SetMaxSize specifies the max length of values of this column. This is
// passed to the dialect.ToSqlType() function, which can use the value
// to alter the generated type for "create table" statements
func (c *ColumnMap) SetMaxSize(size int) *ColumnMap {
	c.MaxSize = size
	return c
}

// Transaction represents a database transaction.
// Insert/Update/Delete/Get/Exec operations will be run in the context
// of that transaction.  Transactions should be terminated with
// a call to Commit() or Rollback()
type Transaction struct {
	dbmap               *DbMap
	tx                  *sql.Tx
	closed              bool
	postCommitCallbacks []func()
}

// Common interface for executing SQL queries. Intended allow delegation
// to both Gorp SqlExecutors as well as vanilla database/sql.DB and .Tx receivers.
type executor interface {
	Exec(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

// Interface that contains the context-less Exec variant used in database/sql.
type legacyExecutor interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

// Simple adapter that implements `executor` and forwards calls to a given
// `legacyExecutor`.
type executorAdapter struct {
	Delegate legacyExecutor
}

func (e executorAdapter) Exec(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return e.Delegate.Exec(query, args...)
}

// SqlExecutor exposes gorp operations that can be run from Pre/Post
// hooks.  This hides whether the current operation that triggered the
// hook is in a transaction.
//
// See the DbMap function docs for each of the functions below for more
// information.
type SqlExecutor interface {
	Get(ctx context.Context, i interface{}, keys ...interface{}) (interface{}, error)
	Insert(ctx context.Context, list ...interface{}) error
	Update(ctx context.Context, list ...interface{}) (int64, error)
	Delete(ctx context.Context, list ...interface{}) (int64, error)
	Exec(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
	Select(ctx context.Context, i interface{}, query string,
		args ...interface{}) ([]interface{}, error)
	SelectInt(ctx context.Context, query string, args ...interface{}) (int64, error)
	SelectNullInt(ctx context.Context, query string, args ...interface{}) (sql.NullInt64, error)
	SelectFloat(ctx context.Context, query string, args ...interface{}) (float64, error)
	SelectNullFloat(ctx context.Context, query string, args ...interface{}) (sql.NullFloat64, error)
	SelectStr(ctx context.Context, query string, args ...interface{}) (string, error)
	SelectNullStr(ctx context.Context, query string, args ...interface{}) (sql.NullString, error)
	SelectOne(ctx context.Context, holder interface{}, query string, args ...interface{}) error
	OnCommit(ctx context.Context, f func())
	query(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	queryRow(ctx context.Context, query string, args ...interface{}) *sql.Row
}

// Compile-time check that DbMap and Transaction implement the SqlExecutor
// interface.
var _, _ SqlExecutor = &DbMap{}, &Transaction{}

// TraceOn turns on SQL statement logging for this DbMap.  After this is
// called, all SQL statements will be sent to the logger.  If prefix is
// a non-empty string, it will be written to the front of all logged
// strings, which can aid in filtering log lines.
//
// Use TraceOn if you want to spy on the SQL statements that gorp
// generates.
func (m *DbMap) TraceOn(logger loggerface.Advanced) {
	m.logger = logger
}

// TraceOff turns off tracing. It is idempotent.
func (m *DbMap) TraceOff() {
	m.logger = nil
}

// AddTable registers the given interface type with gorp. The table name
// will be given the name of the TypeOf(i).  You must call this function,
// or AddTableWithName, for any struct type you wish to persist with
// the given DbMap.
//
// This operation is idempotent. If i's type is already mapped, the
// existing *TableMap is returned
func (m *DbMap) AddTable(i interface{}) *TableMap {
	return m.AddTableWithName(i, "")
}

// AddTableWithName has the same behavior as AddTable, but sets
// table.TableName to name.
func (m *DbMap) AddTableWithName(i interface{}, name string) *TableMap {
	return m.AddTableWithNameAndSchema(i, "", name)
}

// AddTableWithNameAndSchema has the same behavior as AddTable, but sets
// table.TableName to name.
func (m *DbMap) AddTableWithNameAndSchema(i interface{}, schema string, name string) *TableMap {
	t := reflect.TypeOf(i)
	if name == "" {
		name = t.Name()
	}

	// check if we have a table for this type already
	// if so, update the name and return the existing pointer
	for i := range m.tables {
		table := m.tables[i]
		if table.gotype == t {
			table.TableName = name
			return table
		}
	}

	tmap := &TableMap{gotype: t, TableName: name, SchemaName: schema, dbmap: m}
	tmap.Columns = m.readStructColumns(t)
	m.tables = append(m.tables, tmap)

	return tmap
}

func (m *DbMap) readStructColumns(t reflect.Type) (cols []*ColumnMap) {
	n := t.NumField()
	for i := 0; i < n; i++ {
		f := t.Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			// Recursively add nested fields in embedded structs.
			subcols := m.readStructColumns(f.Type)
			// Don't append nested fields that have the same field
			// name as an already-mapped field.
			for _, subcol := range subcols {
				shouldAppend := true
				for _, col := range cols {
					if !subcol.Transient && subcol.fieldName == col.fieldName {
						shouldAppend = false
						break
					}
				}
				if shouldAppend {
					cols = append(cols, subcol)
				}
			}
		} else {
			columnName := f.Tag.Get("db")
			if columnName == "" {
				columnName = f.Name
			}
			gotype := f.Type
			if m.TypeConverter != nil {
				// Make a new pointer to a value of type gotype and
				// pass it to the TypeConverter's FromDb method to see
				// if a different type should be used for the column
				// type during table creation.
				value := reflect.New(gotype).Interface()
				scanner, useHolder := m.TypeConverter.FromDb(value)
				if useHolder {
					gotype = reflect.TypeOf(scanner.Holder)
				}
			}
			cm := &ColumnMap{
				ColumnName: columnName,
				Transient:  columnName == "-",
				fieldName:  f.Name,
				gotype:     gotype,
			}
			// Check for nested fields of the same field name and
			// override them.
			shouldAppend := true
			for index, col := range cols {
				if !col.Transient && col.fieldName == cm.fieldName {
					cols[index] = cm
					shouldAppend = false
					break
				}
			}
			if shouldAppend {
				cols = append(cols, cm)
			}
		}
	}
	return
}

// CreateTables iterates through TableMaps registered to this DbMap and
// executes "create table" statements against the database for each.
//
// This is particularly useful in unit tests where you want to create
// and destroy the schema automatically.
func (m *DbMap) CreateTables(ctx context.Context) error {
	return m.createTables(ctx, false)
}

// CreateTablesIfNotExists is similar to CreateTables, but starts
// each statement with "create table if not exists" so that existing
// tables do not raise errors
func (m *DbMap) CreateTablesIfNotExists(ctx context.Context) error {
	return m.createTables(ctx, true)
}

func (m *DbMap) createTables(ctx context.Context, ifNotExists bool) error {
	var err error
	for i := range m.tables {
		table := m.tables[i]

		s := bytes.Buffer{}

		if strings.TrimSpace(table.SchemaName) != "" {
			schemaCreate := "create schema"
			if ifNotExists {
				s.WriteString(m.Dialect.IfSchemaNotExists(schemaCreate, table.SchemaName))
			} else {
				s.WriteString(schemaCreate)
			}
			s.WriteString(fmt.Sprintf(" %s;", table.SchemaName))
		}

		tableCreate := "create table"
		if ifNotExists {
			s.WriteString(m.Dialect.IfTableNotExists(tableCreate, table.SchemaName, table.TableName))
		} else {
			s.WriteString(tableCreate)
		}
		s.WriteString(fmt.Sprintf(" %s (", m.Dialect.QuotedTableForQuery(table.SchemaName, table.TableName)))

		x := 0
		for _, col := range table.Columns {
			if !col.Transient {
				if x > 0 {
					s.WriteString(", ")
				}
				stype := m.Dialect.ToSqlType(col.gotype, col.MaxSize, col.isAutoIncr)
				s.WriteString(fmt.Sprintf("%s %s", m.Dialect.QuoteField(col.ColumnName), stype))

				if col.isPK || col.isNotNull {
					s.WriteString(" not null")
				}
				if col.isPK && len(table.keys) == 1 {
					s.WriteString(" primary key")
				}
				if col.Unique {
					s.WriteString(" unique")
				}
				if col.isAutoIncr {
					s.WriteString(fmt.Sprintf(" %s", m.Dialect.AutoIncrStr()))
				}

				x++
			}
		}
		if len(table.keys) > 1 {
			s.WriteString(", primary key (")
			for x := range table.keys {
				if x > 0 {
					s.WriteString(", ")
				}
				s.WriteString(m.Dialect.QuoteField(table.keys[x].ColumnName))
			}
			s.WriteString(")")
		}
		if len(table.uniqueTogether) > 0 {
			for _, columns := range table.uniqueTogether {
				s.WriteString(", unique (")
				for i, column := range columns {
					if i > 0 {
						s.WriteString(", ")
					}
					s.WriteString(m.Dialect.QuoteField(column))
				}
				s.WriteString(")")
			}
		}
		s.WriteString(") ")
		s.WriteString(m.Dialect.CreateTableSuffix())
		s.WriteString(m.Dialect.QuerySuffix())
		_, err = m.Exec(ctx, s.String())
		if err != nil {
			break
		}
	}
	return err
}

// DropTable drops an individual table.  Will throw an error
// if the table does not exist.
func (m *DbMap) DropTable(ctx context.Context, table interface{}) error {
	t := reflect.TypeOf(table)
	return m.dropTable(ctx, t, false)
}

// DropTable drops an individual table.  Will NOT throw an error
// if the table does not exist.
func (m *DbMap) DropTableIfExists(ctx context.Context, table interface{}) error {
	t := reflect.TypeOf(table)
	return m.dropTable(ctx, t, true)
}

// DropTables iterates through TableMaps registered to this DbMap and
// executes "drop table" statements against the database for each.
func (m *DbMap) DropTables(ctx context.Context) error {
	return m.dropTables(ctx, false)
}

// DropTablesIfExists is the same as DropTables, but uses the "if exists" clause to
// avoid errors for tables that do not exist.
func (m *DbMap) DropTablesIfExists(ctx context.Context) error {
	return m.dropTables(ctx, true)
}

// Goes through all the registered tables, dropping them one by one.
// If an error is encountered, then it is returned and the rest of
// the tables are not dropped.
func (m *DbMap) dropTables(ctx context.Context, addIfExists bool) (err error) {
	for _, table := range m.tables {
		err = m.dropTableImpl(ctx, table, addIfExists)
		if err != nil {
			return
		}
	}
	return err
}

// Implementation of dropping a single table.
func (m *DbMap) dropTable(ctx context.Context, t reflect.Type, addIfExists bool) error {
	table := tableOrNil(m, t)
	if table == nil {
		return errors.New(fmt.Sprintf("table %s was not registered!", table.TableName))
	}

	return m.dropTableImpl(ctx, table, addIfExists)
}

func (m *DbMap) dropTableImpl(ctx context.Context, table *TableMap, ifExists bool) (err error) {
	tableDrop := "drop table"
	if ifExists {
		tableDrop = m.Dialect.IfTableExists(tableDrop, table.SchemaName, table.TableName)
	}
	_, err = m.Exec(ctx, fmt.Sprintf("%s %s;", tableDrop, m.Dialect.QuotedTableForQuery(table.SchemaName, table.TableName)))
	return err
}

// TruncateTables iterates through TableMaps registered to this DbMap and
// executes "truncate table" statements against the database for each, or in the case of
// sqlite, a "delete from" with no "where" clause, which uses the truncate optimization
// (http://www.sqlite.org/lang_delete.html)
func (m *DbMap) TruncateTables(ctx context.Context) error {
	var err error
	for i := range m.tables {
		table := m.tables[i]
		_, e := m.Exec(ctx, fmt.Sprintf("%s %s;", m.Dialect.TruncateClause(), m.Dialect.QuotedTableForQuery(table.SchemaName, table.TableName)))
		if e != nil {
			err = e
		}
	}
	return err
}

// Insert runs a SQL INSERT statement for each element in list.  List
// items must be pointers.
//
// Any interface whose TableMap has an auto-increment primary key will
// have its last insert id bound to the PK field on the struct.
//
// The hook functions PreInsert() and/or PostInsert() will be executed
// before/after the INSERT statement if the interface defines them.
//
// Panics if any interface in the list has not been registered with AddTable
func (m *DbMap) Insert(ctx context.Context, list ...interface{}) error {
	err := insert(ctx, m, m, list...)
	if err != nil {
		return err
	}

	err = postCommitInsert(ctx, m, list...)
	if err != nil {
		return err
	}

	return nil
}

// Update runs a SQL UPDATE statement for each element in list.  List
// items must be pointers.
//
// The hook functions PreUpdate() and/or PostUpdate() will be executed
// before/after the UPDATE statement if the interface defines them.
//
// Returns the number of rows updated.
//
// Returns an error if SetKeys has not been called on the TableMap
// Panics if any interface in the list has not been registered with AddTable
func (m *DbMap) Update(ctx context.Context, list ...interface{}) (int64, error) {
	i, err := update(ctx, m, m, list...)
	if err != nil {
		return -1, err
	}

	err = postCommitUpdate(ctx, m, list...)
	if err != nil {
		return -1, err
	}

	return i, nil
}

// Delete runs a SQL DELETE statement for each element in list.  List
// items must be pointers.
//
// The hook functions PreDelete() and/or PostDelete() will be executed
// before/after the DELETE statement if the interface defines them.
//
// Returns the number of rows deleted.
//
// Returns an error if SetKeys has not been called on the TableMap
// Panics if any interface in the list has not been registered with AddTable
func (m *DbMap) Delete(ctx context.Context, list ...interface{}) (int64, error) {
	i, err := delete(ctx, m, m, list...)
	if err != nil {
		return -1, err
	}

	err = postCommitDelete(ctx, m, list...)
	if err != nil {
		return -1, err
	}

	return i, nil
}

// Get runs a SQL SELECT to fetch a single row from the table based on the
// primary key(s)
//
// i should be an empty value for the struct to load.  keys should be
// the primary key value(s) for the row to load.  If multiple keys
// exist on the table, the order should match the column order
// specified in SetKeys() when the table mapping was defined.
//
// The hook function PostGet() will be executed after the SELECT
// statement if the interface defines them.
//
// Returns a pointer to a struct that matches or nil if no row is found.
//
// Returns an error if SetKeys has not been called on the TableMap
// Panics if any interface in the list has not been registered with AddTable
func (m *DbMap) Get(ctx context.Context, i interface{}, keys ...interface{}) (interface{}, error) {
	return get(ctx, m, m, i, keys...)
}

// Select runs an arbitrary SQL query, binding the columns in the result
// to fields on the struct specified by i.  args represent the bind
// parameters for the SQL statement.
//
// Column names on the SELECT statement should be aliased to the field names
// on the struct i. Returns an error if one or more columns in the result
// do not match.  It is OK if fields on i are not part of the SQL
// statement.
//
// The hook function PostGet(ctx context.Context, ) will be executed after the SELECT
// statement if the interface defines them.
//
// Values are returned in one of two ways:
// 1. If i is a struct or a pointer to a struct, returns a slice of pointers to
// matching rows of type i.
// 2. If i is a pointer to a slice, the results will be appended to that slice
// and nil returned.
//
// i does NOT need to be registered with AddTable()
func (m *DbMap) Select(ctx context.Context, i interface{}, query string, args ...interface{}) ([]interface{}, error) {
	return hookedselect(ctx, m, m, i, query, args...)
}

// Exec runs an arbitrary SQL statement.  args represent the bind parameters.
// This is equivalent to running:  Exec() using database/sql
func (m *DbMap) Exec(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	if m.logger != nil {
		now := time.Now()
		defer m.trace(ctx, now, query, args...)
	}
	return exec(ctx, m, query, args...)
}

// SelectInt is a convenience wrapper around the gorp.SelectInt function
func (m *DbMap) SelectInt(ctx context.Context, query string, args ...interface{}) (int64, error) {
	return SelectInt(ctx, m, query, args...)
}

// SelectNullInt is a convenience wrapper around the gorp.SelectNullInt function
func (m *DbMap) SelectNullInt(ctx context.Context, query string, args ...interface{}) (sql.NullInt64, error) {
	return SelectNullInt(ctx, m, query, args...)
}

// SelectFloat is a convenience wrapper around the gorp.SelectFlot function
func (m *DbMap) SelectFloat(ctx context.Context, query string, args ...interface{}) (float64, error) {
	return SelectFloat(ctx, m, query, args...)
}

// SelectNullFloat is a convenience wrapper around the gorp.SelectNullFloat function
func (m *DbMap) SelectNullFloat(ctx context.Context, query string, args ...interface{}) (sql.NullFloat64, error) {
	return SelectNullFloat(ctx, m, query, args...)
}

// SelectStr is a convenience wrapper around the gorp.SelectStr function
func (m *DbMap) SelectStr(ctx context.Context, query string, args ...interface{}) (string, error) {
	return SelectStr(ctx, m, query, args...)
}

// SelectNullStr is a convenience wrapper around the gorp.SelectNullStr function
func (m *DbMap) SelectNullStr(ctx context.Context, query string, args ...interface{}) (sql.NullString, error) {
	return SelectNullStr(ctx, m, query, args...)
}

// SelectOne is a convenience wrapper around the gorp.SelectOne function
func (m *DbMap) SelectOne(ctx context.Context, holder interface{}, query string, args ...interface{}) error {
	return SelectOne(ctx, m, m, holder, query, args...)
}

// OnCommit executes immediately when not within a transaction
func (m *DbMap) OnCommit(ctx context.Context, f func()) {
	f()
}

// Begin starts a gorp Transaction
func (m *DbMap) Begin(ctx context.Context) (*Transaction, error) {
	if m.logger != nil {
		now := time.Now()
		defer m.trace(ctx, now, "begin;")
	}
	tx, err := m.Db.Begin()
	if err != nil {
		return nil, err
	}
	return &Transaction{m, tx, false, []func(){}}, nil
}

// TableFor returns the *TableMap corresponding to the given Go Type
// If no table is mapped to that type an error is returned.
// If checkPK is true and the mapped table has no registered PKs, an error is returned.
func (m *DbMap) TableFor(t reflect.Type, checkPK bool) (*TableMap, error) {
	table := tableOrNil(m, t)
	if table == nil {
		return nil, errors.New(fmt.Sprintf("No table found for type: %v", t.Name()))
	}

	if checkPK && len(table.keys) < 1 {
		e := fmt.Sprintf("gorp: No keys defined for table: %s",
			table.TableName)
		return nil, errors.New(e)
	}

	return table, nil
}

// Prepare creates a prepared statement for later queries or executions.
// Multiple queries or executions may be run concurrently from the returned statement.
// This is equivalent to running:  Prepare() using database/sql
func (m *DbMap) Prepare(ctx context.Context, query string) (*sql.Stmt, error) {
	if m.logger != nil {
		now := time.Now()
		defer m.trace(ctx, now, query, nil)
	}
	return m.Db.Prepare(query)
}

func tableOrNil(m *DbMap, t reflect.Type) *TableMap {
	for i := range m.tables {
		table := m.tables[i]
		if table.gotype == t {
			return table
		}
	}
	return nil
}

func (m *DbMap) tableForPointer(ptr interface{}, checkPK bool) (*TableMap, reflect.Value, error) {
	ptrv := reflect.ValueOf(ptr)
	if ptrv.Kind() != reflect.Ptr {
		e := fmt.Sprintf("gorp: passed non-pointer: %v (kind=%v)", ptr,
			ptrv.Kind())
		return nil, reflect.Value{}, errors.New(e)
	}
	elem := ptrv.Elem()
	etype := reflect.TypeOf(elem.Interface())
	t, err := m.TableFor(etype, checkPK)
	if err != nil {
		return nil, reflect.Value{}, err
	}

	return t, elem, nil
}

func (m *DbMap) queryRow(ctx context.Context, query string, args ...interface{}) *sql.Row {
	if m.logger != nil {
		now := time.Now()
		defer m.trace(ctx, now, query, args...)
	}
	return m.Db.QueryRow(query, args...)
}

func (m *DbMap) query(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	if m.logger != nil {
		now := time.Now()
		defer m.trace(ctx, now, query, args...)
	}
	return m.Db.Query(query, args...)
}

func (m *DbMap) trace(ctx context.Context, started time.Time, query string, args ...interface{}) {
	if m.logger != nil {
		durationString := fmt.Sprintf("%.3f", float64(time.Since(started)/time.Microsecond)/1000.0)
		durationFloat, _ := strconv.ParseFloat(durationString, 64)
		m.logger.Info(ctx, "Query complete", map[string]interface{}{
			"query":      query,
			"queryArgs":  formattedArgs(args...),
			"durationMS": durationFloat,
		})
	}
}

func formattedArgs(args ...interface{}) map[string]string {
	var strArgs = map[string]string{}
	for i, a := range args {
		var v interface{} = a
		if x, ok := v.(driver.Valuer); ok {
			y, err := x.Value()
			if err == nil {
				v = y
			}
		}
		strArgs[fmt.Sprintf("%02d", i+1)] = fmt.Sprintf("%v", v)
	}
	return strArgs
}

///////////////

// Insert has the same behavior as DbMap.Insert(), but runs in a transaction.
func (t *Transaction) Insert(ctx context.Context, list ...interface{}) error {
	err := insert(ctx, t.dbmap, t, list...)
	if err != nil {
		return err
	}

	t.postCommitCallbacks = append(t.postCommitCallbacks, func() {
		postCommitInsert(ctx, t.dbmap, list...)
	})

	return nil
}

// Update had the same behavior as DbMap.Update(), but runs in a transaction.
func (t *Transaction) Update(ctx context.Context, list ...interface{}) (int64, error) {
	i, err := update(ctx, t.dbmap, t, list...)
	if err != nil {
		return -1, err
	}

	t.postCommitCallbacks = append(t.postCommitCallbacks, func() {
		postCommitUpdate(ctx, t.dbmap, list...)
	})

	return i, nil
}

// Delete has the same behavior as DbMap.Delete(), but runs in a transaction.
func (t *Transaction) Delete(ctx context.Context, list ...interface{}) (int64, error) {
	i, err := delete(ctx, t.dbmap, t, list...)
	if err != nil {
		return -1, err
	}

	t.postCommitCallbacks = append(t.postCommitCallbacks, func() {
		postCommitDelete(ctx, t.dbmap, list...)
	})

	return i, nil
}

// Get has the same behavior as DbMap.Get(), but runs in a transaction.
func (t *Transaction) Get(ctx context.Context, i interface{}, keys ...interface{}) (interface{}, error) {
	return get(ctx, t.dbmap, t, i, keys...)
}

// Select has the same behavior as DbMap.Select(), but runs in a transaction.
func (t *Transaction) Select(ctx context.Context, i interface{}, query string, args ...interface{}) ([]interface{}, error) {
	return hookedselect(ctx, t.dbmap, t, i, query, args...)
}

// Exec has the same behavior as DbMap.Exec(), but runs in a transaction.
func (t *Transaction) Exec(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	if t.dbmap.logger != nil {
		now := time.Now()
		defer t.dbmap.trace(ctx, now, query, args...)
	}
	return exec(ctx, t, query, args...)
}

// SelectInt is a convenience wrapper around the gorp.SelectInt function.
func (t *Transaction) SelectInt(ctx context.Context, query string, args ...interface{}) (int64, error) {
	return SelectInt(ctx, t, query, args...)
}

// SelectNullInt is a convenience wrapper around the gorp.SelectNullInt function.
func (t *Transaction) SelectNullInt(ctx context.Context, query string, args ...interface{}) (sql.NullInt64, error) {
	return SelectNullInt(ctx, t, query, args...)
}

// SelectFloat is a convenience wrapper around the gorp.SelectFloat function.
func (t *Transaction) SelectFloat(ctx context.Context, query string, args ...interface{}) (float64, error) {
	return SelectFloat(ctx, t, query, args...)
}

// SelectNullFloat is a convenience wrapper around the gorp.SelectNullFloat function.
func (t *Transaction) SelectNullFloat(ctx context.Context, query string, args ...interface{}) (sql.NullFloat64, error) {
	return SelectNullFloat(ctx, t, query, args...)
}

// SelectStr is a convenience wrapper around the gorp.SelectStr function.
func (t *Transaction) SelectStr(ctx context.Context, query string, args ...interface{}) (string, error) {
	return SelectStr(ctx, t, query, args...)
}

// SelectNullStr is a convenience wrapper around the gorp.SelectNullStr function.
func (t *Transaction) SelectNullStr(ctx context.Context, query string, args ...interface{}) (sql.NullString, error) {
	return SelectNullStr(ctx, t, query, args...)
}

// SelectOne is a convenience wrapper around the gorp.SelectOne function.
func (t *Transaction) SelectOne(ctx context.Context, holder interface{}, query string, args ...interface{}) error {
	return SelectOne(ctx, t.dbmap, t, holder, query, args...)
}

// OnCommit executes a function after the transaction has completed.
func (t *Transaction) OnCommit(ctx context.Context, f func()) {
	t.postCommitCallbacks = append(t.postCommitCallbacks, f)
}

// Commit commits the underlying database transaction.
func (t *Transaction) Commit(ctx context.Context) error {
	if !t.closed {
		t.closed = true
		if t.dbmap.logger != nil {
			now := time.Now()
			defer t.dbmap.trace(ctx, now, "commit;")
		}
		err := t.tx.Commit()
		if err != nil {
			return err
		}

		for _, cb := range t.postCommitCallbacks {
			defer cb()
		}
		return nil
	}

	return sql.ErrTxDone
}

// Rollback rolls back the underlying database transaction.
func (t *Transaction) Rollback(ctx context.Context) error {
	if !t.closed {
		t.closed = true
		if t.dbmap.logger != nil {
			now := time.Now()
			defer t.dbmap.trace(ctx, now, "rollback;")
		}
		err := t.tx.Rollback()
		if err != nil && t.dbmap.logger != nil {
			t.dbmap.logger.Error(ctx, "Failed to rollback transaction", err, nil)
		}
		return err
	}

	if t.dbmap.logger != nil {
		t.dbmap.logger.Error(ctx, "Attempted to rollback closed transaction", nil, nil)
	}

	return sql.ErrTxDone
}

// Savepoint creates a savepoint with the given name. The name is interpolated
// directly into the SQL SAVEPOINT statement, so you must sanitize it if it is
// derived from user input.
func (t *Transaction) Savepoint(ctx context.Context, name string) error {
	query := "savepoint " + t.dbmap.Dialect.QuoteField(name)
	if t.dbmap.logger != nil {
		now := time.Now()
		defer t.dbmap.trace(ctx, now, query, nil)
	}
	_, err := t.tx.Exec(query)
	return err
}

// RollbackToSavepoint rolls back to the savepoint with the given name. The
// name is interpolated directly into the SQL SAVEPOINT statement, so you must
// sanitize it if it is derived from user input.
func (t *Transaction) RollbackToSavepoint(ctx context.Context, savepoint string) error {
	query := "rollback to savepoint " + t.dbmap.Dialect.QuoteField(savepoint)
	if t.dbmap.logger != nil {
		now := time.Now()
		defer t.dbmap.trace(ctx, now, query, nil)
	}
	_, err := t.tx.Exec(query)
	return err
}

// ReleaseSavepint releases the savepoint with the given name. The name is
// interpolated directly into the SQL SAVEPOINT statement, so you must sanitize
// it if it is derived from user input.
func (t *Transaction) ReleaseSavepoint(ctx context.Context, savepoint string) error {
	query := "release savepoint " + t.dbmap.Dialect.QuoteField(savepoint)
	if t.dbmap.logger != nil {
		now := time.Now()
		defer t.dbmap.trace(ctx, now, query, nil)
	}
	_, err := t.tx.Exec(query)
	return err
}

// Prepare has the same behavior as DbMap.Prepare(), but runs in a transaction.
func (t *Transaction) Prepare(ctx context.Context, query string) (*sql.Stmt, error) {
	if t.dbmap.logger != nil {
		now := time.Now()
		defer t.dbmap.trace(ctx, now, query, nil)
	}
	return t.tx.Prepare(query)
}

func (t *Transaction) queryRow(ctx context.Context, query string, args ...interface{}) *sql.Row {
	if t.dbmap.logger != nil {
		now := time.Now()
		defer t.dbmap.trace(ctx, now, query, args...)
	}
	return t.tx.QueryRow(query, args...)
}

func (t *Transaction) query(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	if t.dbmap.logger != nil {
		now := time.Now()
		defer t.dbmap.trace(ctx, now, query, args...)
	}
	return t.tx.Query(query, args...)
}

///////////////

// SelectInt executes the given query, which should be a SELECT statement for a single
// integer column, and returns the value of the first row returned.  If no rows are
// found, zero is returned.
func SelectInt(ctx context.Context, e SqlExecutor, query string, args ...interface{}) (int64, error) {
	var h int64
	err := selectVal(ctx, e, &h, query, args...)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	return h, nil
}

// SelectNullInt executes the given query, which should be a SELECT statement for a single
// integer column, and returns the value of the first row returned.  If no rows are
// found, the empty sql.NullInt64 value is returned.
func SelectNullInt(ctx context.Context, e SqlExecutor, query string, args ...interface{}) (sql.NullInt64, error) {
	var h sql.NullInt64
	err := selectVal(ctx, e, &h, query, args...)
	if err != nil && err != sql.ErrNoRows {
		return h, err
	}
	return h, nil
}

// SelectFloat executes the given query, which should be a SELECT statement for a single
// float column, and returns the value of the first row returned. If no rows are
// found, zero is returned.
func SelectFloat(ctx context.Context, e SqlExecutor, query string, args ...interface{}) (float64, error) {
	var h float64
	err := selectVal(ctx, e, &h, query, args...)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	return h, nil
}

// SelectNullFloat executes the given query, which should be a SELECT statement for a single
// float column, and returns the value of the first row returned. If no rows are
// found, the empty sql.NullInt64 value is returned.
func SelectNullFloat(ctx context.Context, e SqlExecutor, query string, args ...interface{}) (sql.NullFloat64, error) {
	var h sql.NullFloat64
	err := selectVal(ctx, e, &h, query, args...)
	if err != nil && err != sql.ErrNoRows {
		return h, err
	}
	return h, nil
}

// SelectStr executes the given query, which should be a SELECT statement for a single
// char/varchar column, and returns the value of the first row returned.  If no rows are
// found, an empty string is returned.
func SelectStr(ctx context.Context, e SqlExecutor, query string, args ...interface{}) (string, error) {
	var h string
	err := selectVal(ctx, e, &h, query, args...)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	return h, nil
}

// SelectNullStr executes the given query, which should be a SELECT
// statement for a single char/varchar column, and returns the value
// of the first row returned.  If no rows are found, the empty
// sql.NullString is returned.
func SelectNullStr(ctx context.Context, e SqlExecutor, query string, args ...interface{}) (sql.NullString, error) {
	var h sql.NullString
	err := selectVal(ctx, e, &h, query, args...)
	if err != nil && err != sql.ErrNoRows {
		return h, err
	}
	return h, nil
}

// SelectOne executes the given query (which should be a SELECT statement)
// and binds the result to holder, which must be a pointer.
//
// If no row is found, an error (sql.ErrNoRows specifically) will be returned
//
// If more than one row is found, an error will be returned.
//
func SelectOne(ctx context.Context, m *DbMap, e SqlExecutor, holder interface{}, query string, args ...interface{}) error {
	t := reflect.TypeOf(holder)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	} else {
		return fmt.Errorf("gorp: SelectOne holder must be a pointer, but got: %t", holder)
	}

	// Handle pointer to pointer
	isptr := false
	if t.Kind() == reflect.Ptr {
		isptr = true
		t = t.Elem()
	}

	if t.Kind() == reflect.Struct {

		list, err := hookedselect(ctx, m, e, holder, query, args...)
		if err != nil {
			return err
		}

		dest := reflect.ValueOf(holder)
		if isptr {
			dest = dest.Elem()
		}

		if list != nil && len(list) > 0 {
			// check for multiple rows
			if len(list) > 1 {
				return fmt.Errorf("gorp: multiple rows returned for: %s - %v", query, args)
			}

			// Initialize if nil
			if dest.IsNil() {
				dest.Set(reflect.New(t))
			}

			// only one row found
			src := reflect.ValueOf(list[0])
			dest.Elem().Set(src.Elem())
		} else {
			// No rows found, return a proper error.
			return sql.ErrNoRows
		}

		return nil
	}

	return selectVal(ctx, e, holder, query, args...)
}

func selectVal(ctx context.Context, e SqlExecutor, holder interface{}, query string, args ...interface{}) error {
	if len(args) == 1 {
		switch m := e.(type) {
		case *DbMap:
			query, args = maybeExpandNamedQuery(m, query, args)
		case *Transaction:
			query, args = maybeExpandNamedQuery(m.dbmap, query, args)
		}
	}
	rows, err := e.query(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	if !rows.Next() {
		return sql.ErrNoRows
	}

	return rows.Scan(holder)
}

///////////////

func hookedselect(ctx context.Context, m *DbMap, exec SqlExecutor, i interface{}, query string,
	args ...interface{}) ([]interface{}, error) {

	list, err := rawselect(ctx, m, exec, i, query, args...)
	if err != nil {
		return nil, err
	}

	// Determine where the results are: written to i, or returned in list
	if t, _ := toSliceType(i); t == nil {
		for _, v := range list {
			if v, ok := v.(HasPostGet); ok {
				err := v.PostGet(ctx, exec)
				if err != nil {
					return nil, err
				}
			}
		}
	} else {
		resultsValue := reflect.Indirect(reflect.ValueOf(i))
		for i := 0; i < resultsValue.Len(); i++ {
			rv := resultsValue.Index(i)
			if rv.CanAddr() {
				rv = rv.Addr()
			}
			if v, ok := rv.Interface().(HasPostGet); ok {
				err := v.PostGet(ctx, exec)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	return list, nil
}

func rawselect(ctx context.Context, m *DbMap, exec SqlExecutor, i interface{}, query string,
	args ...interface{}) ([]interface{}, error) {
	var (
		appendToSlice   = false // Write results to i directly?
		intoStruct      = true  // Selecting into a struct?
		pointerElements = true  // Are the slice elements pointers (vs values)?
	)

	// get type for i, verifying it's a supported destination
	t, err := toType(i)
	if err != nil {
		var err2 error
		if t, err2 = toSliceType(i); t == nil {
			if err2 != nil {
				return nil, err2
			}
			return nil, err
		}
		pointerElements = t.Kind() == reflect.Ptr
		if pointerElements {
			t = t.Elem()
		}
		appendToSlice = true
		intoStruct = t.Kind() == reflect.Struct
	}

	// If the caller supplied a single struct/map argument, assume a "named
	// parameter" query.  Extract the named arguments from the struct/map, create
	// the flat arg slice, and rewrite the query to use the dialect's placeholder.
	if len(args) == 1 {
		query, args = maybeExpandNamedQuery(m, query, args)
	}

	// Run the query
	rows, err := exec.query(ctx, query, args...)
	if err != nil {
		return nil, errors.Wrap(err)
	}
	defer rows.Close()

	// Fetch the column names as returned from db
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	if !intoStruct && len(cols) > 1 {
		return nil, fmt.Errorf("gorp: select into non-struct slice requires 1 column, got %d", len(cols))
	}

	var colToFieldIndex [][]int
	if intoStruct {
		if colToFieldIndex, err = columnToFieldIndex(ctx, m, t, cols); err != nil {
			return nil, err
		}
	}

	conv := m.TypeConverter

	// Add results to one of these two slices.
	var (
		list       = make([]interface{}, 0)
		sliceValue = reflect.Indirect(reflect.ValueOf(i))
	)

	for {
		if !rows.Next() {
			// if error occured return rawselect
			if rows.Err() != nil {
				return nil, rows.Err()
			}
			// time to exit from outer "for" loop
			break
		}
		v := reflect.New(t)
		dest := make([]interface{}, len(cols))

		custScan := make([]CustomScanner, 0)

		for x := range cols {
			f := v.Elem()
			if intoStruct {
				index := colToFieldIndex[x]
				if index == nil {
					// this field is not present in the struct, so create a dummy
					// value for rows.Scan to scan into
					var dummy sql.RawBytes
					dest[x] = &dummy
					continue
				}
				f = f.FieldByIndex(index)
			}
			target := f.Addr().Interface()
			if conv != nil {
				scanner, ok := conv.FromDb(target)
				if ok {
					target = scanner.Holder
					custScan = append(custScan, scanner)
				}
			}
			dest[x] = target
		}

		err = rows.Scan(dest...)
		if err != nil {
			return nil, err
		}

		for _, c := range custScan {
			err = c.Bind()
			if err != nil {
				return nil, err
			}
		}

		if appendToSlice {
			if !pointerElements {
				v = v.Elem()
			}
			sliceValue.Set(reflect.Append(sliceValue, v))
		} else {
			list = append(list, v.Interface())
		}
	}

	if appendToSlice && sliceValue.IsNil() {
		sliceValue.Set(reflect.MakeSlice(sliceValue.Type(), 0, 0))
	}

	return list, nil
}

// Calls the Exec function on the executor, but attempts to expand any eligible named
// query arguments first.
func exec(ctx context.Context, e SqlExecutor, query string, args ...interface{}) (sql.Result, error) {
	var dbMap *DbMap
	var executor executor
	switch m := e.(type) {
	case *DbMap:
		executor = executorAdapter{m.Db}
		dbMap = m
	case *Transaction:
		executor = executorAdapter{m.tx}
		dbMap = m.dbmap
	}

	if len(args) == 1 {
		query, args = maybeExpandNamedQuery(dbMap, query, args)
	}

	return executor.Exec(ctx, query, args...)
}

// maybeExpandNamedQuery checks the given arg to see if it's eligible to be used
// as input to a named query.  If so, it rewrites the query to use
// dialect-dependent bindvars and instantiates the corresponding slice of
// parameters by extracting data from the map / struct.
// If not, returns the input values unchanged.
func maybeExpandNamedQuery(m *DbMap, query string, args []interface{}) (string, []interface{}) {
	var (
		arg    = args[0]
		argval = reflect.ValueOf(arg)
	)
	if argval.Kind() == reflect.Ptr {
		argval = argval.Elem()
	}

	if argval.Kind() == reflect.Map && argval.Type().Key().Kind() == reflect.String {
		return expandNamedQuery(m, query, func(key string) reflect.Value {
			return argval.MapIndex(reflect.ValueOf(key))
		})
	}
	if argval.Kind() != reflect.Struct {
		return query, args
	}
	if _, ok := arg.(time.Time); ok {
		// time.Time is driver.Value
		return query, args
	}
	if _, ok := arg.(driver.Valuer); ok {
		// driver.Valuer will be converted to driver.Value.
		return query, args
	}

	return expandNamedQuery(m, query, argval.FieldByName)
}

var keyRegexp = regexp.MustCompile(`:[[:word:]]+`)

// expandNamedQuery accepts a query with placeholders of the form ":key", and a
// single arg of Kind Struct or Map[string].  It returns the query with the
// dialect's placeholders, and a slice of args ready for positional insertion
// into the query.
func expandNamedQuery(m *DbMap, query string, keyGetter func(key string) reflect.Value) (string, []interface{}) {
	var (
		n    int
		args []interface{}
	)
	return keyRegexp.ReplaceAllStringFunc(query, func(key string) string {
		val := keyGetter(key[1:])
		if !val.IsValid() {
			return key
		}
		args = append(args, val.Interface())
		newVar := m.Dialect.BindVar(n)
		n++
		return newVar
	}), args
}

func columnToFieldIndex(ctx context.Context, m *DbMap, t reflect.Type, cols []string) ([][]int, error) {
	colToFieldIndex := make([][]int, len(cols))

	// check if type t is a mapped table - if so we'll
	// check the table for column aliasing below
	tableMapped := false
	table := tableOrNil(m, t)
	if table != nil {
		tableMapped = true
	}

	// Loop over column names and find field in i to bind to
	// based on column name. all returned columns must match
	// a field in the i struct
	missingColNames := []string{}
	for x := range cols {
		colName := strings.ToLower(cols[x])
		field, found := t.FieldByNameFunc(func(fieldName string) bool {
			field, _ := t.FieldByName(fieldName)
			fieldName = field.Tag.Get("db")

			if fieldName == "-" {
				return false
			} else if fieldName == "" {
				fieldName = field.Name
			}
			if tableMapped {
				colMap := colMapOrNil(table, fieldName)
				if colMap != nil {
					fieldName = colMap.ColumnName
				}
			}
			return colName == strings.ToLower(fieldName)
		})
		if found {
			colToFieldIndex[x] = field.Index
		}
		if colToFieldIndex[x] == nil {
			missingColNames = append(missingColNames, colName)
		}
	}
	if len(missingColNames) > 0 && m.logger != nil {
		m.logger.Warn(ctx, "The database returned columns that don't exist in the model type", nil, log.Fields{
			"model":          t.Name(),
			"missingColumns": missingColNames,
		})
	}
	return colToFieldIndex, nil
}

func fieldByName(val reflect.Value, fieldName string) *reflect.Value {
	// try to find field by exact match
	f := val.FieldByName(fieldName)

	if f != zeroVal {
		return &f
	}

	// try to find by case insensitive match - only the Postgres driver
	// seems to require this - in the case where columns are aliased in the sql
	fieldNameL := strings.ToLower(fieldName)
	fieldCount := val.NumField()
	t := val.Type()
	for i := 0; i < fieldCount; i++ {
		sf := t.Field(i)
		if strings.ToLower(sf.Name) == fieldNameL {
			f := val.Field(i)
			return &f
		}
	}

	return nil
}

// toSliceType returns the element type of the given object, if the object is a
// "*[]*Element" or "*[]Element". If not, returns nil.
// err is returned if the user was trying to pass a pointer-to-slice but failed.
func toSliceType(i interface{}) (reflect.Type, error) {
	t := reflect.TypeOf(i)
	if t.Kind() != reflect.Ptr {
		// If it's a slice, return a more helpful error message
		if t.Kind() == reflect.Slice {
			return nil, fmt.Errorf("gorp: Cannot SELECT into a non-pointer slice: %v", t)
		}
		return nil, nil
	}
	if t = t.Elem(); t.Kind() != reflect.Slice {
		return nil, nil
	}
	return t.Elem(), nil
}

func toType(i interface{}) (reflect.Type, error) {
	t := reflect.TypeOf(i)

	// If a Pointer to a type, follow
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("gorp: Cannot SELECT into this type: %v", reflect.TypeOf(i))
	}
	return t, nil
}

func get(ctx context.Context, m *DbMap, exec SqlExecutor, i interface{},
	keys ...interface{}) (interface{}, error) {

	t, err := toType(i)
	if err != nil {
		return nil, err
	}

	table, err := m.TableFor(t, true)
	if err != nil {
		return nil, err
	}

	plan := table.bindGet()

	v := reflect.New(t)
	dest := make([]interface{}, len(plan.argFields))

	conv := m.TypeConverter
	custScan := make([]CustomScanner, 0)

	for x, fieldName := range plan.argFields {
		f := v.Elem().FieldByName(fieldName)
		target := f.Addr().Interface()
		if conv != nil {
			scanner, ok := conv.FromDb(target)
			if ok {
				target = scanner.Holder
				custScan = append(custScan, scanner)
			}
		}
		dest[x] = target
	}

	row := exec.queryRow(ctx, plan.query, keys...)
	err = row.Scan(dest...)
	if err != nil {
		if err == sql.ErrNoRows {
			err = nil
		} else {
			err = errors.Wrap(err)
		}
		return nil, err
	}

	for _, c := range custScan {
		err = c.Bind()
		if err != nil {
			return nil, err
		}
	}

	if v, ok := v.Interface().(HasPostGet); ok {
		err := v.PostGet(ctx, exec)
		if err != nil {
			return nil, err
		}
	}

	return v.Interface(), nil
}

func delete(ctx context.Context, m *DbMap, exec SqlExecutor, list ...interface{}) (int64, error) {
	count := int64(0)
	for _, ptr := range list {
		table, elem, err := m.tableForPointer(ptr, true)
		if err != nil {
			return -1, err
		}

		eval := elem.Addr().Interface()
		if v, ok := eval.(HasPreDelete); ok {
			err = v.PreDelete(ctx, exec)
			if err != nil {
				return -1, err
			}
		}

		bi, err := table.bindDelete(elem)
		if err != nil {
			return -1, err
		}

		res, err := exec.Exec(ctx, bi.query, bi.args...)
		if err != nil {
			return -1, errors.Wrap(err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return -1, err
		}

		if rows == 0 && bi.existingVersion > 0 {
			return lockError(ctx, m, exec, table.TableName,
				bi.existingVersion, elem, bi.keys...)
		}

		count += rows

		if v, ok := eval.(HasPostDelete); ok {
			err := v.PostDelete(ctx, exec)
			if err != nil {
				return -1, err
			}
		}
	}

	return count, nil
}

func update(ctx context.Context, m *DbMap, exec SqlExecutor, list ...interface{}) (int64, error) {
	count := int64(0)
	for _, ptr := range list {
		table, elem, err := m.tableForPointer(ptr, true)
		if err != nil {
			return -1, err
		}

		eval := elem.Addr().Interface()
		if v, ok := eval.(HasPreUpdate); ok {
			err = v.PreUpdate(ctx, exec)
			if err != nil {
				return -1, err
			}
		}

		bi, err := table.bindUpdate(elem)
		if err != nil {
			return -1, err
		}

		res, err := exec.Exec(ctx, bi.query, bi.args...)
		if err != nil {
			return -1, errors.Wrap(err)
		}

		rows, err := res.RowsAffected()
		if err != nil {
			return -1, err
		}

		if rows == 0 && bi.existingVersion > 0 {
			return lockError(ctx, m, exec, table.TableName,
				bi.existingVersion, elem, bi.keys...)
		}

		if bi.versField != "" {
			elem.FieldByName(bi.versField).SetInt(bi.existingVersion + 1)
		}

		count += rows

		if v, ok := eval.(HasPostUpdate); ok {
			err = v.PostUpdate(ctx, exec)
			if err != nil {
				return -1, err
			}
		}
	}
	return count, nil
}

func insert(ctx context.Context, m *DbMap, exec SqlExecutor, list ...interface{}) error {
	for _, ptr := range list {
		table, elem, err := m.tableForPointer(ptr, false)
		if err != nil {
			return err
		}

		eval := elem.Addr().Interface()
		if v, ok := eval.(HasPreInsert); ok {
			err := v.PreInsert(ctx, exec)
			if err != nil {
				return err
			}
		}

		bi, err := table.bindInsert(elem)
		if err != nil {
			return err
		}

		if bi.autoIncrIdx > -1 {
			f := elem.FieldByName(bi.autoIncrFieldName)
			switch inserter := m.Dialect.(type) {
			case IntegerAutoIncrInserter:
				id, err := inserter.InsertAutoIncr(ctx, exec, bi.query, bi.args...)
				if err != nil {
					return err
				}
				k := f.Kind()
				if (k == reflect.Int) || (k == reflect.Int16) || (k == reflect.Int32) || (k == reflect.Int64) {
					f.SetInt(id)
				} else if (k == reflect.Uint) || (k == reflect.Uint16) || (k == reflect.Uint32) || (k == reflect.Uint64) {
					f.SetUint(uint64(id))
				} else {
					return fmt.Errorf("gorp: Cannot set autoincrement value on non-Int field. SQL=%s  autoIncrIdx=%d autoIncrFieldName=%s", bi.query, bi.autoIncrIdx, bi.autoIncrFieldName)
				}
			case TargetedAutoIncrInserter:
				err := inserter.InsertAutoIncrToTarget(ctx, exec, bi.query, f.Addr().Interface(), bi.args...)
				if err != nil {
					return err
				}
			default:
				return fmt.Errorf("gorp: Cannot use autoincrement fields on dialects that do not implement an autoincrementing interface")
			}
		} else {
			_, err := exec.Exec(ctx, bi.query, bi.args...)
			if err != nil {
				return errors.Wrap(err)
			}
		}

		if v, ok := eval.(HasPostInsert); ok {
			err := v.PostInsert(ctx, exec)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func lockError(ctx context.Context, m *DbMap, exec SqlExecutor, tableName string,
	existingVer int64, elem reflect.Value,
	keys ...interface{}) (int64, error) {

	existing, err := get(ctx, m, exec, elem.Interface(), keys...)
	if err != nil {
		return -1, err
	}

	return -1, errors.Instance(ErrorConcurrentModification).Attach(errors.Fields{
		"tableName":       tableName,
		"keys":            keys,
		"rowExists":       existing != nil,
		"existingVersion": existingVer,
	})
}

func postCommitInsert(ctx context.Context, m *DbMap, list ...interface{}) error {
	for _, ptr := range list {
		_, elem, err := m.tableForPointer(ptr, false)
		if err != nil {
			return err
		}

		eval := elem.Addr().Interface()
		if v, ok := eval.(HasPostCommitInsert); ok {
			err = v.PostCommitInsert(ctx, m)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func postCommitUpdate(ctx context.Context, m *DbMap, list ...interface{}) error {
	for _, ptr := range list {
		_, elem, err := m.tableForPointer(ptr, false)
		if err != nil {
			return err
		}

		eval := elem.Addr().Interface()
		if v, ok := eval.(HasPostCommitUpdate); ok {
			err = v.PostCommitUpdate(ctx, m)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func postCommitDelete(ctx context.Context, m *DbMap, list ...interface{}) error {
	for _, ptr := range list {
		_, elem, err := m.tableForPointer(ptr, false)
		if err != nil {
			return err
		}

		eval := elem.Addr().Interface()
		if v, ok := eval.(HasPostCommitDelete); ok {
			err = v.PostCommitDelete(ctx, m)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// PostUpdate() will be executed after the GET statement.
type HasPostGet interface {
	PostGet(context.Context, SqlExecutor) error
}

// PostUpdate() will be executed after the DELETE statement
type HasPostDelete interface {
	PostDelete(context.Context, SqlExecutor) error
}

// PostCommitDelete() will be executed after the DELETE statement has been committed.
type HasPostCommitDelete interface {
	PostCommitDelete(context.Context, SqlExecutor) error
}

// PostUpdate() will be executed after the UPDATE statement
type HasPostUpdate interface {
	PostUpdate(context.Context, SqlExecutor) error
}

// PostCommitUpdate() will be executed after the UPDATE statement has been committed.
type HasPostCommitUpdate interface {
	PostCommitUpdate(context.Context, SqlExecutor) error
}

// PostInsert() will be executed after the INSERT statement
type HasPostInsert interface {
	PostInsert(context.Context, SqlExecutor) error
}

// PostCommitInsert() will be executed after the INSERT statement has been committed.
type HasPostCommitInsert interface {
	PostCommitInsert(context.Context, SqlExecutor) error
}

// PreDelete() will be executed before the DELETE statement.
type HasPreDelete interface {
	PreDelete(context.Context, SqlExecutor) error
}

// PreUpdate() will be executed before UPDATE statement.
type HasPreUpdate interface {
	PreUpdate(context.Context, SqlExecutor) error
}

// PreInsert() will be executed before INSERT statement.
type HasPreInsert interface {
	PreInsert(context.Context, SqlExecutor) error
}
